package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testIncident(title string) core.Incident {
	start := time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)
	inc := core.NewIncident(title, "frontend", start, time.Hour, 30*time.Minute)
	inc.Source = "manual"
	inc.Labels = map[string]string{"severity": "page"}
	return inc
}

func TestNewIDSortable(t *testing.T) {
	seen := map[string]bool{}
	var ids []string
	for i := 0; i < 50; i++ {
		id := NewID()
		if !strings.HasPrefix(id, "inv_") || len(id) != 30 {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
		if i%10 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	first, last := ids[0], ids[len(ids)-1]
	if first >= last {
		t.Fatalf("ids not time sortable: %s >= %s", first, last)
	}
}

func TestOpenIdempotentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("schema_version rows = %d, want 1", n)
		}
		var mode string
		if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("journal_mode = %q, want wal", mode)
		}
		s.Close()
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	inv := &Investigation{Incident: testIncident("High p99"), Fingerprint: "fp1"}
	if err := s.Create(ctx, inv); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inv.ID == "" || inv.Status != core.StatusQueued || inv.Incident.ID != inv.ID {
		t.Fatalf("Create did not fill fields: %+v", inv)
	}
	got, err := s.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Incident.Title != "High p99" || !got.Incident.StartedAt.Equal(inv.Incident.StartedAt) ||
		got.Incident.Labels["severity"] != "page" || got.Fingerprint != "fp1" || got.Report != nil {
		t.Fatalf("Get = %+v", got)
	}
	if err := s.Create(ctx, &Investigation{ID: inv.ID, Incident: testIncident("dup")}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
	if _, err := s.Get(ctx, "inv_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestStatusReportError(t *testing.T) {
	ctx := context.Background()
	rep := &core.Report{
		Status:     core.StatusInvalidReport,
		Summary:    "catalog N+1",
		Hypotheses: []core.Hypothesis{{Title: "Bad catalog deploy", Confidence: 0.8, EvidenceIDs: []string{"E1"}}},
	}
	tests := []struct {
		name       string
		apply      func(s *Store, id string) error
		wantStatus string
		wantError  string
		wantReport bool
	}{
		{"running", func(s *Store, id string) error { return s.UpdateStatus(ctx, id, core.StatusRunning) }, core.StatusRunning, "", false},
		{"report sets status", func(s *Store, id string) error { return s.SaveReport(ctx, id, rep) }, core.StatusInvalidReport, "", true},
		{"empty report status is completed", func(s *Store, id string) error {
			return s.SaveReport(ctx, id, &core.Report{Summary: "x"})
		}, core.StatusCompleted, "", true},
		{"error fails running", func(s *Store, id string) error {
			if err := s.UpdateStatus(ctx, id, core.StatusRunning); err != nil {
				return err
			}
			return s.SetError(ctx, id, "boom")
		}, core.StatusFailed, "boom", false},
		{"error keeps report status", func(s *Store, id string) error {
			if err := s.SaveReport(ctx, id, rep); err != nil {
				return err
			}
			return s.SetError(ctx, id, "repair failed")
		}, core.StatusInvalidReport, "repair failed", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTest(t)
			inv := &Investigation{Incident: testIncident(tt.name)}
			if err := s.Create(ctx, inv); err != nil {
				t.Fatal(err)
			}
			if err := tt.apply(s, inv.ID); err != nil {
				t.Fatalf("apply: %v", err)
			}
			got, err := s.Get(ctx, inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus || got.Error != tt.wantError || (got.Report != nil) != tt.wantReport {
				t.Fatalf("got status=%q error=%q report=%v", got.Status, got.Error, got.Report != nil)
			}
			if got.UpdatedAt.Before(got.CreatedAt) {
				t.Fatalf("updated_at before created_at")
			}
		})
	}
	s := openTest(t)
	for name, err := range map[string]error{
		"UpdateStatus": s.UpdateStatus(ctx, "nope", core.StatusRunning),
		"SaveReport":   s.SaveReport(ctx, "nope", rep),
		"SetError":     s.SetError(ctx, "nope", "x"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on missing id: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestListNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 5; i++ {
		inv := &Investigation{Incident: testIncident(fmt.Sprintf("inc %d", i)), CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := s.Create(ctx, inv); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inv.ID)
	}
	rep := &core.Report{Hypotheses: []core.Hypothesis{{Title: "Top", Confidence: 0.7}, {Title: "Second", Confidence: 0.2}}}
	if err := s.SaveReport(ctx, ids[3], rep); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		limit, offset int
		want          []string
	}{
		{0, 0, []string{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{2, 0, []string{ids[4], ids[3]}},
		{2, 3, []string{ids[1], ids[0]}},
		{10, 10, nil},
	}
	for _, tt := range tests {
		got, err := s.List(ctx, tt.limit, tt.offset)
		if err != nil {
			t.Fatal(err)
		}
		var gotIDs []string
		for _, g := range got {
			gotIDs = append(gotIDs, g.ID)
		}
		if fmt.Sprint(gotIDs) != fmt.Sprint(tt.want) {
			t.Errorf("List(%d,%d) = %v, want %v", tt.limit, tt.offset, gotIDs, tt.want)
		}
	}

	all, err := s.List(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if all[1].TopHypothesis != "Top" || all[1].Confidence == nil || *all[1].Confidence != 0.7 || all[1].Status != core.StatusCompleted {
		t.Fatalf("summary with report = %+v", all[1])
	}
	if all[0].TopHypothesis != "" || all[0].Confidence != nil || all[0].Title != "inc 4" || all[0].Service != "frontend" {
		t.Fatalf("summary without report = %+v", all[0])
	}
	b, _ := json.Marshal(all[1])
	for _, key := range []string{`"top_hypothesis"`, `"confidence"`, `"created_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("summary JSON %s lacks %s", b, key)
		}
	}
}

func TestEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	inv := &Investigation{Incident: testIncident("ev")}
	if err := s.Create(ctx, inv); err != nil {
		t.Fatal(err)
	}
	types := []string{core.EventStarted, core.EventTriage, core.EventToolCall, core.EventFinished}
	for i, typ := range types {
		ev, err := s.AppendEvent(ctx, inv.ID, core.Event{Seq: 99, Type: typ, Data: json.RawMessage(`{"i":` + fmt.Sprint(i) + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		if ev.Seq != i+1 || ev.At.IsZero() {
			t.Fatalf("AppendEvent #%d = %+v", i, ev)
		}
	}
	if ev, err := s.AppendEvent(ctx, inv.ID, core.Event{Type: core.EventThinking}); err != nil || string(ev.Data) != "null" {
		t.Fatalf("nil data event = %+v, %v", ev, err)
	}
	if _, err := s.AppendEvent(ctx, "inv_missing", core.Event{Type: core.EventStarted}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to missing investigation err = %v, want ErrNotFound", err)
	}

	tests := []struct {
		after    int
		wantSeqs []int
	}{
		{0, []int{1, 2, 3, 4, 5}},
		{2, []int{3, 4, 5}},
		{5, nil},
	}
	for _, tt := range tests {
		evs, err := s.Events(ctx, inv.ID, tt.after)
		if err != nil {
			t.Fatal(err)
		}
		var seqs []int
		for _, e := range evs {
			seqs = append(seqs, e.Seq)
		}
		if fmt.Sprint(seqs) != fmt.Sprint(tt.wantSeqs) {
			t.Errorf("Events(after=%d) seqs = %v, want %v", tt.after, seqs, tt.wantSeqs)
		}
	}
	evs, _ := s.Events(ctx, inv.ID, 0)
	if evs[1].Type != core.EventTriage || string(evs[1].Data) != `{"i":1}` {
		t.Fatalf("event 2 = %+v", evs[1])
	}
}

func TestAppendEventConcurrent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	inv := &Investigation{Incident: testIncident("conc")}
	if err := s.Create(ctx, inv); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	seqs := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := s.AppendEvent(ctx, inv.ID, core.Event{Type: core.EventThinking})
			if err != nil {
				t.Error(err)
				return
			}
			seqs <- ev.Seq
		}()
	}
	wg.Wait()
	close(seqs)
	var got []int
	for q := range seqs {
		got = append(got, q)
	}
	sort.Ints(got)
	for i, q := range got {
		if q != i+1 {
			t.Fatalf("seqs not unique/contiguous: %v", got)
		}
	}
}

func TestFindRecentByFingerprint(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old := &Investigation{Incident: testIncident("old"), Fingerprint: "abc", CreatedAt: now.Add(-time.Hour)}
	recent := &Investigation{Incident: testIncident("recent"), Fingerprint: "abc", CreatedAt: now.Add(-10 * time.Minute)}
	other := &Investigation{Incident: testIncident("other"), Fingerprint: "xyz", CreatedAt: now}
	for _, inv := range []*Investigation{old, recent, other} {
		if err := s.Create(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		fp     string
		since  time.Time
		wantID string
	}{
		{"abc", now.Add(-30 * time.Minute), recent.ID},
		{"abc", now.Add(-2 * time.Hour), recent.ID},
		{"abc", now.Add(-5 * time.Minute), ""},
		{"nope", now.Add(-time.Hour), ""},
	}
	for _, tt := range tests {
		got, err := s.FindRecentByFingerprint(ctx, tt.fp, tt.since)
		if tt.wantID == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("Find(%s, %v) err = %v, want ErrNotFound", tt.fp, tt.since, err)
			}
			continue
		}
		if err != nil || got.ID != tt.wantID {
			t.Errorf("Find(%s, %v) = %v, %v; want %s", tt.fp, tt.since, got, err, tt.wantID)
		}
	}
}

func TestMarkInterrupted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	statuses := []string{core.StatusQueued, core.StatusRunning, core.StatusCompleted}
	var ids []string
	for _, st := range statuses {
		inv := &Investigation{Incident: testIncident(st), Status: st}
		if err := s.Create(ctx, inv); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, inv.ID)
	}
	got, err := s.MarkInterrupted(ctx, "interrupted by restart")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[0], ids[1]}
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("MarkInterrupted = %v, want %v", got, want)
	}
	for i, id := range ids {
		inv, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := core.StatusFailed
		if i == 2 {
			wantStatus = core.StatusCompleted
		}
		if inv.Status != wantStatus {
			t.Errorf("%s status = %s, want %s", statuses[i], inv.Status, wantStatus)
		}
	}
}
