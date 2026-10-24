package correlate

import (
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
)

var (
	start = time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)
	topo  = core.Topology{
		"frontend": {"catalog", "cart"},
		"catalog":  {"postgres"},
		"cart":     {"redis"},
	}
)

func at(min int) time.Time { return start.Add(time.Duration(min) * time.Minute) }

// catalog regresses at 14:00 (5m before the alert), frontend p99 follows at
// 14:02, and catalog starts logging a new error.
func scenario() ([]anomaly.Anomaly, []logpattern.Shift) {
	an := []anomaly.Anomaly{
		{Metric: "http_request_duration_seconds:p99", Service: "frontend", Direction: "up", Onset: at(-3), Score: 25},
		{Metric: "http_request_duration_seconds:p99", Service: "catalog", Direction: "up", Onset: at(-5), Score: 18},
		{Metric: "db_query_duration_seconds:p99", Service: "catalog", Direction: "up", Onset: at(-5), Score: 12},
	}
	sh := []logpattern.Shift{{
		Template:        logpattern.Template{Service: "catalog", Level: "error", Pattern: "pq: too many connections"},
		New:             true,
		IncidentRate:    6,
		Score:           80,
		FirstInIncident: at(-4),
	}}
	return an, sh
}

func TestRankChanges(t *testing.T) {
	an, sh := scenario()
	changes := []core.Change{
		{ID: "commit-billing", Kind: "commit", Service: "billing", T: at(-45), Summary: "refactor invoices"},
		{ID: "deploy-frontend", Kind: "deploy", Service: "frontend", T: at(8), Summary: "rollback attempt"},
		{ID: "deploy-catalog-1.8.2", Kind: "deploy", Service: "catalog", T: at(-10), Summary: "catalog 1.8.2"},
		{ID: "config-cart", Kind: "config", Service: "cart", T: at(-9), Summary: "tune redis timeout"},
		{ID: "infra-global", Kind: "infra", T: at(-20), Summary: "node pool upgrade"},
	}
	got := RankChanges(changes, an, sh, topo, start)
	if len(got) != len(changes) {
		t.Fatalf("got %d", len(got))
	}
	for _, c := range got {
		t.Logf("%.3f %-22s %s", c.Score, c.Change.ID, strings.Join(c.Reasons, "; "))
		if c.Score < 0 || c.Score > 1 {
			t.Errorf("score out of range: %v", c.Score)
		}
	}
	if got[0].Change.ID != "deploy-catalog-1.8.2" {
		t.Fatalf("top change = %s", got[0].Change.ID)
	}
	rank := map[string]int{}
	for i, c := range got {
		rank[c.Change.ID] = i
	}
	if got[0].Score < 2*got[rank["commit-billing"]].Score || got[0].Score < 2*got[rank["deploy-frontend"]].Score {
		t.Errorf("top change not clearly ahead: %+v", got)
	}
	reasons := strings.Join(got[0].Reasons, "; ")
	for _, want := range []string{"deployed 5m before catalog", "catalog shows a new error pattern"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons %q missing %q", reasons, want)
		}
	}
	// Stable across calls.
	again := RankChanges(changes, an, sh, topo, start)
	for i := range got {
		if got[i].Change.ID != again[i].Change.ID || got[i].Score != again[i].Score {
			t.Fatal("unstable ordering")
		}
	}
}

func TestRankChangesNoSignals(t *testing.T) {
	changes := []core.Change{
		{ID: "a", Kind: "deploy", Service: "x", T: at(-30)},
		{ID: "b", Kind: "deploy", Service: "y", T: at(-2)},
	}
	got := RankChanges(changes, nil, nil, nil, start)
	if len(got) != 2 || got[0].Change.ID != "b" {
		t.Fatalf("closest change to the incident start should win: %+v", got)
	}
	if RankChanges(nil, nil, nil, nil, start) == nil {
		// an empty, non-nil slice is fine either way; just must not panic
		t.Log("nil result for no changes")
	}
}

