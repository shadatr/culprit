// Package tools exposes data sources and analysis to the LLM as tools and
// records every result in an evidence ledger.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/correlate"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/llm"
)

// Tool names.
const (
	ToolListMetrics  = "list_metrics"
	ToolGetMetric    = "get_metric"
	ToolSearchLogs   = "search_logs"
	ToolLogPatterns  = "log_patterns"
	ToolListChanges  = "list_changes"
	ToolGetChange    = "get_change"
	ToolGetTopology  = "get_topology"
	ToolSubmitReport = "submit_report"
)

// MaxResultChars caps the text of one tool result sent to the model.
const MaxResultChars = 3000

const (
	maxLogLines    = 5000
	defaultLogLim  = 1000
	maxSeriesShown = 15
	maxDiffChars   = 1500
)

// ErrSubmitReport is returned by Dispatch for submit_report, which the
// investigator handles itself.
var ErrSubmitReport = errors.New("tools: submit_report is handled by the investigator")

// Deps are the sources the tools read. Any source may be nil; Ledger is
// required.
type Deps struct {
	Metrics  core.MetricsSource
	Logs     core.LogsSource
	Changes  core.ChangesSource
	Topology core.Topology
	Ledger   *Ledger
}

// Toolbox runs tools for one investigation. It is safe for concurrent use
// as long as its sources are.
type Toolbox struct {
	d   Deps
	inc core.Incident
	tr  *TriageResult
}

// New returns a Toolbox for inc. tr (may be nil) supplies triage anomalies
// and log shifts used to score changes. A nil d.Ledger is replaced by a new
// one.
func New(d Deps, inc core.Incident, tr *TriageResult) *Toolbox {
	if d.Ledger == nil {
		d.Ledger = NewLedger()
	}
	if tr == nil {
		tr = &TriageResult{}
	}
	return &Toolbox{d: d, inc: inc, tr: tr}
}

// Ledger returns the evidence ledger the toolbox records into.
func (t *Toolbox) Ledger() *Ledger { return t.d.Ledger }

// Definitions returns the tool definitions offered to the model, including
// submit_report.
func Definitions() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        ToolListMetrics,
			Description: "List available metric names that contain a substring. Use it to discover what to query with get_metric. Metric names follow recording-rule style, e.g. http_request_duration_seconds:p99, http_errors:ratio1m, process_cpu:ratio, db_pool_in_use:ratio; every series has a service label.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"match":{"type":"string","description":"Substring to match in metric names; empty lists all."}},
