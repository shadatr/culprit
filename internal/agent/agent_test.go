package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/llm"
	"github.com/shadatr/culprit/internal/tools"
)

var t0 = time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// In-memory sources: the catalog deploy at t0-3m makes catalog's DB pool
// saturate and frontend latency rise; a cart config change is a red herring.

type memMetrics struct{ series []core.Series }

func (m *memMetrics) Name() string { return "mem" }
func (m *memMetrics) ListMetrics(_ context.Context, match string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range m.series {
		if strings.Contains(s.Name, match) && !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s.Name)
		}
	}
	return out, nil
}

var selRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{service="([^"]*)"\})?$`)

func (m *memMetrics) Query(_ context.Context, sel string, w core.Window, _ time.Duration) ([]core.Series, error) {
	g := selRe.FindStringSubmatch(sel)
	if g == nil {
		return nil, fmt.Errorf("unsupported selector %q", sel)
	}
	var out []core.Series
	for _, s := range m.series {
		if s.Name != g[1] || (g[2] != "" && s.Labels["service"] != g[2]) {
			continue
		}
		c := core.Series{Name: s.Name, Labels: s.Labels}
		for _, p := range s.Points {
			if w.Contains(p.T) {
				c.Points = append(c.Points, p)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

type memLogs struct{ lines []core.LogLine }

func (m *memLogs) Name() string { return "mem" }
func (m *memLogs) Services(context.Context, core.Window) ([]string, error) {
	return []string{"cart", "catalog", "frontend"}, nil
}
func (m *memLogs) Query(_ context.Context, q core.LogQuery) ([]core.LogLine, error) {
	var out []core.LogLine
	for _, l := range m.lines {
		if (q.Service == "" || l.Service == q.Service) && strings.Contains(l.Message, q.Contains) &&
			(q.Level == "" || l.Level == q.Level) && q.Window.Contains(l.T) {
			out = append(out, l)
		}
	}
	return out, nil
}

type memChanges struct{ changes []core.Change }

func (m *memChanges) Name() string { return "mem" }
func (m *memChanges) Changes(_ context.Context, w core.Window) ([]core.Change, error) {
	var out []core.Change
	for _, c := range m.changes {
		if w.Contains(c.T) {
			out = append(out, c)
		}
	}
	return out, nil
}

func series(name, svc string, base, incident float64, onset time.Duration) core.Series {
	s := core.Series{Name: name, Labels: map[string]string{"service": svc}}
	for i := -120; i < 30; i++ {
		ts := t0.Add(time.Duration(i) * time.Minute)
		v := base + float64(i%3)*base*0.02
		if ts.Sub(t0) >= onset {
			v = incident
		}
		s.Points = append(s.Points, core.Point{T: ts, V: v})
	}
	return s
}

func scenarioDeps(client llm.Client) Deps {
	var lines []core.LogLine
	for i := -120; i < 30; i++ {
		ts := t0.Add(time.Duration(i) * time.Minute)
		lines = append(lines,
			core.LogLine{T: ts, Service: "frontend", Level: "info", Message: fmt.Sprintf("GET /products 200 %dms", 40+i%7)},
			core.LogLine{T: ts, Service: "cart", Level: "info", Message: "cache hit for cart"},
		)
		if i >= -2 {
			lines = append(lines, core.LogLine{T: ts.Add(time.Second), Service: "catalog", Level: "error", Message: fmt.Sprintf("pool exhausted waiting %dms for connection", 900+i)})
		}
	}
	return Deps{
		LLM: client,
		Metrics: &memMetrics{series: []core.Series{
			series("http_request_duration_seconds:p99", "frontend", 0.2, 1.9, 0),
			series("db_pool_in_use:ratio", "catalog", 0.3, 1.0, -2*time.Minute),
			series("cache_hit:ratio", "cart", 0.95, 0.95, 0),
		}},
		Logs: &memLogs{lines: lines},
		Changes: &memChanges{changes: []core.Change{
			{ID: "deploy-catalog-1.8.2", Kind: "deploy", Service: "catalog", T: t0.Add(-3 * time.Minute), Summary: "Load tags per product (N+1 query)", Diff: "+ for _, p := range products { db.Query(tagsSQL, p.ID) }"},
			{ID: "cfg-cart-ttl", Kind: "config", Service: "cart", T: t0.Add(-90 * time.Minute), Summary: "Lower redis TTL to 60s"},
		}},
		Topology: core.Topology{"frontend": {"catalog", "cart"}, "catalog": {"postgres"}, "cart": {"redis"}},
	}
}

func incident() core.Incident {
	inc := core.NewIncident("High p99 latency on frontend", "frontend", t0, 2*time.Hour, 30*time.Minute)
	inc.ID = "inc-1"
	return inc
}

// placeholderClient wraps a Scripted client and substitutes evidence IDs in
// scripted tool arguments, so scripts stay valid whatever triage records:
// $LAST is the latest evidence ID returned by a tool, $TRIAGE the first
// evidence ID in the triage summary (or $LAST when triage found nothing).
type placeholderClient struct {
	*llm.Scripted
	onCall func(n int) // optional hook, called before each response
	mu     sync.Mutex
	n      int
}

var evRe = regexp.MustCompile(`\bE\d+\b`)

func (p *placeholderClient) Chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	if p.onCall != nil {
		p.onCall(n)
	}
	resp, err := p.Scripted.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	var last, triage string
	for _, m := range req.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "evidence_id: ") {
			last = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(m.Content, "\n", 2)[0], "evidence_id: "))
		}
		if m.Role == "user" && triage == "" && strings.Contains(m.Content, "Deterministic triage") {
			triage = evRe.FindString(m.Content)
		}
	}
	if triage == "" {
		triage = last
	}
	for i, tc := range resp.Message.ToolCalls {
		a := strings.ReplaceAll(string(tc.Arguments), "$LAST", last)
		a = strings.ReplaceAll(a, "$TRIAGE", triage)
		resp.Message.ToolCalls[i].Arguments = json.RawMessage(a)
	}
	return resp, nil
}