func TestSuspectServices(t *testing.T) {
	an, sh := scenario()
	tests := []struct {
		name    string
		topo    core.Topology
		symptom string
		want    string
	}{
		{"dependency over symptomatic frontend", topo, "frontend", "catalog"},
		{"empty topology still prefers earliest+strongest", nil, "frontend", "catalog"},
		{"empty symptom", topo, "", "catalog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SuspectServices(an, sh, tt.topo, tt.symptom)
			if len(got) != 2 {
				t.Fatalf("got %+v", got)
			}
			for _, s := range got {
				t.Logf("%.3f %s %s", s.Score, s.Service, strings.Join(s.Reasons, "; "))
			}
			if got[0].Service != tt.want {
				t.Errorf("top = %s, want %s", got[0].Service, tt.want)
			}
			if !got[0].EarliestOnset.Equal(at(-5)) {
				t.Errorf("earliest onset = %s", got[0].EarliestOnset)
			}
		})
	}
}

func TestSuspectServicesLaterDependencyLoses(t *testing.T) {
	// cart only degrades 10 minutes after frontend: it is a victim, not a cause.
	an := []anomaly.Anomaly{
		{Metric: "http_request_duration_seconds:p99", Service: "frontend", Direction: "up", Onset: at(-1), Score: 20},
		{Metric: "http_request_duration_seconds:p99", Service: "cart", Direction: "up", Onset: at(9), Score: 20},
	}
	got := SuspectServices(an, nil, topo, "frontend")
	if len(got) != 2 || got[0].Service != "frontend" {
		t.Fatalf("got %+v", got)
	}
}

func TestSuspectServicesEmpty(t *testing.T) {
	if got := SuspectServices(nil, nil, nil, ""); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
	got := SuspectServices(nil, nil, topo, "frontend")
	if len(got) != 1 || got[0].Service != "frontend" || got[0].Score > 0.2 {
		t.Errorf("got %+v", got)
	}
}

func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 4 * time.Minute: "4m", 65 * time.Minute: "1h5m", 2 * time.Hour: "2h",
	} {
		if got := human(d); got != want {
			t.Errorf("human(%s) = %s, want %s", d, got, want)
		}
	}
}

func an(svc, metric, dir string, onsetSec int, score float64) anomaly.Anomaly {
	return anomaly.Anomaly{Metric: metric, Service: svc, Direction: dir, Onset: start.Add(time.Duration(onsetSec) * time.Second), Score: score, StepSeconds: 15}
}

func logShift(svc, level, pattern string, score float64) logpattern.Shift {
	return logpattern.Shift{Template: logpattern.Template{Service: svc, Level: level, Pattern: pattern, Sample: pattern}, Score: score, BaselineRate: 0.5, IncidentRate: 20, FirstInIncident: start}
}

const (
	p99    = "http_request_duration_seconds:p99"
	errs   = "http_errors:ratio1m"
	rate   = "http_requests:rate1m"
	cpu    = "process_cpu:ratio"
	memory = "process_memory:bytes"
)