"additionalProperties":false}`),
		},
		{
			Name:        ToolGetMetric,
			Description: "Fetch series for a Prometheus selector (NOT PromQL: no functions or operators) and return per-series statistics before vs after the incident start, detected anomaly and onset, and a coarse trend. Records evidence.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"selector":{"type":"string","description":"Series selector such as db_pool_in_use:ratio{service=\"catalog\"} or http_errors:ratio1m{service=~\"cart|checkout\"}."},
"start":{"type":"string","format":"date-time","description":"RFC3339 start; defaults to the incident window start."},
"end":{"type":"string","format":"date-time","description":"RFC3339 end; defaults to the incident window end."}},
"required":["selector"],"additionalProperties":false}`),
		},
		{
			Name:        ToolSearchLogs,
			Description: "Search one service's logs and return lines clustered into templates with counts, levels, first/last seen, plus up to 5 sample lines. Records evidence.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"service":{"type":"string","description":"Service name."},
"contains":{"type":"string","description":"Case-sensitive substring the message must contain."},
"level":{"type":"string","description":"Log level filter, e.g. error, warn, info."},
"start":{"type":"string","format":"date-time","description":"RFC3339 start; defaults to the incident window start."},
"end":{"type":"string","format":"date-time","description":"RFC3339 end; defaults to the incident window end."},
"limit":{"type":"integer","minimum":1,"maximum":5000,"description":"Maximum lines to scan (default 1000)."}},
"required":["service"],"additionalProperties":false}`),
		},
		{
			Name:        ToolLogPatterns,
			Description: "Compare a service's log templates in the baseline (before incident start) with the incident period and return new or spiking templates with rates. Records evidence.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"service":{"type":"string","description":"Service name."}},
"required":["service"],"additionalProperties":false}`),
		},
		{
			Name:        ToolListChanges,
			Description: "List deploys, commits, config and flag changes in the incident window with a correlation score (timing vs anomaly onsets, service overlap, topology). Records evidence.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"service":{"type":"string","description":"Only changes to this service; empty lists all."}},
"additionalProperties":false}`),
		},
		{
			Name:        ToolGetChange,
			Description: "Get one change by ID with author, time, summary and a truncated diff. Records evidence.",
			Parameters: json.RawMessage(`{"type":"object","properties":{
"id":{"type":"string","description":"Change ID as shown by list_changes."}},
"required":["id"],"additionalProperties":false}`),
		},
		{
			Name:        ToolGetTopology,
			Description: "Return the service dependency map (service -> services it calls). A fault in a dependency usually surfaces as symptoms in its callers.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        ToolSubmitReport,
			Description: "Submit the final root-cause report. Every hypothesis must cite existing evidence IDs (E1, E2, ...). Hypotheses are ranked by confidence, highest first; the timeline is sorted by time. Returns validation errors to fix if the report is rejected.",
			Parameters:  json.RawMessage(submitReportSchema),
		},
	}
}

const submitReportSchema = `{"type":"object","properties":{
"summary":{"type":"string","description":"Two to four sentences: what happened, the most likely root cause, and impact."},
"timeline":{"type":"array","description":"Key moments, sorted by time ascending.","items":{"type":"object","properties":{
  "t":{"type":"string","format":"date-time","description":"RFC3339 time."},
  "text":{"type":"string"},
  "evidence_ids":{"type":"array","items":{"type":"string"}}},
  "required":["t","text"],"additionalProperties":false}},
"hypotheses":{"type":"array","minItems":1,"description":"Candidate root causes ranked by confidence, most likely first.","items":{"type":"object","properties":{
  "title":{"type":"string","description":"Short root-cause statement."},
  "explanation":{"type":"string","description":"Causal chain from cause to symptom, referencing evidence."},
  "service":{"type":"string","description":"Service where the root cause lives."},
  "category":{"type":"string","enum":["bad-deploy","config-change","resource-exhaustion","dependency-failure","traffic-spike","certificate-expiry","other"]},
  "change_id":{"type":"string","description":"ID of the responsible change, if any."},
  "confidence":{"type":"number","minimum":0,"maximum":1},
  "evidence_ids":{"type":"array","minItems":1,"items":{"type":"string"},"description":"Evidence IDs supporting this hypothesis."},
  "next_steps":{"type":"array","items":{"type":"string"},"description":"Concrete read-only checks or remediations for a human."}},
  "required":["title","explanation","confidence","evidence_ids"],"additionalProperties":false}}},
"required":["summary","hypotheses"],"additionalProperties":false}`

// Dispatch runs tool name with JSON args. It returns the evidence ID (empty
// if nothing was recorded) and a compact summary for the model. A non-nil
// error is a tool failure meant to be shown to the model, not a fatal one.
func (t *Toolbox) Dispatch(ctx context.Context, name string, args json.RawMessage) (string, string, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var (
		id, text string
		err      error
	)
	switch name {
	case ToolListMetrics:
		id, text, err = t.listMetrics(ctx, args)
	case ToolGetMetric:
		id, text, err = t.getMetric(ctx, args)
	case ToolSearchLogs:
		id, text, err = t.searchLogs(ctx, args)
	case ToolLogPatterns:
		id, text, err = t.logPatterns(ctx, args)
	case ToolListChanges:
		id, text, err = t.listChanges(ctx, args)
	case ToolGetChange:
		id, text, err = t.getChange(ctx, args)
	case ToolGetTopology:
		id, text, err = t.getTopology()
	case ToolSubmitReport:
		return "", "", ErrSubmitReport
	default:
		return "", "", fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", name, err)
	}
	return id, capText(text, MaxResultChars), nil
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments %s: %w", clip(string(args), 200), err)
	}
	return nil
}

func notConfigured(kind string) string {
	return fmt.Sprintf("No %s source is configured for this investigation, so this tool has no data. Rely on other evidence.", kind)
}

// window parses optional RFC3339 bounds, defaulting to the incident window.
func (t *Toolbox) window(start, end string) (core.Window, error) {
	w := t.inc.Window
	if start != "" {
		s, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return w, fmt.Errorf("start %q is not RFC3339: %w", start, err)
		}
		w.Start = s
	}
	if end != "" {
		e, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return w, fmt.Errorf("end %q is not RFC3339: %w", end, err)
		}
		w.End = e
	}
	if !w.End.After(w.Start) {
		return w, fmt.Errorf("end %s must be after start %s", w.End.Format(time.RFC3339), w.Start.Format(time.RFC3339))
	}
	return w, nil
}

// record adds evidence and returns its ID.
func (t *Toolbox) record(kind, tool string, input json.RawMessage, summary string, data any) string {
	return t.d.Ledger.Add(core.Evidence{
		Kind:    kind,
		Tool:    tool,
		Input:   input,
		Summary: capText(summary, 800),
		Data:    mustJSON(data),
	})
}

func (t *Toolbox) listMetrics(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		Match string `json:"match"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if t.d.Metrics == nil {
		return "", notConfigured("metrics"), nil
	}
	names, err := t.d.Metrics.ListMetrics(ctx, a.Match)
	if err != nil {
		return "", "", err
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Sprintf("No metrics match %q.", a.Match), nil
	}
	const show = 150
	var b strings.Builder
	fmt.Fprintf(&b, "%d metrics match %q:\n", len(names), a.Match)
	for i, n := range names {
		if i == show {
			fmt.Fprintf(&b, "... and %d more; narrow the match.\n", len(names)-show)
			break
		}
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return "", b.String(), nil
}