func call(id, name, args string) llm.Response {
	return llm.Response{
		Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}},
		Usage:   llm.Usage{PromptTokens: 1000, CompletionTokens: 100},
	}
}

func text(s string) llm.Response {
	return llm.Response{Message: llm.Message{Role: "assistant", Content: s}, Usage: llm.Usage{PromptTokens: 500, CompletionTokens: 50}}
}

const validReport = `{"summary":"Catalog release 1.8.2 introduced an N+1 query that exhausted the DB pool; frontend p99 rose as a result.",
"timeline":[{"t":"2026-10-20T14:02:00Z","text":"catalog 1.8.2 deployed","evidence_ids":["$LAST"]},{"t":"2026-10-20T14:05:00Z","text":"frontend p99 rises"}],
"hypotheses":[
 {"title":"N+1 query in catalog 1.8.2 exhausts DB pool","explanation":"deploy precedes pool saturation and frontend latency","service":"catalog","category":"bad-deploy","change_id":"deploy-catalog-1.8.2","confidence":0.85,"evidence_ids":["$LAST","$TRIAGE"],"next_steps":["Roll back catalog to 1.8.1"]},
 {"title":"Cart TTL change","explanation":"red herring: 90 minutes earlier, cart healthy","service":"cart","category":"config-change","confidence":0.05,"evidence_ids":["$TRIAGE"]}]}`

type recorder struct {
	mu     sync.Mutex
	events []core.Event
}

func (r *recorder) sink(e core.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

func (r *recorder) check(t *testing.T, wantStatus string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) < 3 {
		t.Fatalf("only %d events", len(r.events))
	}
	for i, e := range r.events {
		if e.Seq != i+1 {
			t.Errorf("event %d seq %d", i, e.Seq)
		}
		if !json.Valid(e.Data) {
			t.Errorf("event %d (%s) invalid data", i, e.Type)
		}
	}
	if r.events[0].Type != core.EventStarted {
		t.Errorf("first event %s", r.events[0].Type)
	}
	last := r.events[len(r.events)-1]
	if last.Type != core.EventFinished {
		t.Fatalf("last event %s", last.Type)
	}
	var fin struct{ Status string }
	json.Unmarshal(last.Data, &fin)
	if fin.Status != wantStatus {
		t.Errorf("finished status %q want %q", fin.Status, wantStatus)
	}
	if prev := r.events[len(r.events)-2]; prev.Type != core.EventReport {
		t.Errorf("event before finished is %s", prev.Type)
	}
}

