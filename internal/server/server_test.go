package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/config"
	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/store"
)

// fakeInvestigator runs fn, or emits a short canned investigation.
type fakeInvestigator struct {
	fn func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error)
}

func (f *fakeInvestigator) Run(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
	if f.fn != nil {
		return f.fn(ctx, inc, sink)
	}
	emit(sink, core.EventStarted, inc)
	emit(sink, core.EventToolCall, map[string]any{"id": "c1", "name": "get_metric"})
	return goodReport(inc), nil
}

func emit(sink core.EventSink, typ string, data any) {
	b, _ := json.Marshal(data)
	sink(core.Event{Type: typ, At: time.Now(), Data: b})
}

func goodReport(inc core.Incident) *core.Report {
	return &core.Report{
		IncidentID: inc.ID,
		Status:     core.StatusCompleted,
		Summary:    "catalog deploy added an N+1 query",
		Hypotheses: []core.Hypothesis{{Title: "Bad catalog deploy", Confidence: 0.82, EvidenceIDs: []string{"E1"}}},
	}
}

type testEnv struct {
	srv   *Server
	store *store.Store
	http  *httptest.Server
	now   time.Time
}

func newEnv(t *testing.T, inv Investigator, mutate func(*Options)) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	return newEnvWithStore(t, st, inv, mutate)
}

func newEnvWithStore(t *testing.T, st *store.Store, inv Investigator, mutate func(*Options)) *testEnv {
	t.Helper()
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	opts := Options{
		Store:        st,
		Investigator: inv,
		Config:       config.Default(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          func() time.Time { return now },
		Heartbeat:    50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		hs.Close()
		st.Close()
	})
	return &testEnv{srv: srv, store: st, http: hs, now: now}
}

func (e *testEnv) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *testEnv) create(t *testing.T, body string) string {
	t.Helper()
	resp, b := e.do(t, "POST", "/api/investigations", body, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		t.Fatalf("create response %s: %v", b, err)
	}
	return out.ID
}