// queryStep picks a step giving roughly 240 points over w.
func queryStep(w core.Window) time.Duration {
	s := (w.End.Sub(w.Start) / 240).Round(time.Second)
	return max(s, 15*time.Second)
}

type seriesView struct {
	Stats anomaly.Stats `json:"stats"`
	Trend string        `json:"trend"`
}

func (t *Toolbox) getMetric(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		Selector string `json:"selector"`
		Start    string `json:"start"`
		End      string `json:"end"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(a.Selector) == "" {
		return "", "", errors.New("selector is required")
	}
	if t.d.Metrics == nil {
		return "", notConfigured("metrics"), nil
	}
	w, err := t.window(a.Start, a.End)
	if err != nil {
		return "", "", err
	}
	series, err := t.d.Metrics.Query(ctx, a.Selector, w, queryStep(w))
	if err != nil {
		return "", "", err
	}
	if len(series) == 0 {
		return "", fmt.Sprintf("No series match %s in %s..%s. Check the name with list_metrics.", a.Selector, w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339)), nil
	}
	views := make([]seriesView, 0, len(series))
	for _, s := range series {
		st := anomaly.Describe(s, t.inc.StartedAt)
		if st.BeforeMean == 0 && st.AfterMean == 0 && st.AfterMax == 0 && len(s.Points) > 0 {
			// Analysis returned no numbers; fill in plain means.
			st.BeforeMean, st.AfterMean, st.AfterMax = splitMeans(s.Points, t.inc.StartedAt)
			if st.BeforeMean != 0 {
				st.ChangePercent = (st.AfterMean - st.BeforeMean) / math.Abs(st.BeforeMean) * 100
			}
		}
		if st.Metric == "" {
			st.Metric = s.Name
		}
		views = append(views, seriesView{Stats: st, Trend: trend(s.Points, 12)})
	}
	slices.SortStableFunc(views, func(x, y seriesView) int {
		xs, ys := 0.0, 0.0
		if x.Stats.Anomaly != nil {
			xs = 1e6 + x.Stats.Anomaly.Score
		} else {
			xs = math.Abs(x.Stats.ChangePercent)
		}
		if y.Stats.Anomaly != nil {
			ys = 1e6 + y.Stats.Anomaly.Score
		} else {
			ys = math.Abs(y.Stats.ChangePercent)
		}
		switch {
		case xs > ys:
			return -1
		case xs < ys:
			return 1
		}
		return 0
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%d series for %s (incident start %s):\n", len(views), a.Selector, t.inc.StartedAt.UTC().Format(time.RFC3339))
	for i, v := range views {
		if i == maxSeriesShown {
			fmt.Fprintf(&b, "... %d more series omitted; narrow the selector.\n", len(views)-i)
			break
		}
		st := v.Stats
		fmt.Fprintf(&b, "- %s%s: before mean %s p95 %s | after mean %s max %s | change %s",
			st.Metric, fmtLabels(st.Labels), num(st.BeforeMean), num(st.BeforeP95), num(st.AfterMean), num(st.AfterMax), pct(st.ChangePercent))
		if an := st.Anomaly; an != nil {
			fmt.Fprintf(&b, " | ANOMALY %s score %s onset %s", an.Direction, num(an.Score), ts(an.Onset, t.inc.StartedAt))
		}
		fmt.Fprintf(&b, "\n  trend(12 buckets): %s\n", v.Trend)
	}
	text := b.String()
	views = views[:min(len(views), maxSeriesShown*2)]
	id := t.record("metric", ToolGetMetric, args, text, map[string]any{"selector": a.Selector, "window": w, "series": views})
	return id, text, nil
}

func (t *Toolbox) searchLogs(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		Service  string `json:"service"`
		Contains string `json:"contains"`
		Level    string `json:"level"`
		Start    string `json:"start"`
		End      string `json:"end"`
		Limit    int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if a.Service == "" {
		return "", "", errors.New("service is required")
	}
	if t.d.Logs == nil {
		return "", notConfigured("logs"), nil
	}
	w, err := t.window(a.Start, a.End)
	if err != nil {
		return "", "", err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultLogLim
	}
	limit = min(limit, maxLogLines)
	lines, err := t.d.Logs.Query(ctx, core.LogQuery{Service: a.Service, Contains: a.Contains, Level: a.Level, Window: w, Limit: limit})
	if err != nil {
		return "", "", err
	}
	if len(lines) == 0 {
		return "", fmt.Sprintf("No log lines for service %q matching contains=%q level=%q in %s..%s.", a.Service, a.Contains, a.Level, w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339)), nil
	}
	tpls := logpattern.Mine(lines)
	if len(tpls) == 0 {
		tpls = fallbackMine(lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d lines from %s", len(lines), a.Service)
	if len(lines) >= limit {
		b.WriteString(" (limit reached; results may be partial)")
	}
	fmt.Fprintf(&b, ", %d templates:\n", len(tpls))
	for i, tp := range tpls {
		if i == 12 {
			fmt.Fprintf(&b, "... %d more templates\n", len(tpls)-i)
			break
		}
		fmt.Fprintf(&b, "- x%d [%s] %s (first %s, last %s)\n", tp.Count, levelOr(tp.Level), clip(tp.Pattern, 160), ts(tp.First, t.inc.StartedAt), ts(tp.Last, t.inc.StartedAt))
	}
	b.WriteString("Samples:\n")
	for _, l := range sampleLines(lines, 5) {
		fmt.Fprintf(&b, "  %s [%s] %s\n", l.T.UTC().Format(time.RFC3339), levelOr(l.Level), clip(l.Message, 200))
	}
	text := b.String()
	if len(tpls) > 30 {
		tpls = tpls[:30]
	}
	id := t.record("logs", ToolSearchLogs, args, text, map[string]any{"service": a.Service, "window": w, "lines": len(lines), "templates": tpls})
	return id, text, nil
}

func levelOr(l string) string {
	if l == "" {
		return "-"
	}
	return l
}

// sampleLines picks up to n lines, preferring errors/warnings, then the
// most recent, returned oldest first.
func sampleLines(lines []core.LogLine, n int) []core.LogLine {
	rank := func(l core.LogLine) int {
		switch strings.ToLower(l.Level) {
		case "error", "err", "fatal", "critical", "panic":
			return 0
		case "warn", "warning":
			return 1
		}
		return 2
	}
	idx := make([]int, len(lines))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		ri, rj := rank(lines[idx[i]]), rank(lines[idx[j]])
		if ri != rj {
			return ri < rj
		}
		return lines[idx[i]].T.After(lines[idx[j]].T)
	})
	// First pass: one line per distinct shape; second pass: any distinct
	// message.
	var out []core.LogLine
	picked := map[int]bool{}
	for pass := range 2 {
		seen := map[string]bool{}
		for _, l := range out {
			seen[normalise(l.Message)] = true
		}
		for _, i := range idx {
			if len(out) == n {
				break
			}
			key := normalise(lines[i].Message)
			if pass == 1 {
				key = lines[i].Message
			}
			if picked[i] || seen[key] {
				continue
			}
			seen[key] = true
			picked[i] = true
			out = append(out, lines[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

func (t *Toolbox) logPatterns(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		Service string `json:"service"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if a.Service == "" {
		return "", "", errors.New("service is required")
	}
	if t.d.Logs == nil {
		return "", notConfigured("logs"), nil
	}
	lines, err := t.d.Logs.Query(ctx, core.LogQuery{Service: a.Service, Window: t.inc.Window, Limit: maxLogLines})
	if err != nil {
		return "", "", err
	}
	if len(lines) == 0 {
		return "", fmt.Sprintf("No log lines for service %q in the incident window.", a.Service), nil
	}
	shifts := logpattern.Diff(lines, t.inc.StartedAt)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d lines scanned; baseline = before %s.\n", a.Service, len(lines), t.inc.StartedAt.UTC().Format(time.RFC3339))
	if len(shifts) == 0 {
		b.WriteString("No new or spiking templates during the incident.\n")
	}
	for i, s := range shifts {
		if i == 10 {
			fmt.Fprintf(&b, "... %d more shifts\n", len(shifts)-i)
			break
		}
		b.WriteString("- " + shiftLine(s, t.inc.StartedAt) + "\n")
	}
	text := b.String()
	if len(shifts) > 20 {
		shifts = shifts[:20]
	}
	id := t.record("log_patterns", ToolLogPatterns, args, text, map[string]any{"service": a.Service, "lines": len(lines), "shifts": shifts})
	return id, text, nil
}