func TestSuspectServicesScenarios(t *testing.T) {
	topo := core.Topology{"frontend": {"catalog", "cart", "search"}, "catalog": {"postgres"}, "cart": {"redis"}, "search": {"catalog"}}
	tests := []struct {
		name   string
		an     []anomaly.Anomaly
		sh     []logpattern.Shift
		want   string
		reason string
	}{
		{
			name: "resource-only leaf one step later loses to service with latency",
			an: []anomaly.Anomaly{
				an("catalog", p99, "up", -300, 60), an("postgres", cpu, "up", -285, 30),
				an("frontend", p99, "up", -285, 25), an("search", p99, "up", -285, 30),
			},
			want: "catalog",
		},
		{
			name: "earlier by more than a step dominates a stronger later signal",
			an: []anomaly.Anomaly{
				an("postgres", cpu, "up", -360, 40), an("catalog", p99, "up", -300, 60), an("frontend", p99, "up", -300, 30),
			},
			want: "postgres",
		},
		{
			name: "a faint early wiggle does not claim the onset",
			an: []anomaly.Anomaly{
				an("redis", cpu, "up", -1800, 4), an("catalog", p99, "up", -300, 60), an("frontend", p99, "up", -300, 30),
			},
			want: "catalog",
		},
		{
			name: "simultaneous onset: dependency with its own TLS errors, named by the caller",
			an: []anomaly.Anomaly{
				an("frontend", errs, "up", 0, 85), an("search", rate, "down", 0, 17), an("search", cpu, "down", 0, 14),
				an("catalog", rate, "down", 0, 5), an("catalog", cpu, "down", 0, 5), an("postgres", cpu, "down", 0, 5),
			},
			sh: []logpattern.Shift{
				logShift("frontend", "error", "search upstream error: x509: certificate has expired", 80),
				logShift("search", "warn", "http: TLS handshake error from <*>: bad certificate", 60),
			},
			want: "search", reason: "frontend error logs name search",
		},
		{
			name: "caller blaming a healthy dependency does not move the blame",
			an: []anomaly.Anomaly{
				an("frontend", errs, "up", -285, 46), an("catalog", cpu, "up", -285, 8), an("catalog", rate, "up", -270, 5),
			},
			sh:   []logpattern.Shift{logShift("frontend", "error", "upstream catalog: context deadline exceeded", 80)},
			want: "frontend",
		},
		{
			name: "saturated database with its own warnings beats its victim's latency",
			an: []anomaly.Anomaly{
				an("catalog", p99, "up", -825, 78), an("postgres", cpu, "up", -825, 51), an("frontend", p99, "up", -795, 32),
			},
			sh:   []logpattern.Shift{logShift("postgres", "warn", "checkpoints are occurring too frequently", 20)},
			want: "postgres", reason: "its dependency postgres shows errors/latency",
		},
		{
			name: "traffic entering at the edge",
			an: []anomaly.Anomaly{
				an("frontend", rate, "up", -480, 30), an("frontend", p99, "up", -390, 67),
				an("search", rate, "up", -495, 30), an("search", errs, "up", -330, 59), an("search", p99, "up", -390, 53),
				an("catalog", rate, "up", -495, 30), an("postgres", cpu, "up", -495, 50),
			},
			want: "frontend", reason: "load entered here",
		},
		{
			name: "dependency that stopped reporting",
			an: []anomaly.Anomaly{
				an("redis", cpu, "absent", -120, 100), an("cart", errs, "up", -105, 100), an("frontend", p99, "up", -105, 57),
			},
			want: "redis",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SuspectServices(tt.an, tt.sh, topo, "frontend")
			var all []string
			for _, s := range got {
				all = append(all, s.Service+" "+strings.Join(s.Reasons, "; "))
				t.Logf("%.3f %s: %s", s.Score, s.Service, strings.Join(s.Reasons, "; "))
			}
			if got[0].Service != tt.want {
				t.Fatalf("top = %s, want %s", got[0].Service, tt.want)
			}
			if tt.reason != "" && !strings.Contains(strings.Join(all, "\n"), tt.reason) {
				t.Errorf("missing reason %q", tt.reason)
			}
		})
	}
}

func TestRankChangesUsesLongLeadOnset(t *testing.T) {
	// A leak that began 2h before the alert: the deploy right before its onset
	// beats recent unrelated changes.
	ans := []anomaly.Anomaly{{Metric: memory, Service: "cart", Direction: "up", Onset: at(-139), Score: 14, Resets: 5}}
	changes := []core.Change{
		{ID: "deploy-cart-2.4.0", Kind: "deploy", Service: "cart", T: at(-140)},
		{ID: "deploy-frontend", Kind: "deploy", Service: "frontend", T: at(-6)},
	}
	got := RankChanges(changes, ans, nil, topo, start)
	if got[0].Change.ID != "deploy-cart-2.4.0" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(strings.Join(got[0].Reasons, ";"), "with 5 resets") {
		t.Errorf("reasons %v", got[0].Reasons)
	}
}