func count(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}

func TestRunHappyPath(t *testing.T) {
	s := &llm.Scripted{Responses: []llm.Response{
		call("c1", "get_metric", `{"selector":"db_pool_in_use:ratio{service=\"catalog\"}"}`),
		call("c2", "search_logs", `{"service":"catalog","level":"error"}`),
		call("c3", "get_metric", `{"selector":"up","start":"not-a-time"}`), // tool error goes back to the model
		call("c4", "get_change", `{"id":"deploy-catalog-1.8.2"}`),
		call("c5", "submit_report", validReport),
	}}
	rec := &recorder{}
	inv := New(scenarioDeps(&placeholderClient{Scripted: s}), Config{Model: "test-model"})
	rep, err := inv.Run(context.Background(), incident(), rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusCompleted {
		t.Fatalf("status %s: %s", rep.Status, rep.Summary)
	}
	if rep.IncidentID != "inc-1" || rep.Model != "test-model" || rep.FinishedAt.Before(rep.StartedAt) {
		t.Errorf("report meta %+v", rep)
	}
	if len(rep.Hypotheses) != 2 || rep.Hypotheses[0].ChangeID != "deploy-catalog-1.8.2" {
		t.Errorf("hypotheses %+v", rep.Hypotheses)
	}
	if rep.Usage.Steps != 5 || rep.Usage.ToolCalls != 5 || rep.Usage.PromptTokens != 5000 || rep.Usage.CompletionTokens != 500 {
		t.Errorf("usage %+v", rep.Usage)
	}
	ids := map[string]bool{}
	for _, e := range rep.Evidence {
		ids[e.ID] = true
	}
	for _, h := range rep.Hypotheses {
		for _, id := range h.EvidenceIDs {
			if !ids[id] {
				t.Errorf("cited %s not in report evidence", id)
			}
		}
	}
	// get_metric, search_logs and get_change each recorded evidence.
	toolEv := 0
	for _, e := range rep.Evidence {
		if e.Tool != "triage" {
			toolEv++
		}
	}
	if toolEv != 3 {
		t.Errorf("tool evidence = %d, want 3", toolEv)
	}

	reqs := s.Calls()
	if len(reqs) != 5 {
		t.Fatalf("requests %d", len(reqs))
	}
	first := reqs[0]
	if first.Messages[0].Role != "system" || !strings.Contains(first.Messages[0].Content, "read-only") {
		t.Error("missing system prompt")
	}
	if !strings.Contains(first.Messages[1].Content, "Deterministic triage") || !strings.Contains(first.Messages[1].Content, "deploy-catalog-1.8.2") {
		t.Errorf("user prompt lacks triage:\n%s", first.Messages[1].Content)
	}
	if len(first.Tools) != len(tools.Definitions()) || first.ToolChoice != "auto" || first.Model != "test-model" {
		t.Errorf("first request %+v", first.ToolChoice)
	}
	if *first.Temperature != DefaultTemperature {
		t.Errorf("temperature %v", *first.Temperature)
	}
	lastTool := func(r llm.Request) llm.Message { return r.Messages[len(r.Messages)-1] }
	if m := lastTool(reqs[1]); m.Role != "tool" || m.ToolCallID != "c1" || !strings.HasPrefix(m.Content, "evidence_id: E") {
		t.Errorf("tool message %+v", m)
	}
	if m := lastTool(reqs[3]); !strings.HasPrefix(m.Content, "error: ") {
		t.Errorf("tool error not returned to model: %q", m.Content)
	}

	rec.check(t, core.StatusCompleted)
	ty := rec.types()
	if count(ty, core.EventTriage) != 1 || count(ty, core.EventToolCall) != 5 || count(ty, core.EventToolResult) != 5 {
		t.Errorf("event types %v", ty)
	}
	if count(ty, core.EventEvidence) != len(rep.Evidence) {
		t.Errorf("evidence events %d vs %d", count(ty, core.EventEvidence), len(rep.Evidence))
	}
}

func TestRunRepairsInvalidReport(t *testing.T) {
	bad := `{"summary":"x","timeline":[{"t":"2026-10-20T14:05:00Z","text":"b"},{"t":"2026-10-20T14:00:00Z","text":"a"}],
"hypotheses":[{"title":"a","explanation":"e","confidence":0.2,"evidence_ids":["$TRIAGE"]},{"title":"b","explanation":"e","confidence":0.9,"evidence_ids":["$TRIAGE"]}]}`
	s := &llm.Scripted{Responses: []llm.Response{
		call("c1", "get_topology", `{}`),
		call("c2", "submit_report", bad),
		call("c3", "submit_report", `{"summary":"no hypotheses","hypotheses":[]}`),
		call("c4", "submit_report", validReport),
	}}
	rec := &recorder{}
	rep, err := New(scenarioDeps(&placeholderClient{Scripted: s}), Config{}).Run(context.Background(), incident(), rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusCompleted {
		t.Fatalf("status %s: %s", rep.Status, rep.Summary)
	}
	reqs := s.Calls()
	fb := reqs[2].Messages[len(reqs[2].Messages)-1].Content
	for _, want := range []string{"Report rejected", "rank hypotheses", "sort the timeline", "Valid evidence IDs"} {
		if !strings.Contains(fb, want) {
			t.Errorf("feedback missing %q:\n%s", want, fb)
		}
	}
	if reqs[2].ToolChoice != tools.ToolSubmitReport || reqs[3].ToolChoice != tools.ToolSubmitReport {
		t.Errorf("repair rounds not forced: %q %q", reqs[2].ToolChoice, reqs[3].ToolChoice)
	}
	rec.check(t, core.StatusCompleted)
}

func TestRunRejectsUnknownEvidence(t *testing.T) {
	unknown := `{"summary":"guess","hypotheses":[{"title":"db","explanation":"e","service":"catalog","confidence":0.7,"evidence_ids":["E999","$TRIAGE"]}]}`
	s := &llm.Scripted{Responses: []llm.Response{
		call("c1", "submit_report", unknown),
		call("c2", "submit_report", unknown),
		call("c3", "submit_report", unknown),
		call("c4", "submit_report", validReport), // never reached
	}}
	rec := &recorder{}
	rep, err := New(scenarioDeps(&placeholderClient{Scripted: s}), Config{}).Run(context.Background(), incident(), rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusInvalidReport {
		t.Fatalf("status %s", rep.Status)
	}
	if s.Remaining() != 1 {
		t.Errorf("remaining %d, want 1 (stop after 2 repairs)", s.Remaining())
	}
	if !strings.Contains(s.Calls()[1].Messages[len(s.Calls()[1].Messages)-1].Content, `"E999" does not exist`) {
		t.Error("feedback does not name unknown evidence")
	}
	for _, h := range rep.Hypotheses {
		for _, id := range h.EvidenceIDs {
			if id == "E999" {
				t.Error("unknown evidence kept in invalid report")
			}
		}
	}
	rec.check(t, core.StatusInvalidReport)
	if count(rec.types(), core.EventError) != 1 {
		t.Errorf("want one error event: %v", rec.types())
	}
}

func TestRunBudgets(t *testing.T) {
	loop := func(n int) []llm.Response {
		var rs []llm.Response
		for i := range n {
			rs = append(rs, call(fmt.Sprintf("c%d", i), "get_topology", `{}`))
		}
		return rs
	}
	tests := []struct {
		name      string
		cfg       Config
		responses []llm.Response
		wantSteps int
		forcedAt  int // request index expected to force submit_report; -1 for none
	}{
		{"steps", Config{MaxSteps: 3}, loop(10), 3, 2},
		{"tokens", Config{MaxTokens: 2500}, loop(10), 3, 2}, // 1100 tokens/step: 2nd step crosses 80%
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &llm.Scripted{Responses: tt.responses}
			rec := &recorder{}
			rep, err := New(scenarioDeps(s), tt.cfg).Run(context.Background(), incident(), rec.sink)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Status != core.StatusBudgetExceeded {
				t.Fatalf("status %s", rep.Status)
			}
			if rep.Usage.Steps != tt.wantSteps {
				t.Errorf("steps %d want %d", rep.Usage.Steps, tt.wantSteps)
			}
			reqs := s.Calls()
			if got := reqs[tt.forcedAt].ToolChoice; got != tools.ToolSubmitReport {
				t.Errorf("request %d tool_choice %q", tt.forcedAt, got)
			}
			if reqs[0].ToolChoice != "auto" {
				t.Errorf("first tool_choice %q", reqs[0].ToolChoice)
			}
			if !strings.Contains(rep.Summary, "budget exhausted") {
				t.Errorf("summary %q", rep.Summary)
			}
			rec.check(t, core.StatusBudgetExceeded)
		})
	}
}

// blockingClient waits for ctx to end.
type blockingClient struct{}

func (blockingClient) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunDurationBudget(t *testing.T) {
	rep, err := New(scenarioDeps(blockingClient{}), Config{MaxDuration: 100 * time.Millisecond}).Run(context.Background(), incident(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusBudgetExceeded {
		t.Errorf("status %s", rep.Status)
	}
}

func TestRunPlainTextNudgedThenForced(t *testing.T) {
	s := &llm.Scripted{Responses: []llm.Response{
		text("I think it's the database."),
		text("Definitely the database."),
		call("c1", "submit_report", strings.ReplaceAll(validReport, "$LAST", "$TRIAGE")),
	}}
	rec := &recorder{}
	rep, err := New(scenarioDeps(&placeholderClient{Scripted: s}), Config{}).Run(context.Background(), incident(), rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusCompleted {
		t.Fatalf("status %s: %s", rep.Status, rep.Summary)
	}
	reqs := s.Calls()
	if m := reqs[1].Messages[len(reqs[1].Messages)-1]; m.Role != "user" || !strings.Contains(m.Content, "submit_report") {
		t.Errorf("no nudge: %+v", m)
	}
	if reqs[1].ToolChoice != "auto" || reqs[2].ToolChoice != tools.ToolSubmitReport {
		t.Errorf("tool choices %q %q", reqs[1].ToolChoice, reqs[2].ToolChoice)
	}
	if count(rec.types(), core.EventThinking) != 2 {
		t.Errorf("thinking events: %v", rec.types())
	}
}

func TestRunNilSources(t *testing.T) {
	s := &llm.Scripted{Responses: []llm.Response{
		call("c1", "list_metrics", `{}`),
		call("c2", "search_logs", `{"service":"frontend"}`),
		call("c3", "list_changes", `{}`),
		call("c4", "get_topology", `{}`),
		call("c5", "submit_report", `{"summary":"Only topology is known.","hypotheses":[{"title":"catalog","explanation":"frontend depends on catalog","service":"catalog","confidence":0.2,"evidence_ids":["$LAST"]}]}`),
	}}
	deps := Deps{LLM: &placeholderClient{Scripted: s}, Topology: core.Topology{"frontend": {"catalog"}}}
	rep, err := New(deps, Config{}).Run(context.Background(), incident(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != core.StatusCompleted {
		t.Fatalf("status %s: %s", rep.Status, rep.Summary)
	}
	reqs := s.Calls()
	for i := 1; i <= 3; i++ {
		if c := reqs[i].Messages[len(reqs[i].Messages)-1].Content; !strings.Contains(c, "not configured") && !strings.Contains(c, "No ") {
			t.Errorf("request %d tool result %q", i, c)
		}
	}
	if !strings.Contains(reqs[0].Messages[1].Content, "no metrics source configured") {
		t.Error("triage summary should mention missing sources")
	}
}

func TestRunNoLLM(t *testing.T) {
	rec := &recorder{}
	rep, err := New(Deps{}, Config{}).Run(context.Background(), incident(), rec.sink)
	if err == nil || rep == nil || rep.Status != core.StatusFailed {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	rec.check(t, core.StatusFailed)
}

func TestRunContextCancel(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := &recorder{}
		rep, err := New(scenarioDeps(&llm.Scripted{}), Config{}).Run(ctx, incident(), rec.sink)
		if !errors.Is(err, context.Canceled) || rep.Status != core.StatusFailed {
			t.Fatalf("rep %v err %v", rep.Status, err)
		}
		rec.check(t, core.StatusFailed)
	})
	t.Run("mid loop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := &llm.Scripted{Responses: []llm.Response{
			call("c1", "get_topology", `{}`),
			call("c2", "get_topology", `{}`),
			call("c3", "submit_report", validReport),
		}}
		pc := &placeholderClient{Scripted: s, onCall: func(n int) {
			if n == 2 {
				cancel()
			}
		}}
		rec := &recorder{}
		rep, err := New(scenarioDeps(pc), Config{}).Run(ctx, incident(), rec.sink)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if rep == nil || rep.Status != core.StatusFailed || rep.Usage.Steps != 1 {
			t.Fatalf("rep %+v", rep)
		}
		if len(rep.Evidence) == 0 {
			t.Error("failed report should keep evidence gathered so far")
		}
		rec.check(t, core.StatusFailed)
	})
}

// statelessClient answers from the request alone so one instance can serve
// concurrent runs: it calls get_topology, then submits a report citing it.
type statelessClient struct{}

func (statelessClient) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	for _, m := range req.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "evidence_id: ") {
			id := strings.TrimPrefix(strings.SplitN(m.Content, "\n", 2)[0], "evidence_id: ")
			r := call("s", "submit_report", fmt.Sprintf(`{"summary":"s","hypotheses":[{"title":"t","explanation":"e","confidence":0.5,"evidence_ids":[%q]}]}`, id))
			return &r, nil
		}
	}
	r := call("t", "get_topology", `{}`)
	return &r, nil
}