func shiftLine(s logpattern.Shift, start time.Time) string {
	tag := "NEW"
	if !s.New {
		if s.BaselineRate > 0 {
			tag = "x" + num(s.IncidentRate/s.BaselineRate)
		} else {
			tag = "UP"
		}
	}
	return fmt.Sprintf("%s [%s] %s/min -> %s/min first %s: %s", tag, levelOr(s.Template.Level), num(s.BaselineRate), num(s.IncidentRate), ts(s.FirstInIncident, start), clip(s.Template.Pattern, 140))
}

// rankChanges scores cs using triage signals, falling back to ordering by
// time when analysis returns nothing.
func (t *Toolbox) rankChanges(cs []core.Change) []correlate.ScoredChange {
	return rankChanges(cs, t.tr.Anomalies, t.tr.Shifts, t.d.Topology, t.inc.StartedAt)
}

func rankChanges(cs []core.Change, an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, start time.Time) []correlate.ScoredChange {
	if len(cs) == 0 {
		return nil
	}
	r := correlate.RankChanges(cs, an, sh, topo, start)
	if len(r) == 0 {
		r = fallbackRank(cs, start)
	}
	return r
}

func changeLine(sc correlate.ScoredChange, start time.Time) string {
	c := sc.Change
	svc := c.Service
	if svc == "" {
		svc = "-"
	}
	line := fmt.Sprintf("%s %s %s at %s score %.2f: %s", c.ID, c.Kind, svc, ts(c.T, start), sc.Score, clip(c.Summary, 140))
	if c.Author != "" {
		line += " (by " + c.Author + ")"
	}
	if len(sc.Reasons) > 0 {
		line += " [" + clip(strings.Join(sc.Reasons, "; "), 160) + "]"
	}
	return line
}