// waitStatus polls until id has a status other than queued/running.
func (e *testEnv) waitDone(t *testing.T, id string) *store.Investigation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		inv, err := e.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if inv.Status != core.StatusQueued && inv.Status != core.StatusRunning {
			// The finished event is written right after the final status.
			evs, _ := e.store.Events(context.Background(), id, 0)
			if len(evs) > 0 && evs[len(evs)-1].Type == core.EventFinished {
				return inv
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("investigation %s did not finish", id)
	return nil
}

const validBody = `{"title":"High p99 latency","service":"frontend","started_at":"2026-10-20T14:05:00Z"}`

func eventTypes(evs []core.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestCreateEventsReportFlow(t *testing.T) {
	e := newEnv(t, &fakeInvestigator{}, nil)
	id := e.create(t, validBody)
	inv := e.waitDone(t, id)
	if inv.Status != core.StatusCompleted || inv.Report == nil || inv.Report.Hypotheses[0].Title != "Bad catalog deploy" {
		t.Fatalf("investigation = %+v", inv)
	}

	resp, b := e.do(t, "GET", "/api/investigations/"+id, "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d %s", resp.StatusCode, b)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	for _, k := range []string{"id", "incident", "status", "report", "created_at", "updated_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("GET response lacks %q: %s", k, b)
		}
	}
	inc := got["incident"].(map[string]any)
	if inc["source"] != "manual" || inc["id"] != id {
		t.Errorf("incident = %v", inc)
	}
	win := inc["window"].(map[string]any)
	if win["start"] != "2026-10-20T13:05:00Z" || win["end"] != "2026-10-20T14:35:00Z" {
		t.Errorf("default window = %v", win)
	}

	evs, _ := e.store.Events(context.Background(), id, 0)
	want := []string{core.EventStarted, core.EventToolCall, core.EventReport, core.EventFinished}
	if fmt.Sprint(eventTypes(evs)) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", eventTypes(evs), want)
	}

	resp, b = e.do(t, "GET", "/api/investigations", "", nil)
	var list []store.InvestigationSummary
	if err := json.Unmarshal(b, &list); err != nil || resp.StatusCode != 200 || len(list) != 1 {
		t.Fatalf("list: %d %s", resp.StatusCode, b)
	}
	if list[0].TopHypothesis != "Bad catalog deploy" || list[0].Confidence == nil || *list[0].Confidence != 0.82 {
		t.Errorf("summary = %+v", list[0])
	}

	resp, _ = e.do(t, "GET", "/api/investigations/inv_nope", "", nil)
	if resp.StatusCode != 404 {
		t.Errorf("missing investigation status = %d", resp.StatusCode)
	}
	resp, b = e.do(t, "GET", "/api/health", "", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"ok":true}` {
		t.Errorf("health = %d %s", resp.StatusCode, b)
	}
}

func TestRunOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		fn         func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error)
		wantStatus string
		wantError  string
		wantEvents []string
	}{
		{
			name: "error without report",
			fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
				return nil, errors.New("llm unavailable")
			},
			wantStatus: core.StatusFailed, wantError: "llm unavailable",
			wantEvents: []string{core.EventError, core.EventFinished},
		},
		{
			name: "invalid report with error, agent emits its own events",
			fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
				r := goodReport(inc)
				r.Status = core.StatusInvalidReport
				emit(sink, core.EventReport, r)
				emit(sink, core.EventError, map[string]string{"message": "bad citations"})
				emit(sink, core.EventFinished, map[string]string{"status": r.Status})
				return r, errors.New("bad citations")
			},
			wantStatus: core.StatusInvalidReport, wantError: "bad citations",
			wantEvents: []string{core.EventReport, core.EventError, core.EventFinished},
		},
		{
			name: "panic",
			fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
				panic("kaboom")
			},
			wantStatus: core.StatusFailed, wantError: "investigator panic: kaboom",
			wantEvents: []string{core.EventError, core.EventFinished},
		},
		{
			name: "nil report and nil error",
			fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
				return nil, nil
			},
			wantStatus: core.StatusFailed, wantError: "investigator returned no report",
			wantEvents: []string{core.EventError, core.EventFinished},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, &fakeInvestigator{fn: tt.fn}, nil)
			id := e.create(t, validBody)
			inv := e.waitDone(t, id)
			if inv.Status != tt.wantStatus || inv.Error != tt.wantError {
				t.Fatalf("status=%q error=%q", inv.Status, inv.Error)
			}
			evs, _ := e.store.Events(context.Background(), id, 0)
			if fmt.Sprint(eventTypes(evs)) != fmt.Sprint(tt.wantEvents) {
				t.Fatalf("events = %v, want %v", eventTypes(evs), tt.wantEvents)
			}
			var fin map[string]string
			json.Unmarshal(evs[len(evs)-1].Data, &fin)
			if fin["status"] != tt.wantStatus {
				t.Fatalf("finished status = %q", fin["status"])
			}
		})
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, &fakeInvestigator{}, nil)
	tests := []struct {
		name, body string
		wantStatus int
		wantErr    string
	}{
		{"bad json", `{`, 400, "invalid JSON"},
		{"missing title", `{"started_at":"2026-10-20T14:05:00Z"}`, 400, "title is required"},
		{"blank title", `{"title":"  ","started_at":"2026-10-20T14:05:00Z"}`, 400, "title is required"},
		{"missing started_at", `{"title":"x"}`, 400, "started_at is required"},
		{"inverted window", `{"title":"x","started_at":"2026-10-20T14:05:00Z","window":{"start":"2026-10-20T15:00:00Z","end":"2026-10-20T14:00:00Z"}}`, 400, "window.end"},
		{"bad id", `{"id":"a b","title":"x","started_at":"2026-10-20T14:05:00Z"}`, 400, "id must be"},
		{"too large", `{"title":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 413, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, b := e.do(t, "POST", "/api/investigations", tt.body, nil)
			var out map[string]string
			json.Unmarshal(b, &out)
			if resp.StatusCode != tt.wantStatus || !strings.Contains(out["error"], tt.wantErr) {
				t.Fatalf("got %d %s, want %d containing %q", resp.StatusCode, b, tt.wantStatus, tt.wantErr)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
		})
	}

	t.Run("explicit id and conflict", func(t *testing.T) {
		body := `{"id":"my-incident-1","title":"x","started_at":"2026-10-20T14:05:00Z"}`
		if id := e.create(t, body); id != "my-incident-1" {
			t.Fatalf("id = %q", id)
		}
		resp, _ := e.do(t, "POST", "/api/investigations", body, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("duplicate id status = %d", resp.StatusCode)
		}
	})

	t.Run("window end clamped to now", func(t *testing.T) {
		// Started 10 minutes before the fake now: end is now, not start+30m.
		id := e.create(t, `{"title":"recent","started_at":"2026-10-20T14:50:00Z"}`)
		inv, err := e.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !inv.Incident.Window.End.Equal(e.now) {
			t.Fatalf("window end = %v, want %v", inv.Incident.Window.End, e.now)
		}
	})
}

