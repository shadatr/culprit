package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var t0 = time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)

type fakeMetrics struct {
	names  []string
	series map[string][]core.Series
	err    error
}

func (f *fakeMetrics) Name() string { return "fake-metrics" }
func (f *fakeMetrics) ListMetrics(_ context.Context, match string) ([]string, error) {
	var out []string
	for _, n := range f.names {
		if strings.Contains(n, match) {
			out = append(out, n)
		}
	}
	return out, f.err
}
func (f *fakeMetrics) Query(_ context.Context, sel string, w core.Window, _ time.Duration) ([]core.Series, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.series[sel], nil
}

type fakeLogs struct {
	lines []core.LogLine
	err   error
}

func (f *fakeLogs) Name() string { return "fake-logs" }
func (f *fakeLogs) Services(context.Context, core.Window) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, l := range f.lines {
		if !seen[l.Service] {
			seen[l.Service] = true
			out = append(out, l.Service)
		}
	}
	return out, f.err
}
func (f *fakeLogs) Query(_ context.Context, q core.LogQuery) ([]core.LogLine, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []core.LogLine
	for _, l := range f.lines {
		if q.Service != "" && l.Service != q.Service {
			continue
		}
		if q.Contains != "" && !strings.Contains(l.Message, q.Contains) {
			continue
		}
		if q.Level != "" && l.Level != q.Level {
			continue
		}
		if !q.Window.Contains(l.T) {
			continue
		}
		out = append(out, l)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

type fakeChanges struct {
	changes []core.Change
	err     error
}

func (f *fakeChanges) Name() string { return "fake-changes" }
func (f *fakeChanges) Changes(_ context.Context, w core.Window) ([]core.Change, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []core.Change
	for _, c := range f.changes {
		if w.Contains(c.T) {
			out = append(out, c)
		}
	}
	return out, nil
}

func ramp(name, svc string, before, after float64) core.Series {
	s := core.Series{Name: name, Labels: map[string]string{"service": svc}}
	for i := -60; i < 30; i++ {
		v := before
		if i >= 0 {
			v = after
		}
		s.Points = append(s.Points, core.Point{T: t0.Add(time.Duration(i) * time.Minute), V: v})
	}
	return s
}

func fakeDeps() Deps {
	var lines []core.LogLine
	for i := -60; i < 30; i++ {
		lines = append(lines, core.LogLine{T: t0.Add(time.Duration(i) * time.Minute), Service: "catalog", Level: "info", Message: fmt.Sprintf("served request %d in %dms", i+100, 20+i%5)})
		if i >= 0 {
			lines = append(lines, core.LogLine{T: t0.Add(time.Duration(i)*time.Minute + time.Second), Service: "catalog", Level: "error", Message: fmt.Sprintf("query timeout after %dms on products", 3000+i)})
		}
	}
	return Deps{
		Metrics: &fakeMetrics{
			names: []string{"http_request_duration_seconds:p99", "db_pool_in_use:ratio"},
			series: map[string][]core.Series{
				"http_request_duration_seconds:p99":                     {ramp("http_request_duration_seconds:p99", "frontend", 0.2, 1.8)},
				"db_pool_in_use:ratio":                                  {ramp("db_pool_in_use:ratio", "catalog", 0.3, 0.98)},
				`http_request_duration_seconds:p99{service="frontend"}`: {ramp("http_request_duration_seconds:p99", "frontend", 0.2, 1.8)},
			},
		},
		Logs: &fakeLogs{lines: lines},
		Changes: &fakeChanges{changes: []core.Change{
			{ID: "deploy-catalog-1.8.2", Kind: "deploy", Service: "catalog", T: t0.Add(-3 * time.Minute), Author: "ana", Summary: "Load product tags per product", Diff: strings.Repeat("+ for p in products: query(p)\n", 200)},
			{ID: "cfg-cart-ttl", Kind: "config", Service: "cart", T: t0.Add(-50 * time.Minute), Summary: "Lower redis TTL"},
		}},
		Topology: core.Topology{"frontend": {"catalog", "cart"}, "catalog": {"postgres"}, "cart": {"redis"}},
		Ledger:   NewLedger(),
	}
}

func testIncident() core.Incident {
	return core.NewIncident("High p99 latency on frontend", "frontend", t0, time.Hour, 30*time.Minute)
}

func TestLedger(t *testing.T) {
	var l Ledger // zero value usable
	if l.Has("E1") {
		t.Fatal("empty ledger has E1")
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Add(core.Evidence{Kind: "metric", ID: "ignored"})
		}()
	}
	wg.Wait()
	if l.Len() != 50 {
		t.Fatalf("len = %d", l.Len())
	}
	list := l.List()
	for i, e := range list {
		if want := fmt.Sprintf("E%d", i+1); e.ID != want {
			t.Errorf("item %d id %s want %s", i, e.ID, want)
		}
		if e.At.IsZero() {
			t.Errorf("item %d has zero At", i)
		}
	}
	if e, ok := l.Get("E50"); !ok || e.ID != "E50" {
		t.Errorf("Get(E50) = %v %v", e, ok)
	}
	if _, ok := l.Get("E51"); ok || l.Has("E51") {
		t.Error("E51 should not exist")
	}
}

func TestDefinitionsAreValidSchemas(t *testing.T) {
	defs := Definitions()
	want := []string{ToolListMetrics, ToolGetMetric, ToolSearchLogs, ToolLogPatterns, ToolListChanges, ToolGetChange, ToolGetTopology, ToolSubmitReport}
	if len(defs) != len(want) {
		t.Fatalf("got %d defs", len(defs))
	}
	for i, d := range defs {
		if d.Name != want[i] {
			t.Errorf("def %d = %s want %s", i, d.Name, want[i])
		}
		var schema map[string]any
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			t.Errorf("%s: invalid schema: %v", d.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: schema type %v", d.Name, schema["type"])
		}
		if d.Description == "" {
			t.Errorf("%s: no description", d.Name)
		}
	}
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		args     string
		wantEv   bool
		wantErr  string
		contains []string
	}{
		{"list metrics", ToolListMetrics, `{"match":"http"}`, false, "", []string{"1 metrics", "http_request_duration_seconds:p99"}},
		{"get metric", ToolGetMetric, `{"selector":"http_request_duration_seconds:p99{service=\"frontend\"}"}`, true, "", []string{"1 series", `service="frontend"`, "trend"}},
		{"get metric explicit window", ToolGetMetric, `{"selector":"db_pool_in_use:ratio","start":"2026-10-20T13:00:00Z","end":"2026-10-20T14:30:00Z"}`, true, "", []string{"db_pool_in_use:ratio"}},
		{"get metric no series", ToolGetMetric, `{"selector":"nope"}`, false, "", []string{"No series"}},
		{"get metric bad time", ToolGetMetric, `{"selector":"up","start":"yesterday"}`, false, "RFC3339", nil},
		{"get metric reversed window", ToolGetMetric, `{"selector":"up","start":"2026-10-20T15:00:00Z","end":"2026-10-20T14:00:00Z"}`, false, "after start", nil},
		{"get metric missing selector", ToolGetMetric, `{}`, false, "selector is required", nil},
		{"search logs", ToolSearchLogs, `{"service":"catalog","level":"error"}`, true, "", []string{"lines from catalog", "query timeout", "Samples"}},
		{"search logs contains", ToolSearchLogs, `{"service":"catalog","contains":"served"}`, true, "", []string{"served request"}},
		{"search logs none", ToolSearchLogs, `{"service":"ghost"}`, false, "", []string{"No log lines"}},
		{"log patterns", ToolLogPatterns, `{"service":"catalog"}`, true, "", []string{"catalog:", "lines scanned"}},
		{"list changes", ToolListChanges, `{}`, true, "", []string{"deploy-catalog-1.8.2", "cfg-cart-ttl"}},
		{"list changes by service", ToolListChanges, `{"service":"cart"}`, true, "", []string{"cfg-cart-ttl"}},
		{"get change", ToolGetChange, `{"id":"deploy-catalog-1.8.2"}`, true, "", []string{"Author: ana", "Diff:", "truncated"}},
		{"get change unknown", ToolGetChange, `{"id":"x"}`, false, "", []string{"No change"}},
		{"topology", ToolGetTopology, ``, true, "", []string{"frontend -> catalog, cart"}},
		{"bad json", ToolGetMetric, `"not json"`, false, "invalid arguments", nil},
		{"unknown tool", "rm_rf", `{}`, false, "unknown tool", nil},
		{"submit report", ToolSubmitReport, `{}`, false, "handled by the investigator", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fakeDeps()
			tb := New(d, testIncident(), nil)
			id, text, err := tb.Dispatch(context.Background(), tt.tool, json.RawMessage(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (id != "") != tt.wantEv {
				t.Errorf("evidence id = %q, want evidence %v", id, tt.wantEv)
			}
			if id != "" && !d.Ledger.Has(id) {
				t.Errorf("id %s not in ledger", id)
			}
			if len(text) > MaxResultChars {
				t.Errorf("result %d chars > cap", len(text))
			}
			for _, c := range tt.contains {
				if !strings.Contains(text, c) {
					t.Errorf("result missing %q:\n%s", c, text)
				}
			}
		})
	}
}

func TestDispatchNilSources(t *testing.T) {
	tb := New(Deps{}, testIncident(), nil)
	for _, c := range []struct{ tool, args string }{
		{ToolListMetrics, `{}`},
		{ToolGetMetric, `{"selector":"up"}`},
		{ToolSearchLogs, `{"service":"a"}`},
		{ToolLogPatterns, `{"service":"a"}`},
		{ToolListChanges, `{}`},
		{ToolGetChange, `{"id":"a"}`},
		{ToolGetTopology, `{}`},
	} {
		id, text, err := tb.Dispatch(context.Background(), c.tool, json.RawMessage(c.args))
		if err != nil || id != "" {
			t.Errorf("%s: id %q err %v", c.tool, id, err)
		}
		if !strings.Contains(text, "configured") {
			t.Errorf("%s: text %q", c.tool, text)
		}
	}
}

func TestDispatchSourceErrorIsToolError(t *testing.T) {
	d := Deps{Metrics: &fakeMetrics{err: errors.New("prometheus down")}, Ledger: NewLedger()}
	_, _, err := New(d, testIncident(), nil).Dispatch(context.Background(), ToolGetMetric, json.RawMessage(`{"selector":"up"}`))
	if err == nil || !strings.Contains(err.Error(), "prometheus down") {
		t.Errorf("err = %v", err)
	}
}

func TestDispatchCapsLargeResults(t *testing.T) {
	var series []core.Series
	for i := range 100 {
		series = append(series, ramp("m", fmt.Sprintf("svc-%03d-with-a-rather-long-name", i), 1, 2))
	}
	d := Deps{Metrics: &fakeMetrics{series: map[string][]core.Series{"m": series}}, Ledger: NewLedger()}
	_, text, err := New(d, testIncident(), nil).Dispatch(context.Background(), ToolGetMetric, json.RawMessage(`{"selector":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > MaxResultChars {
		t.Errorf("len %d", len(text))
	}
}

func TestTriage(t *testing.T) {
	d := fakeDeps()
	inc := testIncident()
	tr, err := Triage(context.Background(), d, inc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Summary) > MaxTriageChars {
		t.Errorf("summary too long: %d", len(tr.Summary))
	}
	for _, want := range []string{"Triage for", "deploy-catalog-1.8.2", "Topology"} {
		if !strings.Contains(tr.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, tr.Summary)
		}
	}
	if len(tr.Changes) != 2 {
		t.Errorf("changes = %+v", tr.Changes)
	}
	if len(tr.EvidenceIDs) != d.Ledger.Len() {
		t.Errorf("evidence ids %v vs ledger %d", tr.EvidenceIDs, d.Ledger.Len())
	}
	for _, id := range tr.EvidenceIDs {
		if !strings.Contains(tr.Summary, "["+id+"]") {
			t.Errorf("summary does not tag %s", id)
		}
	}
}

func TestTriageNilSourcesAndErrors(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
		want []string
	}{
		{"nil sources", Deps{Ledger: NewLedger()}, []string{"no metrics source", "no logs source", "no changes source"}},
		{"failing sources", Deps{
			Metrics: &fakeMetrics{err: errors.New("boom-m")},
			Logs:    &fakeLogs{err: errors.New("boom-l")},
			Changes: &fakeChanges{err: errors.New("boom-c")},
			Ledger:  NewLedger(),
		}, []string{"boom-m", "boom-l", "boom-c", "Notes"}},
		{"too many metrics", Deps{Metrics: &fakeMetrics{names: make([]string, 61)}, Ledger: NewLedger()}, []string{"61 metrics available"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := Triage(context.Background(), tt.deps, testIncident(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(tr.Summary, w) {
					t.Errorf("summary missing %q:\n%s", w, tr.Summary)
				}
			}
		})
	}
}

func TestTriageCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Triage(ctx, fakeDeps(), testIncident(), []string{"m"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if _, err := Triage(context.Background(), Deps{}, testIncident(), nil); err == nil {
		t.Error("nil ledger should error")
	}
}

func TestCapText(t *testing.T) {
	s := strings.Repeat("line of text é\n", 1000)
	got := capText(s, 500)
	if len(got) > 500 || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("len %d suffix %q", len(got), got[len(got)-20:])
	}
	if capText("short", 500) != "short" {
		t.Error("short text changed")
	}
}

func TestPrioritise(t *testing.T) {
	got := prioritise([]string{"zeta", "redis", "cart", "frontend", "catalog"}, "frontend", core.Topology{"frontend": {"catalog", "cart"}, "cart": {"redis"}})
	want := "frontend,catalog,cart,redis,zeta"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v want %s", got, want)
	}
}

func TestFallbackMine(t *testing.T) {
	tpls := fallbackMine([]core.LogLine{
		{Message: "timeout after 300ms calling 10.0.0.1:5432", Level: "error"},
		{Message: "timeout after 450ms calling 10.0.0.2:5432", Level: "error"},
		{Message: "ok"},
	})
	if len(tpls) != 2 || tpls[0].Count != 2 || tpls[0].Level != "error" {
		t.Errorf("templates = %+v", tpls)
	}
}