func (t *Toolbox) listChanges(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		Service string `json:"service"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if t.d.Changes == nil {
		return "", notConfigured("changes"), nil
	}
	cs, err := t.d.Changes.Changes(ctx, t.inc.Window)
	if err != nil {
		return "", "", err
	}
	if a.Service != "" {
		cs = slices.DeleteFunc(cs, func(c core.Change) bool { return !strings.EqualFold(c.Service, a.Service) })
	}
	if len(cs) == 0 {
		return "", fmt.Sprintf("No changes found in the incident window (service filter %q).", a.Service), nil
	}
	ranked := t.rankChanges(cs)
	var b strings.Builder
	fmt.Fprintf(&b, "%d changes in window, ranked by correlation:\n", len(ranked))
	for i, sc := range ranked {
		if i == 15 {
			fmt.Fprintf(&b, "... %d more\n", len(ranked)-i)
			break
		}
		b.WriteString("- " + changeLine(sc, t.inc.StartedAt) + "\n")
	}
	text := b.String()
	data := make([]correlate.ScoredChange, 0, min(len(ranked), 30))
	for _, sc := range ranked[:min(len(ranked), 30)] {
		sc.Change = truncateDiff(sc.Change, 500)
		data = append(data, sc)
	}
	id := t.record("changes", ToolListChanges, args, text, data)
	return id, text, nil
}

func (t *Toolbox) getChange(ctx context.Context, args json.RawMessage) (string, string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(args, &a); err != nil {
		return "", "", err
	}
	if a.ID == "" {
		return "", "", errors.New("id is required")
	}
	if t.d.Changes == nil {
		return "", notConfigured("changes"), nil
	}
	find := func(w core.Window) (*core.Change, error) {
		cs, err := t.d.Changes.Changes(ctx, w)
		if err != nil {
			return nil, err
		}
		for i := range cs {
			if cs[i].ID == a.ID {
				return &cs[i], nil
			}
		}
		return nil, nil
	}
	c, err := find(t.inc.Window)
	if err == nil && c == nil {
		wide := core.Window{Start: t.inc.Window.Start.Add(-7 * 24 * time.Hour), End: t.inc.Window.End}
		c, err = find(wide)
	}
	if err != nil {
		return "", "", err
	}
	if c == nil {
		return "", fmt.Sprintf("No change with id %q. Use list_changes to see valid IDs.", a.ID), nil
	}
	ch := truncateDiff(*c, maxDiffChars)
	var b strings.Builder
	fmt.Fprintf(&b, "Change %s (%s) service=%s at %s\n", ch.ID, ch.Kind, ch.Service, ts(ch.T, t.inc.StartedAt))
	if ch.Author != "" {
		fmt.Fprintf(&b, "Author: %s\n", ch.Author)
	}
	if ch.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", ch.URL)
	}
	fmt.Fprintf(&b, "Summary: %s\n", ch.Summary)
	if ch.Diff != "" {
		fmt.Fprintf(&b, "Diff:\n%s\n", ch.Diff)
	} else {
		b.WriteString("Diff: (none recorded)\n")
	}
	text := b.String()
	id := t.record("change", ToolGetChange, args, text, truncateDiff(*c, 8000))
	return id, text, nil
}

func (t *Toolbox) getTopology() (string, string, error) {
	if len(t.d.Topology) == 0 {
		return "", "No service topology is configured.", nil
	}
	text := topologyText(t.d.Topology, t.inc.Service)
	id := t.record("topology", ToolGetTopology, nil, text, t.d.Topology)
	return id, text, nil
}

func topologyText(topo core.Topology, symptom string) string {
	keys := make([]string, 0, len(topo))
	for k := range topo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("Service -> services it calls:\n")
	for _, k := range keys {
		deps := topo[k]
		if len(deps) == 0 {
			fmt.Fprintf(&b, "- %s -> (none)\n", k)
			continue
		}
		fmt.Fprintf(&b, "- %s -> %s\n", k, strings.Join(deps, ", "))
	}
	if symptom != "" {
		fmt.Fprintf(&b, "Symptomatic service: %s\n", symptom)
	}
	return b.String()
}