// sseEvent is one parsed SSE message.
type sseEvent struct {
	id, event string
	data      core.Event
}

// readSSE reads events until the stream ends or n events arrive (n<0: all).
func readSSE(t *testing.T, body io.Reader, n int, out chan<- sseEvent) {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cur sseEvent
	count := 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				out <- cur
				count++
				cur = sseEvent{}
				if n >= 0 && count >= n {
					return
				}
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Errorf("bad data line %q: %v", line, err)
			}
		}
	}
}

func openSSE(t *testing.T, e *testEnv, id string, hdr map[string]string, query string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.http.URL+"/api/investigations/"+id+"/events"+query, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("sse status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return resp
}

func TestSSEReplayLiveAndResume(t *testing.T) {
	gate := make(chan struct{})
	inv := &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		emit(sink, core.EventStarted, inc)
		emit(sink, core.EventTriage, map[string]any{"summary": "frontend p99 up", "evidence_ids": []string{"E1"}})
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		emit(sink, core.EventToolCall, map[string]any{"id": "c1", "name": "get_metric"})
		emit(sink, core.EventToolResult, map[string]any{"id": "c1", "evidence_id": "E2"})
		return goodReport(inc), nil
	}}
	e := newEnv(t, inv, nil)
	id := e.create(t, validBody)

	// Wait until the first two events are stored, then connect: they replay.
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, _ := e.store.Events(context.Background(), id, 0)
		if len(evs) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("events not stored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp := openSSE(t, e, id, nil, "")
	defer resp.Body.Close()
	got := make(chan sseEvent, 32)
	done := make(chan struct{})
	go func() { readSSE(t, resp.Body, -1, got); close(done) }()

	for i := 1; i <= 2; i++ {
		ev := <-got
		if ev.id != strconv.Itoa(i) || ev.data.Seq != i {
			t.Fatalf("replayed event %d = %+v", i, ev)
		}
	}
	close(gate)
	var live []string
	for ev := range got {
		live = append(live, ev.event)
		if ev.event == core.EventFinished {
			break
		}
	}
	want := []string{core.EventToolCall, core.EventToolResult, core.EventReport, core.EventFinished}
	if fmt.Sprint(live) != fmt.Sprint(want) {
		t.Fatalf("live events = %v, want %v", live, want)
	}
	select {
	case <-done: // server closed the stream after finished
	case <-time.After(2 * time.Second):
		t.Fatal("stream not closed after finished")
	}

	tests := []struct {
		name     string
		hdr      map[string]string
		query    string
		wantSeqs string
	}{
		{"Last-Event-ID", map[string]string{"Last-Event-ID": "4"}, "", "[5 6]"},
		{"after query", nil, "?after=2", "[3 4 5 6]"},
		{"header wins", map[string]string{"Last-Event-ID": "5"}, "?after=1", "[6]"},
		{"full replay", nil, "", "[1 2 3 4 5 6]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := openSSE(t, e, id, tt.hdr, tt.query)
			defer resp.Body.Close()
			ch := make(chan sseEvent, 32)
			readSSE(t, resp.Body, -1, ch) // returns when the server closes
			close(ch)
			var seqs []int
			for ev := range ch {
				seqs = append(seqs, ev.data.Seq)
			}
			if fmt.Sprint(seqs) != tt.wantSeqs {
				t.Fatalf("seqs = %v, want %s", seqs, tt.wantSeqs)
			}
		})
	}

	resp2, _ := e.do(t, "GET", "/api/investigations/"+id+"/events", "", map[string]string{"Last-Event-ID": "x"})
	if resp2.StatusCode != 400 {
		t.Fatalf("bad Last-Event-ID status = %d", resp2.StatusCode)
	}
}

