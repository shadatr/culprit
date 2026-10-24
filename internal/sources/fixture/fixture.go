// Package fixture loads offline scenario directories (PLAN.md §5) and serves
// them through the core source interfaces.
package fixture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shadatr/culprit/internal/core"
)

// Default incident window when scenario.yaml omits lookback/after.
const (
	defaultLookback = 2 * time.Hour
	defaultAfter    = 30 * time.Minute
	defaultLogLimit = 1000
)

// Truth is the known root cause of a scenario.
type Truth struct {
	Service  string   `yaml:"service" json:"service"`
	ChangeID string   `yaml:"change_id" json:"change_id,omitempty"`
	Category string   `yaml:"category" json:"category"`
	Keywords []string `yaml:"keywords" json:"keywords"`
}

// Scenario is a loaded scenario.
type Scenario struct {
	Name        string
	Description string
	Dir         string
	Incident    core.Incident // Window computed from lookback/after; Source = "eval"
	Topology    core.Topology
	Truth       Truth

	series  []core.Series  // as loaded
	logs    []core.LogLine // sorted oldest first
	changes []core.Change  // sorted newest first
}

// scenarioFile is the on-disk shape of scenario.yaml.
type scenarioFile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Incident    struct {
		Title     string            `yaml:"title"`
		Service   string            `yaml:"service"`
		StartedAt string            `yaml:"started_at"`
		Lookback  string            `yaml:"lookback"`
		After     string            `yaml:"after"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"incident"`
	Topology core.Topology `yaml:"topology"`
	Truth    Truth         `yaml:"truth"`
}

// metricFile is one entry of metrics.json.
type metricFile struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"` // [unix_seconds, value]
}

// Load reads dir. metrics.json, logs.jsonl and changes.json are optional.
func Load(dir string) (*Scenario, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		return nil, fmt.Errorf("fixture: read scenario: %w", err)
	}
	var f scenarioFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("fixture: parse %s/scenario.yaml: %w", dir, err)
	}
	if f.Name == "" {
		f.Name = filepath.Base(filepath.Clean(dir))
	}
	if f.Incident.StartedAt == "" {
		return nil, fmt.Errorf("fixture: %s: incident.started_at is required", f.Name)
	}
	start, err := time.Parse(time.RFC3339, f.Incident.StartedAt)
	if err != nil {
		return nil, fmt.Errorf("fixture: %s: incident.started_at: %w", f.Name, err)
	}
	if f.Truth.Service == "" {
		return nil, fmt.Errorf("fixture: %s: truth.service is required", f.Name)
	}
	lookback, err := parseDuration(f.Incident.Lookback, defaultLookback)
	if err != nil {
		return nil, fmt.Errorf("fixture: %s: incident.lookback: %w", f.Name, err)
	}
	after, err := parseDuration(f.Incident.After, defaultAfter)
	if err != nil {
		return nil, fmt.Errorf("fixture: %s: incident.after: %w", f.Name, err)
	}

	inc := core.NewIncident(f.Incident.Title, f.Incident.Service, start.UTC(), lookback, after)
	inc.ID = f.Name
	inc.Source = "eval"
	inc.Labels = f.Incident.Labels
	sc := &Scenario{
		Name:        f.Name,
		Description: f.Description,
		Dir:         dir,
		Incident:    inc,
		Topology:    f.Topology,
		Truth:       f.Truth,
	}
	if sc.Topology == nil {
		sc.Topology = core.Topology{}
	}

	if sc.series, err = loadMetrics(filepath.Join(dir, "metrics.json")); err != nil {
		return nil, fmt.Errorf("fixture: %s: %w", f.Name, err)
	}
	if sc.logs, err = loadLogs(filepath.Join(dir, "logs.jsonl")); err != nil {
		return nil, fmt.Errorf("fixture: %s: %w", f.Name, err)
	}
	if sc.changes, err = loadChanges(filepath.Join(dir, "changes.json")); err != nil {
		return nil, fmt.Errorf("fixture: %s: %w", f.Name, err)
	}
	return sc, nil
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

// readOptional returns nil, nil when path does not exist.
func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return b, nil
}

func loadMetrics(path string) ([]core.Series, error) {
	b, err := readOptional(path)
	if err != nil || b == nil {
		return nil, err
	}
	var raw []metricFile
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse metrics.json: %w", err)
	}
	out := make([]core.Series, 0, len(raw))
	for i, m := range raw {
		if m.Name == "" {
			return nil, fmt.Errorf("metrics.json: series %d has no name", i)
		}
		s := core.Series{Name: m.Name, Labels: m.Labels, Points: make([]core.Point, 0, len(m.Points))}
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		for _, p := range m.Points {
			sec, frac := math.Modf(p[0])
			t := time.Unix(int64(sec), int64(frac*1e9)).UTC()
			s.Points = append(s.Points, core.Point{T: t, V: p[1]})
		}
		sort.SliceStable(s.Points, func(a, b int) bool { return s.Points[a].T.Before(s.Points[b].T) })
		out = append(out, s)
	}
	return out, nil
}

