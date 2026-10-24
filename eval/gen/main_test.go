package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shadatr/culprit/internal/core"
)

type yamlScenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Incident    struct {
		Title     string `yaml:"title"`
		Service   string `yaml:"service"`
		StartedAt string `yaml:"started_at"`
		Lookback  string `yaml:"lookback"`
		After     string `yaml:"after"`
	} `yaml:"incident"`
	Topology map[string][]string `yaml:"topology"`
	Truth    struct {
		Service  string   `yaml:"service"`
		ChangeID string   `yaml:"change_id"`
		Category string   `yaml:"category"`
		Keywords []string `yaml:"keywords"`
	} `yaml:"truth"`
}

type jsonSeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"`
}

var categories5 = []string{"bad-deploy", "config-change", "resource-exhaustion", "dependency-failure", "traffic-spike", "certificate-expiry", "other"}

var metricNames5 = []string{
	"http_request_duration_seconds:p99", "http_requests:rate1m", "http_errors:ratio1m",
	"process_cpu:ratio", "process_memory:bytes", "db_pool_in_use:ratio",
	"db_query_duration_seconds:p99", "cache_hit:ratio",
}

var sets = []struct {
	name, committed string
	min             int
}{
	{"dev", filepath.Join("..", "scenarios"), 10},
	{"heldout", filepath.Join("..", "heldout"), 8},
}

func mustSet(t *testing.T, set string) []scenario {
	t.Helper()
	list, err := scenarioSet(set)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestGeneratedScenariosAreValid(t *testing.T) {
	for _, set := range sets {
		t.Run(set.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := generate(dir, set.name); err != nil {
				t.Fatal(err)
			}
			list := mustSet(t, set.name)
			if len(list) < set.min {
				t.Fatalf("want >= %d scenarios, got %d", set.min, len(list))
			}
			for _, sc := range list {
				w := sc.world()
				var logged []string
				for _, s := range append(append([]svcSpec{}, w.http...), w.infra...) {
					logged = append(logged, s.name)
				}
				t.Run(sc.name, func(t *testing.T) { checkScenario(t, filepath.Join(dir, sc.name), logged) })
			}
		})
	}
}

// TestSetsAreDisjoint keeps the held-out set blind: no shared scenario
// names, services or alert times with the dev set.
func TestSetsAreDisjoint(t *testing.T) {
	dev, held := mustSet(t, "dev"), mustSet(t, "heldout")
	devSvc := map[string]bool{}
	for _, e := range shop.topology {
		devSvc[e.svc] = true
		for _, c := range e.calls {
			devSvc[c] = true
		}
	}
	for _, e := range heldout.topology {
		for _, s := range append([]string{e.svc}, e.calls...) {
			if devSvc[s] {
				t.Errorf("held-out service %q also in dev topology", s)
			}
		}
	}
	names := map[string]bool{}
	for _, sc := range dev {
		names[sc.name] = true
	}
	for _, sc := range held {
		if names[sc.name] {
			t.Errorf("scenario name %q in both sets", sc.name)
		}
	}
}