func TestRunConcurrent(t *testing.T) {
	inv := New(scenarioDeps(statelessClient{}), Config{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inc := incident()
			inc.ID = fmt.Sprintf("inc-%d", i)
			rec := &recorder{}
			rep, err := inv.Run(context.Background(), inc, rec.sink)
			if err != nil {
				errs <- err
				return
			}
			if rep.Status != core.StatusCompleted || rep.IncidentID != inc.ID {
				errs <- fmt.Errorf("run %d: status %s id %s", i, rep.Status, rep.IncidentID)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestValidate(t *testing.T) {
	has := func(id string) bool { return id == "E1" || id == "E2" }
	ts := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	ok := submission{
		Summary:    "s",
		Hypotheses: []core.Hypothesis{{Title: "a", Confidence: 0.8, EvidenceIDs: []string{"E1"}}, {Title: "b", Confidence: 0.8, EvidenceIDs: []string{"E2"}}},
		Timeline:   []core.TimelineEntry{{T: ts("2026-10-20T14:00:00Z"), Text: "x"}, {T: ts("2026-10-20T14:00:00Z"), Text: "y", EvidenceIDs: []string{"E1"}}},
	}
	tests := []struct {
		name string
		mut  func(*submission)
		want string
	}{
		{"valid", func(*submission) {}, ""},
		{"no hypotheses", func(s *submission) { s.Hypotheses = nil }, "at least one hypothesis"},
		{"empty summary", func(s *submission) { s.Summary = " " }, "summary is empty"},
		{"confidence high", func(s *submission) { s.Hypotheses[0].Confidence = 1.5 }, "outside [0,1]"},
		{"confidence negative", func(s *submission) { s.Hypotheses[1].Confidence = -0.1 }, "outside [0,1]"},
		{"unranked", func(s *submission) { s.Hypotheses[1].Confidence = 0.9 }, "rank hypotheses"},
		{"no citations", func(s *submission) { s.Hypotheses[0].EvidenceIDs = nil }, "cites no evidence"},
		{"unknown citation", func(s *submission) { s.Hypotheses[0].EvidenceIDs = []string{"E9"} }, `"E9" does not exist`},
		{"timeline unknown", func(s *submission) { s.Timeline[1].EvidenceIDs = []string{"E3"} }, `timeline entry 2: evidence "E3"`},
		{"timeline unsorted", func(s *submission) { s.Timeline[1].T = ts("2026-10-20T13:00:00Z") }, "sort the timeline"},
		{"no title", func(s *submission) { s.Hypotheses[0].Title = "" }, "title is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ok
			s.Hypotheses = append([]core.Hypothesis(nil), ok.Hypotheses...)
			s.Timeline = append([]core.TimelineEntry(nil), ok.Timeline...)
			tt.mut(&s)
			errs := validate(s, has)
			if tt.want == "" {
				if len(errs) != 0 {
					t.Errorf("unexpected errors %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), tt.want) {
				t.Errorf("errors %v, want %q", errs, tt.want)
			}
		})
	}
	if _, err := parseSubmission(json.RawMessage(`{"hypotheses":"nope"}`)); err == nil {
		t.Error("bad submission parsed")
	}
}