func loadLogs(path string) ([]core.LogLine, error) {
	b, err := readOptional(path)
	if err != nil || b == nil {
		return nil, err
	}
	var out []core.LogLine
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var l core.LogLine
		if err := json.Unmarshal(line, &l); err != nil {
			return nil, fmt.Errorf("logs.jsonl line %d: %w", n, err)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read logs.jsonl: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

func loadChanges(path string) ([]core.Change, error) {
	b, err := readOptional(path)
	if err != nil || b == nil {
		return nil, err
	}
	var out []core.Change
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse changes.json: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.After(out[j].T) })
	return out, nil
}

// inWindow treats a zero window as unbounded.
func inWindow(w core.Window, t time.Time) bool {
	if w.Start.IsZero() && w.End.IsZero() {
		return true
	}
	if !w.Start.IsZero() && t.Before(w.Start) {
		return false
	}
	if !w.End.IsZero() && t.After(w.End) {
		return false
	}
	return true
}

// Metrics returns a MetricsSource over the scenario's series. Query supports
// selectors with =, !=, =~ and !~ matchers; points outside the window are
// dropped; step is ignored.
func (s *Scenario) Metrics() core.MetricsSource { return &metrics{series: s.series} }

// Logs returns a LogsSource over the scenario's lines.
func (s *Scenario) Logs() core.LogsSource { return &logs{lines: s.logs} }

// Changes returns a ChangesSource over the scenario's changes.
func (s *Scenario) Changes() core.ChangesSource { return &changes{changes: s.changes} }

type metrics struct{ series []core.Series }

func (m *metrics) Name() string { return "fixture" }

// ListMetrics returns distinct metric names containing match
// (case-insensitive), sorted.
func (m *metrics) ListMetrics(ctx context.Context, match string) ([]string, error) {
	match = strings.ToLower(match)
	seen := map[string]bool{}
	var out []string
	for _, s := range m.series {
		if seen[s.Name] || !strings.Contains(strings.ToLower(s.Name), match) {
			continue
		}
		seen[s.Name] = true
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out, nil
}

// Query returns copies of the matching series restricted to w. Series with
// no points in w are omitted, as Prometheus does.
func (m *metrics) Query(ctx context.Context, selector string, w core.Window, step time.Duration) ([]core.Series, error) {
	sel, err := ParseSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("fixture: %w", err)
	}
	var out []core.Series
	for _, s := range m.series {
		if !sel.Matches(s.Name, s.Labels) {
			continue
		}
		var pts []core.Point
		for _, p := range s.Points {
			if inWindow(w, p.T) {
				pts = append(pts, p)
			}
		}
		if len(pts) == 0 {
			continue
		}
		labels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			labels[k] = v
		}
		out = append(out, core.Series{Name: s.Name, Labels: labels, Points: pts})
	}
	return out, nil
}

type logs struct{ lines []core.LogLine }

func (l *logs) Name() string { return "fixture" }

// Services lists services with at least one line in w, sorted.
func (l *logs) Services(ctx context.Context, w core.Window) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, ln := range l.lines {
		if ln.Service == "" || seen[ln.Service] || !inWindow(w, ln.T) {
			continue
		}
		seen[ln.Service] = true
		out = append(out, ln.Service)
	}
	sort.Strings(out)
	return out, nil
}

// Query returns matching lines, oldest first. Service matches exactly;
// Contains is a case-insensitive substring; Level is case-insensitive.
func (l *logs) Query(ctx context.Context, q core.LogQuery) ([]core.LogLine, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	contains := strings.ToLower(q.Contains)
	var out []core.LogLine
	for _, ln := range l.lines {
		if !inWindow(q.Window, ln.T) {
			continue
		}
		if q.Service != "" && ln.Service != q.Service {
			continue
		}
		if q.Level != "" && !strings.EqualFold(ln.Level, q.Level) {
			continue
		}
		if contains != "" && !strings.Contains(strings.ToLower(ln.Message), contains) {
			continue
		}
		out = append(out, ln)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type changes struct{ changes []core.Change }

func (c *changes) Name() string { return "fixture" }

// Changes returns changes in w, newest first.
func (c *changes) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	var out []core.Change
	for _, ch := range c.changes {
		if inWindow(w, ch.T) {
			out = append(out, ch)
		}
	}
	return out, nil
}
