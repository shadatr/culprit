package logpattern

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestMask(t *testing.T) {
	tests := []struct{ in, want string }{
		{"user 123 not found", "user <*> not found"},
		{"request took 250ms (limit 1.5s)", "request took <*> (limit <*>)"},
		{"GET /api/users/42/orders?page=3 200", "GET /api/users/<*>/orders?<*> <*>"},
		{"connect to 10.0.0.12:5432 failed", "connect to <*> failed"},
		{"id=550e8400-e29b-41d4-a716-446655440000 ok", "id=<*> ok"},
		{"trace 4bf92f3577b34da6a3ce929d0e0e4736 span 0xdeadbeef", "trace <*> span <*>"},
		{`user="bob smith" said 'hi there', can't stop`, `user=<*> said <*>, can't stop`},
		{"mail sent to bob@example.com", "mail sent to <*>"},
		{"fetch https://api.example.com:8443/v1/items/123?x=1 failed", "fetch https://api.example.com:<*>/v1/items/<*>?<*> failed"},
		{"order-123456 shipped at 2026-10-20T14:05:00Z", "order-<*> shipped at <*>"},
		{"pool size 80% at 12:03:04", "pool size <*> at <*>"},
		{"retry in 1m30s, status=503 level=error", "retry in <*>, status=<*> level=error"},
		{"pod catalog-7d9f8c4b5-x2kq9 OOMKilled", "pod catalog-<*>-x2kq9 OOMKilled"},
		{"v1 http2 utf8 stays", "v1 http2 utf8 stays"},
	}
	for _, tt := range tests {
		if got := Mask(tt.in); got != tt.want {
			t.Errorf("Mask(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func line(min float64, svc, level, msg string) core.LogLine {
	return core.LogLine{T: t0.Add(time.Duration(min * float64(time.Minute))), Service: svc, Level: level, Message: msg}
}

func TestMineDrain(t *testing.T) {
	lines := []core.LogLine{
		line(0, "api", "warn", "user 123 not found"),
		line(1, "api", "warn", "user 456 not found"),
		line(2, "api", "info", "user alice logged in from web"),
		line(3, "api", "info", "user bob logged in from mobile"),
		line(4, "api", "info", "user carol logged in from web"),
		line(5, "db", "warn", "user 789 not found"), // other service: separate template
		line(6, "api", "info", "cache hit"),
		line(7, "api", "info", "cache miss"), // 2 tokens, 1 match: not merged
	}
	got := Mine(lines)
	want := map[string]int{
		"api|user <*> logged in from <*>": 3,
		"api|user <*> not found":          2,
		"db|user <*> not found":           1,
		"api|cache hit":                   1,
		"api|cache miss":                  1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d templates: %+v", len(got), got)
	}
	for _, tp := range got {
		k := tp.Service + "|" + tp.Pattern
		if want[k] != tp.Count {
			t.Errorf("%s: count %d, want %d", k, tp.Count, want[k])
		}
	}
	top := got[0]
	if top.Pattern != "user <*> logged in from <*>" || top.Sample != "user alice logged in from web" ||
		top.Level != "info" || !top.First.Equal(t0.Add(2*time.Minute)) || !top.Last.Equal(t0.Add(4*time.Minute)) {
		t.Errorf("top template: %+v", top)
	}
	if got[1].Pattern != "user <*> not found" || got[1].Level != "warn" || got[1].ID == "" {
		t.Errorf("second template: %+v", got[1])
	}
	// IDs are stable and differ per service.
	again := Mine(lines)
	ids := map[string]bool{}
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Errorf("unstable id for %s", got[i].Pattern)
		}
		ids[got[i].ID] = true
	}
	if len(ids) != len(got) {
		t.Errorf("duplicate ids")
	}
}

// scenario: 2h baseline of normal traffic, 30m incident with a new error
// template in catalog and a modest rise of an existing info template.
func scenario() ([]core.LogLine, time.Time) {
	r := rand.New(rand.NewPCG(7, 9))
	start := t0.Add(2 * time.Hour)
	var lines []core.LogLine
	for s := 0; s < 150*60; s += 5 {
		min := float64(s) / 60
		incident := s >= 120*60
		lines = append(lines, line(min, "catalog", "info", fmt.Sprintf("served GET /products/%d in %dms", r.IntN(1000), 5+r.IntN(40))))
		if r.IntN(10) == 0 {
			lines = append(lines, line(min, "cart", "warn", fmt.Sprintf("slow redis call took %dms", 100+r.IntN(50))))
		}
		if r.IntN(4) == 0 || (incident && r.IntN(4) == 0) {
			lines = append(lines, line(min, "frontend", "info", fmt.Sprintf("rendered page for session %08x", r.Uint32())))
		}
		if incident && s >= 121*60 && r.IntN(2) == 0 {
			lines = append(lines, line(min, "catalog", "error", fmt.Sprintf("pq: too many connections for role catalog (pool %d/%d)", 80+r.IntN(20), 100)))
		}
	}
	return lines, start
}

func TestDiff(t *testing.T) {
	lines, start := scenario()
	got := Diff(lines, start)
	if len(got) == 0 {
		t.Fatal("no shifts")
	}
	top := got[0]
	if !top.New || top.Template.Service != "catalog" || top.Template.Level != "error" || top.BaselineRate != 0 {
		t.Fatalf("top shift: %+v", top)
	}
	if top.FirstInIncident.Before(start.Add(time.Minute)) || top.FirstInIncident.After(start.Add(2*time.Minute)) {
		t.Errorf("first in incident = %s", top.FirstInIncident)
	}
	for _, s := range got {
		t.Logf("%.2f new=%v %s/%s %q base=%.3f inc=%.3f", s.Score, s.New, s.Template.Service, s.Template.Level, s.Template.Pattern, s.BaselineRate, s.IncidentRate)
		if s.Template.Service == "cart" || s.Template.Service == "catalog" && !s.New {
			t.Errorf("steady template reported as a rise: %+v", s)
		}
		if s.IncidentRate <= s.BaselineRate {
			t.Errorf("not a rise: %+v", s)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("not sorted at %d", i)
		}
	}
}

func TestDiffEdgeCases(t *testing.T) {
	start := t0.Add(time.Hour)
	if Diff(nil, start) != nil {
		t.Error("nil lines")
	}
	onlyIncident := []core.LogLine{line(61, "a", "error", "boom 1"), line(62, "a", "error", "boom 2")}
	if Diff(onlyIncident, start) != nil {
		t.Error("no baseline should give no shifts")
	}
	// A single new info line is noise; a single new error line is reported.
	lines := []core.LogLine{
		line(0, "a", "info", "tick"), line(30, "a", "info", "tick"), line(59, "a", "info", "tick"),
		line(61, "a", "info", "something odd happened"),
		line(62, "a", "error", "disk full on /var"),
	}
	got := Diff(lines, start)
	if len(got) != 1 || got[0].Template.Pattern != "disk full on /var" || !got[0].New {
		t.Fatalf("got %+v", got)
	}
}

func BenchmarkMine(b *testing.B) {
	r := rand.New(rand.NewPCG(3, 4))
	msgs := []string{
		"served GET /products/%d in %dms",
		"user %d not found",
		"payment %d declined code=%d",
		"cache miss for key product:%d took %dms",
		"connection from 10.0.%d.%d accepted",
		"job %d finished in %ds",
		"timeout after %dms calling inventory attempt %d",
	}
	svcs := []string{"catalog", "cart", "frontend", "payments", "inventory"}
	lines := make([]core.LogLine, 50000)
	for i := range lines {
		lines[i] = core.LogLine{
			T:       t0.Add(time.Duration(i) * 100 * time.Millisecond),
			Service: svcs[r.IntN(len(svcs))],
			Level:   "info",
			Message: fmt.Sprintf(msgs[r.IntN(len(msgs))], r.IntN(100000), r.IntN(300)),
		}
	}
	b.ResetTimer()
	for b.Loop() {
		Mine(lines)
	}
}

func TestDiffIgnoresSmallRises(t *testing.T) {
	start := t0.Add(2 * time.Hour)
	var lines []core.LogLine
	for m := 0; m < 150; m += 2 {
		lines = append(lines, line(float64(m), "redis", "info", "DB saved on disk"))
		if m >= 120 && m%6 == 0 { // ~1.5x the baseline rate in the incident
			lines = append(lines, line(float64(m)+1, "redis", "info", "DB saved on disk"))
		}
	}
	if got := Diff(lines, start); len(got) != 0 {
		t.Fatalf("1.5x rise reported: %+v", got)
	}
}