func TestSSEHeartbeatAndDisconnect(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return goodReport(inc), nil
	}}, nil)
	defer close(release)
	id := e.create(t, validBody)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.http.URL+"/api/investigations/"+id+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil || line != ": heartbeat\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	if n := e.srv.broker.count(id); n != 1 {
		t.Fatalf("subscribers = %d, want 1", n)
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for e.srv.broker.count(id) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber not removed after client disconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSEResyncAfterDrop(t *testing.T) {
	const n = 50
	gate := make(chan struct{})
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		<-gate
		for i := 0; i < n; i++ {
			emit(sink, core.EventThinking, map[string]string{"text": strconv.Itoa(i)})
		}
		return goodReport(inc), nil
	}}, func(o *Options) { o.SubscriberBuffer = 1 })
	id := e.create(t, validBody)
	resp := openSSE(t, e, id, nil, "")
	defer resp.Body.Close()
	close(gate)
	ch := make(chan sseEvent, n+10)
	readSSE(t, resp.Body, -1, ch)
	close(ch)
	var seqs []int
	for ev := range ch {
		seqs = append(seqs, ev.data.Seq)
	}
	if len(seqs) != n+2 {
		t.Fatalf("got %d events, want %d: %v", len(seqs), n+2, seqs)
	}
	for i, q := range seqs {
		if q != i+1 {
			t.Fatalf("seqs out of order or missing: %v", seqs)
		}
	}
}

func TestBrokerDropSignalsLag(t *testing.T) {
	b := newBroker(1)
	sub, unsub := b.subscribe("a")
	defer unsub()
	b.publish("a", core.Event{Seq: 1})
	b.publish("a", core.Event{Seq: 2}) // dropped
	b.publish("b", core.Event{Seq: 1}) // no subscribers
	if ev := <-sub.ch; ev.Seq != 1 {
		t.Fatalf("got seq %d", ev.Seq)
	}
	select {
	case <-sub.lagged:
	default:
		t.Fatal("lag not signalled")
	}
}

func alertPayload(alerts ...string) string {
	return `{"version":"4","status":"firing","receiver":"culprit","alerts":[` + strings.Join(alerts, ",") + `]}`
}

func TestAlertmanagerWebhook(t *testing.T) {
	e := newEnv(t, &fakeInvestigator{}, nil)
	firing := `{"status":"firing","labels":{"alertname":"HighLatency","service":"frontend","severity":"page"},
		"annotations":{"summary":"Frontend p99 above 1s"},"startsAt":"2026-10-20T14:55:00Z","fingerprint":"fp-1"}`
	noSummary := `{"status":"firing","labels":{"alertname":"ErrorRate","job":"checkout"},"startsAt":"2026-10-20T14:58:00Z"}`
	resolved := `{"status":"resolved","labels":{"alertname":"Old"},"fingerprint":"fp-old"}`

	var res webhookResult
	resp, b := e.do(t, "POST", "/api/webhooks/alertmanager", alertPayload(firing, noSummary, resolved), nil)
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(b, &res) != nil {
		t.Fatalf("webhook: %d %s", resp.StatusCode, b)
	}
	if len(res.Created) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("first delivery = %+v", res)
	}

	tests := []struct {
		id                    string
		title, service, start string
		wantLabel, wantLabelV string
	}{
		{res.Created[0], "Frontend p99 above 1s", "frontend", "2026-10-20T14:55:00Z", "severity", "page"},
		{res.Created[1], "ErrorRate", "checkout", "2026-10-20T14:58:00Z", "job", "checkout"},
	}
	for _, tt := range tests {
		inv, err := e.store.Get(context.Background(), tt.id)
		if err != nil {
			t.Fatal(err)
		}
		inc := inv.Incident
		if inc.Title != tt.title || inc.Service != tt.service || inc.StartedAt.Format(time.RFC3339) != tt.start ||
			inc.Source != "alertmanager" || inc.Labels[tt.wantLabel] != tt.wantLabelV {
			t.Errorf("incident = %+v", inc)
		}
		if !inc.Window.Start.Equal(inc.StartedAt.Add(-time.Hour)) || !inc.Window.End.Equal(e.now) {
			t.Errorf("window = %+v", inc.Window)
		}
	}

	// Redelivery within 30 minutes is deduplicated, including the alert that
	// had no fingerprint (hashed from labels).
	resp, b = e.do(t, "POST", "/api/webhooks/alertmanager", alertPayload(firing, noSummary), nil)
	res = webhookResult{}
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(b, &res) != nil {
		t.Fatalf("webhook: %d %s", resp.StatusCode, b)
	}
	if len(res.Created) != 0 || len(res.Skipped) != 2 {
		t.Fatalf("redelivery = %+v", res)
	}

	resp, _ = e.do(t, "POST", "/api/webhooks/alertmanager", `nope`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("bad payload status = %d", resp.StatusCode)
	}
}