func checkScenario(t *testing.T, dir string, logged []string) {
	var total int64
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		total += int64(len(b))
		return b
	}

	// scenario.yaml
	var sc yamlScenario
	dec := yaml.NewDecoder(bytes.NewReader(read("scenario.yaml")))
	dec.KnownFields(true)
	if err := dec.Decode(&sc); err != nil {
		t.Fatalf("scenario.yaml: %v", err)
	}
	if sc.Name != filepath.Base(dir) {
		t.Errorf("name %q != dir %q", sc.Name, filepath.Base(dir))
	}
	start, err := time.Parse(time.RFC3339, sc.Incident.StartedAt)
	if err != nil {
		t.Fatalf("started_at: %v", err)
	}
	lb, err1 := time.ParseDuration(sc.Incident.Lookback)
	af, err2 := time.ParseDuration(sc.Incident.After)
	if err1 != nil || err2 != nil {
		t.Fatalf("durations: %v %v", err1, err2)
	}
	ws, we := start.Add(-lb), start.Add(af)
	if sc.Incident.Title == "" || sc.Incident.Service == "" {
		t.Error("incident title/service missing")
	}
	services := map[string]bool{}
	for k, vs := range sc.Topology {
		services[k] = true
		for _, v := range vs {
			services[v] = true
		}
	}
	if !services[sc.Truth.Service] || !services[sc.Incident.Service] {
		t.Errorf("truth service %q or incident service %q not in topology", sc.Truth.Service, sc.Incident.Service)
	}
	if !slices.Contains(categories5, sc.Truth.Category) {
		t.Errorf("bad category %q", sc.Truth.Category)
	}
	if len(sc.Truth.Keywords) == 0 {
		t.Error("no truth keywords")
	}
	if !strings.Contains(strings.ToLower(sc.Description), "red herring") {
		t.Error("description does not name the red herring")
	}

	// metrics.json
	var series []jsonSeries
	if err := json.Unmarshal(read("metrics.json"), &series); err != nil {
		t.Fatalf("metrics.json: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range series {
		seen[s.Name] = true
		svc := s.Labels["service"]
		if svc == "" {
			t.Errorf("series %s without service label", s.Name)
		}
		if !services[svc] {
			t.Errorf("series %s service %q not in topology", s.Name, svc)
		}
		if len(s.Points) < 100 {
			t.Errorf("series %s{%s} has only %d points", s.Name, svc, len(s.Points))
		}
		for i, p := range s.Points {
			pt := time.Unix(int64(p[0]), 0)
			if pt.Before(ws) || pt.After(we) {
				t.Fatalf("series %s{%s} point %v outside window", s.Name, svc, pt)
			}
			if i > 0 && p[0] <= s.Points[i-1][0] {
				t.Fatalf("series %s{%s} timestamps not increasing at %d", s.Name, svc, i)
			}
			if p[1] < 0 || (strings.HasSuffix(s.Name, ":ratio") || strings.HasSuffix(s.Name, "ratio1m")) && p[1] > 1 {
				t.Fatalf("series %s{%s} bad value %v", s.Name, svc, p[1])
			}
		}
	}
	for _, n := range metricNames5 {
		if !seen[n] {
			t.Errorf("metric %s missing", n)
		}
	}

	// changes.json
	var changes []core.Change
	if err := json.Unmarshal(read("changes.json"), &changes); err != nil {
		t.Fatalf("changes.json: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range changes {
		if ids[c.ID] {
			t.Errorf("duplicate change id %s", c.ID)
		}
		ids[c.ID] = true
		if c.Summary == "" || c.Kind == "" || c.T.IsZero() {
			t.Errorf("incomplete change %+v", c)
		}
		if !slices.Contains([]string{"deploy", "commit", "config", "flag", "infra"}, c.Kind) {
			t.Errorf("change %s bad kind %q", c.ID, c.Kind)
		}
	}
	if sc.Truth.ChangeID != "" && !ids[sc.Truth.ChangeID] {
		t.Errorf("truth change %s not in changes.json", sc.Truth.ChangeID)
	}
	if len(changes) < 3 {
		t.Errorf("want several changes, got %d", len(changes))
	}

	// logs.jsonl
	sv := bufio.NewScanner(bytes.NewReader(read("logs.jsonl")))
	sv.Buffer(make([]byte, 1<<20), 1<<20)
	var prev time.Time
	n := 0
	perSvc := map[string]int{}
	for sv.Scan() {
		var l core.LogLine
		if err := json.Unmarshal(sv.Bytes(), &l); err != nil {
			t.Fatalf("logs.jsonl line %d: %v", n+1, err)
		}
		n++
		if l.Service == "" || l.Message == "" {
			t.Fatalf("logs line %d missing service/message", n)
		}
		if l.T.Before(ws) || l.T.After(we) {
			t.Fatalf("logs line %d at %v outside window", n, l.T)
		}
		if l.T.Before(prev) {
			t.Fatalf("logs line %d not sorted", n)
		}
		prev = l.T
		perSvc[l.Service]++
	}
	if err := sv.Err(); err != nil {
		t.Fatal(err)
	}
	for _, s := range logged {
		if perSvc[s] == 0 {
			t.Errorf("no logs for %s", s)
		}
	}
	if total > 3<<20 {
		t.Errorf("scenario is %d bytes, want < 3 MiB", total)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	for _, set := range sets {
		t.Run(set.name, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			if err := generate(a, set.name); err != nil {
				t.Fatal(err)
			}
			if err := generate(b, set.name); err != nil {
				t.Fatal(err)
			}
			compareTrees(t, mustSet(t, set.name), a, b)
		})
	}
}

// TestCommittedScenariosUpToDate checks eval/scenarios and eval/heldout match
// the generator byte for byte.
func TestCommittedScenariosUpToDate(t *testing.T) {
	for _, set := range sets {
		t.Run(set.name, func(t *testing.T) {
			fresh := t.TempDir()
			if err := generate(fresh, set.name); err != nil {
				t.Fatal(err)
			}
			compareTrees(t, mustSet(t, set.name), fresh, set.committed)
		})
	}
}

func compareTrees(t *testing.T, list []scenario, want, got string) {
	t.Helper()
	for _, sc := range list {
		for _, f := range []string{"scenario.yaml", "metrics.json", "logs.jsonl", "changes.json"} {
			a, err := os.ReadFile(filepath.Join(want, sc.name, f))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(got, sc.name, f))
			if err != nil {
				t.Fatalf("%v (run: go run ./eval/gen -set dev && go run ./eval/gen -set heldout)", err)
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs (run: go run ./eval/gen -set dev && go run ./eval/gen -set heldout)", sc.name, f)
			}
		}
	}
}
