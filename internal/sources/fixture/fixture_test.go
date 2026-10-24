package fixture

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

const scenarioYAML = `name: catalog-bad-deploy
description: A catalog release adds an N+1 query.
incident:
  title: High p99 latency on frontend
  service: frontend
  started_at: 2026-10-20T14:05:00Z
  lookback: 2h
  after: 30m
topology:
  frontend: [catalog, cart]
  catalog: [postgres]
truth:
  service: catalog
  change_id: deploy-catalog-1.8.2
  category: bad-deploy
  keywords: [query, N+1]
`

// t0 is the scenario's incident start.
var t0 = time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)

func writeScenario(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func fullScenario(t *testing.T) *Scenario {
	t.Helper()
	ts := func(d time.Duration) string { return strconv.FormatInt(t0.Add(d).Unix(), 10) }
	metrics := `[
 {"name":"http_request_duration_seconds:p99","labels":{"service":"frontend"},
  "points":[[` + ts(-3*time.Hour) + `,0.1],[` + ts(-time.Hour) + `,0.2],[` + ts(10*time.Minute) + `,1.5]]},
 {"name":"http_request_duration_seconds:p99","labels":{"service":"catalog"},
  "points":[[` + ts(-time.Hour) + `,0.05],[` + ts(5*time.Minute) + `,0.9]]},
 {"name":"process_cpu:ratio","labels":{"service":"catalog"},"points":[[` + ts(-3*time.Hour) + `,0.3]]}
]`
	logs := strings.Join([]string{
		`{"t":"2026-10-20T14:10:00Z","service":"catalog","level":"ERROR","message":"slow Query on products"}`,
		`{"t":"2026-10-20T13:00:00Z","service":"catalog","level":"info","message":"started"}`,
		``,
		`{"t":"2026-10-20T14:06:00Z","service":"frontend","level":"error","message":"upstream timeout"}`,
		`{"t":"2026-10-20T10:00:00Z","service":"old","level":"info","message":"outside window"}`,
	}, "\n")
	changes := `[
 {"id":"deploy-catalog-1.8.2","kind":"deploy","service":"catalog","t":"2026-10-20T13:58:00Z","summary":"catalog 1.8.2"},
 {"id":"cfg-1","kind":"config","service":"cart","t":"2026-10-20T13:00:00Z","summary":"cache ttl"},
 {"id":"ancient","kind":"deploy","service":"cart","t":"2026-10-19T13:00:00Z","summary":"old"}
]`
	dir := writeScenario(t, map[string]string{
		"scenario.yaml": scenarioYAML,
		"metrics.json":  metrics,
		"logs.jsonl":    logs,
		"changes.json":  changes,
	})
	sc, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return sc
}

func TestLoadIncident(t *testing.T) {
	sc := fullScenario(t)
	want := core.Incident{
		ID:        "catalog-bad-deploy",
		Title:     "High p99 latency on frontend",
		Service:   "frontend",
		StartedAt: t0,
		Window:    core.Window{Start: t0.Add(-2 * time.Hour), End: t0.Add(30 * time.Minute)},
		Source:    "eval",
	}
	if !reflect.DeepEqual(sc.Incident, want) {
		t.Fatalf("Incident = %+v\nwant %+v", sc.Incident, want)
	}
	if sc.Truth.Service != "catalog" || sc.Truth.ChangeID != "deploy-catalog-1.8.2" || len(sc.Truth.Keywords) != 2 {
		t.Fatalf("Truth = %+v", sc.Truth)
	}
	if !reflect.DeepEqual(sc.Topology["frontend"], []string{"catalog", "cart"}) {
		t.Fatalf("Topology = %+v", sc.Topology)
	}
	if sc.Name != "catalog-bad-deploy" || sc.Description == "" {
		t.Fatalf("Name/Description = %q/%q", sc.Name, sc.Description)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing yaml", map[string]string{}, "read scenario"},
		{"no start", map[string]string{"scenario.yaml": "name: x\ntruth:\n  service: a\n"}, "started_at is required"},
		{"no truth", map[string]string{"scenario.yaml": "name: x\nincident:\n  started_at: 2026-10-20T14:05:00Z\n"}, "truth.service is required"},
		{"bad duration", map[string]string{"scenario.yaml": "name: x\nincident:\n  started_at: 2026-10-20T14:05:00Z\n  lookback: 2 hours\ntruth:\n  service: a\n"}, "lookback"},
		{"bad metrics", map[string]string{"scenario.yaml": scenarioYAML, "metrics.json": "{"}, "metrics.json"},
		{"bad logs", map[string]string{"scenario.yaml": scenarioYAML, "logs.jsonl": "{}\nnot json\n"}, "logs.jsonl line 2"},
		{"bad changes", map[string]string{"scenario.yaml": scenarioYAML, "changes.json": "[1]"}, "changes.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeScenario(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadOptionalFilesAndDefaults(t *testing.T) {
	dir := writeScenario(t, map[string]string{"scenario.yaml": "incident:\n  started_at: 2026-10-20T14:05:00Z\ntruth:\n  service: a\n"})
	sc, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != filepath.Base(dir) {
		t.Fatalf("Name = %q, want dir name", sc.Name)
	}
	if got := sc.Incident.Window.End.Sub(sc.Incident.Window.Start); got != defaultLookback+defaultAfter {
		t.Fatalf("window length = %v", got)
	}
	ctx := context.Background()
	if s, err := sc.Metrics().Query(ctx, "up", sc.Incident.Window, 0); err != nil || len(s) != 0 {
		t.Fatalf("Query = %v, %v", s, err)
	}
	if l, err := sc.Logs().Query(ctx, core.LogQuery{}); err != nil || len(l) != 0 {
		t.Fatalf("Logs = %v, %v", l, err)
	}
	if c, err := sc.Changes().Changes(ctx, sc.Incident.Window); err != nil || len(c) != 0 {
		t.Fatalf("Changes = %v, %v", c, err)
	}
}

func TestMetrics(t *testing.T) {
	sc := fullScenario(t)
	m := sc.Metrics()
	ctx := context.Background()
	w := sc.Incident.Window

	names, err := m.ListMetrics(ctx, "HTTP")
	if err != nil || !reflect.DeepEqual(names, []string{"http_request_duration_seconds:p99"}) {
		t.Fatalf("ListMetrics = %v, %v", names, err)
	}
	all, _ := m.ListMetrics(ctx, "")
	if len(all) != 2 {
		t.Fatalf("ListMetrics(\"\") = %v", all)
	}

	tests := []struct {
		sel        string
		wantSeries int
		wantPoints int
	}{
		{"http_request_duration_seconds:p99", 2, 4},
		{`http_request_duration_seconds:p99{service="frontend"}`, 1, 2},
		{`{__name__=~"http_.*",service=~"cat.*"}`, 1, 2},
		{`process_cpu:ratio`, 0, 0}, // only point is outside the window
		{`nope`, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.sel, func(t *testing.T) {
			got, err := m.Query(ctx, tt.sel, w, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			pts := 0
			for _, s := range got {
				pts += len(s.Points)
				for _, p := range s.Points {
					if !w.Contains(p.T) {
						t.Fatalf("point %v outside window", p.T)
					}
				}
			}
			if len(got) != tt.wantSeries || pts != tt.wantPoints {
				t.Fatalf("got %d series %d points, want %d %d", len(got), pts, tt.wantSeries, tt.wantPoints)
			}
		})
	}

	if _, err := m.Query(ctx, "rate(x[5m])", w, 0); err == nil {
		t.Fatal("want error for PromQL expression")
	}
	// Results must be copies.
	got, _ := m.Query(ctx, `http_request_duration_seconds:p99{service="frontend"}`, w, 0)
	got[0].Labels["service"] = "mutated"
	again, _ := m.Query(ctx, `http_request_duration_seconds:p99{service="frontend"}`, w, 0)
	if len(again) != 1 {
		t.Fatal("source was mutated through returned labels")
	}
}

func TestLogs(t *testing.T) {
	sc := fullScenario(t)
	l := sc.Logs()
	ctx := context.Background()
	w := sc.Incident.Window

	svcs, err := l.Services(ctx, w)
	if err != nil || !reflect.DeepEqual(svcs, []string{"catalog", "frontend"}) {
		t.Fatalf("Services = %v, %v", svcs, err)
	}

	tests := []struct {
		name string
		q    core.LogQuery
		want []string
	}{
		{"all in window oldest first", core.LogQuery{Window: w}, []string{"started", "upstream timeout", "slow Query on products"}},
		{"service", core.LogQuery{Service: "catalog", Window: w}, []string{"started", "slow Query on products"}},
		{"service exact", core.LogQuery{Service: "cat", Window: w}, nil},
		{"contains case-insensitive", core.LogQuery{Contains: "QUERY", Window: w}, []string{"slow Query on products"}},
		{"level case-insensitive", core.LogQuery{Level: "error", Window: w}, []string{"upstream timeout", "slow Query on products"}},
		{"limit", core.LogQuery{Window: w, Limit: 1}, []string{"started"}},
		{"zero window is unbounded", core.LogQuery{Service: "old"}, []string{"outside window"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Query(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var msgs []string
			for _, g := range got {
				msgs = append(msgs, g.Message)
			}
			if !reflect.DeepEqual(msgs, tt.want) {
				t.Fatalf("got %q, want %q", msgs, tt.want)
			}
		})
	}
}

func TestChanges(t *testing.T) {
	sc := fullScenario(t)
	got, err := sc.Changes().Changes(context.Background(), sc.Incident.Window)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if want := []string{"deploy-catalog-1.8.2", "cfg-1"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestSourcesImplementInterfaces(t *testing.T) {
	sc := fullScenario(t)
	for _, n := range []string{sc.Metrics().Name(), sc.Logs().Name(), sc.Changes().Name()} {
		if n != "fixture" {
			t.Fatalf("Name = %q", n)
		}
	}
}