func TestAlertmanagerDedupeExpires(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := &store.Investigation{
		Incident:    core.Incident{Title: "old", StartedAt: time.Now()},
		Status:      core.StatusCompleted,
		Fingerprint: "fp-1",
		CreatedAt:   time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC), // 60m before fake now
	}
	if err := st.Create(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	e := newEnvWithStore(t, st, &fakeInvestigator{}, nil)
	resp, b := e.do(t, "POST", "/api/webhooks/alertmanager",
		alertPayload(`{"status":"firing","labels":{"alertname":"A"},"fingerprint":"fp-1"}`), nil)
	var res webhookResult
	json.Unmarshal(b, &res)
	if resp.StatusCode != http.StatusAccepted || len(res.Created) != 1 {
		t.Fatalf("expired dedupe: %d %s", resp.StatusCode, b)
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t, &fakeInvestigator{}, nil)
	origin := config.Default().Server.CORSOrigin
	tests := []struct {
		name, method, path, origin string
		preflight                  bool
		wantStatus                 int
		wantAllow                  string
	}{
		{"allowed preflight", "OPTIONS", "/api/investigations", origin, true, 204, origin},
		{"other origin preflight", "OPTIONS", "/api/investigations", "http://evil.example", true, 204, ""},
		{"allowed GET", "GET", "/api/health", origin, false, 200, origin},
		{"other origin GET", "GET", "/api/health", "http://evil.example", false, 200, ""},
		{"no origin", "GET", "/api/health", "", false, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.origin != "" {
				hdr["Origin"] = tt.origin
			}
			if tt.preflight {
				hdr["Access-Control-Request-Method"] = "POST"
				hdr["Access-Control-Request-Headers"] = "content-type"
			}
			resp, _ := e.do(t, tt.method, tt.path, "", hdr)
			if resp.StatusCode != tt.wantStatus || resp.Header.Get("Access-Control-Allow-Origin") != tt.wantAllow {
				t.Fatalf("status %d allow-origin %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
			}
			if tt.preflight && tt.wantAllow != "" && !strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "POST") {
				t.Fatalf("allow-methods %q", resp.Header.Get("Access-Control-Allow-Methods"))
			}
		})
	}
}

func TestBoundedConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return goodReport(inc), nil
	}}, func(o *Options) { o.Config.Server.Concurrency = 2 })

	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, e.create(t, validBody))
	}
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("workers did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
	queued := 0
	for _, id := range ids {
		inv, _ := e.store.Get(context.Background(), id)
		if inv.Status == core.StatusQueued {
			queued++
		}
	}
	if queued != 3 {
		t.Fatalf("queued = %d, want 3", queued)
	}
	close(release)
	for _, id := range ids {
		if inv := e.waitDone(t, id); inv.Status != core.StatusCompleted {
			t.Fatalf("%s status %s", id, inv.Status)
		}
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

func TestQueueFull(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return goodReport(inc), nil
	}}, func(o *Options) { o.Config.Server.Concurrency = 1; o.QueueSize = 1 })
	defer close(release)
	e.create(t, validBody) // running (eventually)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ := e.store.List(context.Background(), 10, 0)
		if list[0].Status == core.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first investigation did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.create(t, validBody) // queued
	resp, b := e.do(t, "POST", "/api/investigations", validBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(b, []byte("queue is full")) {
		t.Fatalf("third create: %d %s", resp.StatusCode, b)
	}
}

func TestShutdownCancelsAfterGrace(t *testing.T) {
	var sawCancel atomic.Bool
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		<-ctx.Done()
		sawCancel.Store(true)
		return nil, fmt.Errorf("agent: %w", ctx.Err())
	}}, func(o *Options) { o.Config.Server.Concurrency = 1 })
	running := e.create(t, validBody)
	queued := e.create(t, validBody)
	deadline := time.Now().Add(5 * time.Second)
	for {
		inv, _ := e.store.Get(context.Background(), running)
		if inv.Status == core.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("investigation did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// An open SSE stream must not block shutdown.
	sse := openSSE(t, e, running, nil, "")
	defer sse.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second || !sawCancel.Load() {
		t.Fatalf("shutdown took %v, cancelled=%v", time.Since(start), sawCancel.Load())
	}
	inv, _ := e.store.Get(context.Background(), running)
	if inv.Status != core.StatusFailed || !strings.Contains(inv.Error, "context canceled") {
		t.Fatalf("running investigation after shutdown = %s %q", inv.Status, inv.Error)
	}
	inv, _ = e.store.Get(context.Background(), queued)
	if inv.Status != core.StatusQueued {
		t.Fatalf("queued investigation after shutdown = %s", inv.Status)
	}
	resp, _ := e.do(t, "POST", "/api/investigations", validBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("create after shutdown status = %d", resp.StatusCode)
	}
	io.Copy(io.Discard, sse.Body) // stream ends
}

func TestShutdownGraceful(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, &fakeInvestigator{fn: func(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
		<-release
		return goodReport(inc), nil
	}}, nil)
	id := e.create(t, validBody)
	for {
		inv, _ := e.store.Get(context.Background(), id)
		if inv.Status == core.StatusRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = e.srv.Shutdown(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if inv, _ := e.store.Get(context.Background(), id); inv.Status != core.StatusCompleted {
		t.Fatalf("status after graceful shutdown = %s", inv.Status)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var ids []string
	for _, status := range []string{core.StatusQueued, core.StatusRunning, core.StatusCompleted} {
		inv := &store.Investigation{Incident: core.Incident{Title: status, StartedAt: time.Now()}, Status: status}
		if err := st.Create(ctx, inv); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inv.ID)
	}
	e := newEnvWithStore(t, st, &fakeInvestigator{}, nil)
	for i, id := range ids {
		inv, _ := e.store.Get(ctx, id)
		if i < 2 {
			if inv.Status != core.StatusFailed || inv.Error != "interrupted by restart" {
				t.Errorf("%s: status %s error %q", id, inv.Status, inv.Error)
			}
			// A stream for it terminates instead of waiting forever.
			resp := openSSE(t, e, id, nil, "")
			ch := make(chan sseEvent, 8)
			readSSE(t, resp.Body, -1, ch)
			resp.Body.Close()
			close(ch)
			var types []string
			for ev := range ch {
				types = append(types, ev.event)
			}
			if fmt.Sprint(types) != "[error finished]" {
				t.Errorf("%s: events %v", id, types)
			}
		} else if inv.Status != core.StatusCompleted {
			t.Errorf("%s: completed investigation changed to %s", id, inv.Status)
		}
	}
}

func TestPanicRecoveryMiddleware(t *testing.T) {
	e := newEnv(t, &fakeInvestigator{}, nil)
	h := e.srv.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("panic response %d %s", rec.Code, rec.Body)
	}
}
