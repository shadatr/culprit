package anomaly

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// incidentStart is 2h after t0; series run 30m past it at 1m resolution.
var incidentStart = t0.Add(2 * time.Hour)

// gen builds a 151-point series (2h baseline + 30m incident at 1m step) whose
// value at minute i is f(i) plus Gaussian noise of sd.
func gen(seed uint64, name, svc string, sd float64, f func(min int) float64) core.Series {
	r := rand.New(rand.NewPCG(seed, seed*7+1))
	s := core.Series{Name: name, Labels: map[string]string{"service": svc}}
	for i := 0; i <= 150; i++ {
		s.Points = append(s.Points, core.Point{T: t0.Add(time.Duration(i) * time.Minute), V: f(i) + r.NormFloat64()*sd})
	}
	return s
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func TestDetectScenarios(t *testing.T) {
	const startMin = 120
	tests := []struct {
		name      string
		series    core.Series
		want      bool
		dir       string
		onset     time.Time
		tolerance time.Duration
	}{
		{
			name: "latency step 7m before incident start",
			series: gen(1, "http_request_duration_seconds:p99", "catalog", 0.01, func(m int) float64 {
				if m >= startMin-7 {
					return 0.45
				}
				return 0.2
			}),
			want: true, dir: "up", onset: incidentStart.Add(-7 * time.Minute), tolerance: 2 * time.Minute,
		},
		{
			name: "memory leak ramp starting 20m before",
			series: gen(2, "process_memory:bytes", "cart", 4e6, func(m int) float64 {
				base := 500e6
				if m >= startMin-20 {
					return base + float64(m-(startMin-20))*8e6
				}
				return base
			}),
			want: true, dir: "up", onset: incidentStart.Add(-20 * time.Minute), tolerance: 4 * time.Minute,
		},
		{
			name: "cache hit ratio drop after start",
			series: gen(3, "cache_hit:ratio", "catalog", 0.005, func(m int) float64 {
				if m >= startMin+2 {
					return 0.70
				}
				return 0.95
			}),
			want: true, dir: "down", onset: incidentStart.Add(2 * time.Minute), tolerance: 2 * time.Minute,
		},
		{
			name: "flat noisy series",
			series: gen(4, "http_request_duration_seconds:p99", "cart", 0.03, func(int) float64 {
				return 0.3
			}),
			want: false,
		},
		{
			name: "tiny error ratio wiggle",
			series: gen(5, "http_errors:ratio1m", "cart", 0.00002, func(m int) float64 {
				if m >= startMin {
					return 0.0003
				}
				return 0.0001
			}),
			want: false,
		},
		{
			name: "single spike is not a shift",
			series: gen(6, "process_cpu:ratio", "cart", 0.01, func(m int) float64 {
				if m == startMin+5 {
					return 0.95
				}
				return 0.3
			}),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect([]core.Series{tt.series}, Options{IncidentStart: incidentStart})
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("expected no anomaly, got %+v", got[0])
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 anomaly, got %d", len(got))
			}
			a := got[0]
			if a.Direction != tt.dir {
				t.Errorf("direction = %s, want %s", a.Direction, tt.dir)
			}
			if d := absDur(a.Onset.Sub(tt.onset)); d > tt.tolerance {
				t.Errorf("onset = %s, want %s +- %s", a.Onset.Format(time.TimeOnly), tt.onset.Format(time.TimeOnly), tt.tolerance)
			}
			if a.Service != tt.series.Labels["service"] {
				t.Errorf("service = %q", a.Service)
			}
			t.Logf("score=%.2f onset=%s baseline=%.4g peak=%.4g", a.Score, a.Onset.Format(time.TimeOnly), a.Baseline, a.Peak)
		})
	}
}

func TestTinyErrorRatioDoesNotOutrankLatency(t *testing.T) {
	lat := gen(10, "http_request_duration_seconds:p99", "frontend", 0.01, func(m int) float64 {
		if m >= 118 {
			return 0.4
		}
		return 0.2
	})
	errs := gen(11, "http_errors:ratio1m", "frontend", 0.00001, func(m int) float64 {
		if m >= 120 {
			return 0.0004 // 4x relative, but tiny in absolute terms
		}
		return 0.0001
	})
	got := Detect([]core.Series{errs, lat}, Options{IncidentStart: incidentStart, MinScore: 0.001})
	if len(got) == 0 || got[0].Metric != "http_request_duration_seconds:p99" {
		t.Fatalf("latency should rank first, got %+v", got)
	}
	if len(got) > 1 && got[1].Score >= got[0].Score/5 {
		t.Errorf("tiny error ratio scored too close: %.2f vs %.2f", got[1].Score, got[0].Score)
	}
}

func TestDetectEdgeCases(t *testing.T) {
	constant := gen(20, "x", "a", 0, func(int) float64 { return 5 })
	zeroThenErrors := gen(21, "http_errors:rate1m", "a", 0, func(m int) float64 {
		if m >= 121 {
			return 3
		}
		return 0
	})
	withNaN := gen(22, "y", "a", 0.01, func(m int) float64 {
		if m%10 == 0 {
			return math.NaN()
		}
		return 1
	})
	short := core.Series{Name: "short", Points: withNaN.Points[115:125]}
	gappy := gen(23, "http_request_duration_seconds:p99", "gap", 0.01, func(m int) float64 {
		if m >= 121 {
			return 0.6
		}
		return 0.2
	})
	gappy.Points = append(gappy.Points[:60:60], gappy.Points[100:]...) // 40m gap in baseline

	if got := Detect([]core.Series{constant, withNaN, short}, Options{IncidentStart: incidentStart}); len(got) != 0 {
		t.Fatalf("constant/NaN/short series flagged: %+v", got)
	}
	got := Detect([]core.Series{zeroThenErrors, gappy}, Options{IncidentStart: incidentStart})
	if len(got) != 2 {
		t.Fatalf("want 2 anomalies, got %+v", got)
	}
	for _, a := range got {
		if math.IsInf(a.Score, 0) || math.IsNaN(a.Score) || a.Score > scoreCap {
			t.Errorf("%s: bad score %v", a.Metric, a.Score)
		}
		if d := absDur(a.Onset.Sub(incidentStart.Add(time.Minute))); d > time.Minute {
			t.Errorf("%s: onset %s", a.Metric, a.Onset.Format(time.TimeOnly))
		}
	}
}

func TestDetectOrderingAndLimit(t *testing.T) {
	step := func(m int) float64 {
		if m >= 120 {
			return 2
		}
		return 1
	}
	a := gen(30, "b_metric", "x", 0, step)
	b := gen(30, "a_metric", "x", 0, step)
	c := gen(30, "a_metric", "w", 0, step)
	got := Detect([]core.Series{a, b, c}, Options{IncidentStart: incidentStart})
	if len(got) != 3 {
		t.Fatalf("got %d", len(got))
	}
	order := fmt.Sprint(got[0].Metric, got[0].Service, got[1].Metric, got[1].Service, got[2].Metric)
	if order != "a_metricwa_metricxb_metric" {
		t.Errorf("unstable tie-break order: %s", order)
	}
	if got := Detect([]core.Series{a, b, c}, Options{IncidentStart: incidentStart, MaxResults: 1}); len(got) != 1 {
		t.Errorf("MaxResults ignored: %d", len(got))
	}
}

func TestDescribe(t *testing.T) {
	s := gen(40, "http_request_duration_seconds:p99", "catalog", 0, func(m int) float64 {
		if m >= 120 {
			return 0.4
		}
		return 0.2
	})
	st := Describe(s, incidentStart)
	if math.Abs(st.BeforeMean-0.2) > 1e-9 || math.Abs(st.AfterMean-0.4) > 1e-9 || st.AfterMax != 0.4 || st.BeforeP95 != 0.2 {
		t.Errorf("stats wrong: %+v", st)
	}
	if st.ChangePercent != 100 {
		t.Errorf("change percent = %v", st.ChangePercent)
	}
	if st.Anomaly == nil || st.Anomaly.Direction != "up" {
		t.Errorf("anomaly missing: %+v", st.Anomaly)
	}
	flat := Describe(gen(41, "z", "q", 0, func(int) float64 { return 0 }), incidentStart)
	if flat.Anomaly != nil || flat.ChangePercent != 0 {
		t.Errorf("flat zero series: %+v", flat)
	}
}

func BenchmarkDetect(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	var series []core.Series
	start := t0.Add(400 * time.Minute)
	for i := range 500 {
		s := core.Series{Name: fmt.Sprintf("m%d", i), Labels: map[string]string{"service": fmt.Sprintf("s%d", i%20)}}
		for j := range 500 {
			v := 1 + r.NormFloat64()*0.05
			if i%10 == 0 && j > 390 {
				v += 1
			}
			s.Points = append(s.Points, core.Point{T: t0.Add(time.Duration(j) * time.Minute), V: v})
		}
		series = append(series, s)
	}
	b.ResetTimer()
	for b.Loop() {
		Detect(series, Options{IncidentStart: start})
	}
}

func TestNoFalsePositivesOnNoise(t *testing.T) {
	var series []core.Series
	for seed := uint64(100); seed < 300; seed++ {
		level := 0.05 + float64(seed%7)*0.3
		series = append(series, gen(seed, fmt.Sprintf("m%d", seed), "s", level*0.1, func(int) float64 { return level }))
	}
	if got := Detect(series, Options{IncidentStart: incidentStart}); len(got) != 0 {
		t.Fatalf("%d noise series flagged, top %+v", len(got), got[0])
	}
}

// gen3h builds a 3.5h series at 15s resolution: 3h before incidentStart3 and
// 30m after.
var incidentStart3 = t0.Add(3 * time.Hour)

func gen3h(seed uint64, name string, sd float64, f func(min float64) float64) core.Series {
	r := rand.New(rand.NewPCG(seed, seed*3+5))
	s := core.Series{Name: name, Labels: map[string]string{"service": "svc"}}
	for i := 0; i <= 210*4; i++ {
		m := float64(i) / 4
		s.Points = append(s.Points, core.Point{T: t0.Add(time.Duration(i) * 15 * time.Second), V: f(m) + r.NormFloat64()*sd})
	}
	return s
}

func TestOnsetNotClampedByLead(t *testing.T) {
	// Step 75 minutes before the incident start: far outside MaxLead.
	s := gen3h(50, "http_request_duration_seconds:p99", 0.01, func(m float64) float64 {
		if m >= 105 {
			return 0.6
		}
		return 0.2
	})
	got := Detect([]core.Series{s}, Options{IncidentStart: incidentStart3})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if want := t0.Add(105 * time.Minute); absDur(got[0].Onset.Sub(want)) > time.Minute {
		t.Errorf("onset %s, want %s", got[0].Onset.Format(time.TimeOnly), want.Format(time.TimeOnly))
	}
}

func TestLeakWithRestartsLongBeforeIncident(t *testing.T) {
	// Memory flat at 256MB, then from minute 40 ramps 15MB/min; every time it
	// reaches 1GB the process is OOM-killed and restarts at 300MB.
	s := gen3h(51, "process_memory:bytes", 2e6, func(m float64) float64 {
		if m < 40 {
			return 256e6
		}
		v := 256e6 + (m-40)*15e6
		for v > 1e9 {
			v -= 700e6
		}
		return v
	})
	got := Detect([]core.Series{s}, Options{IncidentStart: incidentStart3})
	if len(got) != 1 {
		t.Fatalf("leak not detected: %+v", got)
	}
	a := got[0]
	if a.Direction != Up || a.Resets < 2 {
		t.Errorf("direction %s resets %d", a.Direction, a.Resets)
	}
	if want := t0.Add(40 * time.Minute); absDur(a.Onset.Sub(want)) > 3*time.Minute {
		t.Errorf("onset %s, want ~%s", a.Onset.Format(time.TimeOnly), want.Format(time.TimeOnly))
	}
}

func TestDiurnalDriftNotFlagged(t *testing.T) {
	var series []core.Series
	for seed := uint64(60); seed < 80; seed++ {
		// +30% smooth growth over 3.5h, like morning traffic.
		series = append(series, gen3h(seed, fmt.Sprintf("http_requests:rate1m_%d", seed), 2, func(m float64) float64 {
			return 100 + 30*m/210 + 3*math.Sin(m/25)
		}))
	}
	if got := Detect(series, Options{IncidentStart: incidentStart3}); len(got) != 0 {
		t.Fatalf("%d drifting series flagged, first %+v", len(got), got[0])
	}
}

func TestAbsentSeries(t *testing.T) {
	full := gen3h(70, "process_cpu:ratio", 0.01, func(float64) float64 { return 0.1 })
	stopped := full
	stopped.Labels = map[string]string{"service": "redis"}
	cut := splitIndex(full.Points, incidentStart3.Add(-2*time.Minute))
	stopped.Points = full.Points[:cut:cut]
	resumed := core.Series{Name: "process_memory:bytes", Labels: map[string]string{"service": "redis"}}
	resumed.Points = append(append([]core.Point{}, full.Points[:cut]...), full.Points[cut+40:]...) // 10m gap over the start
	blip := core.Series{Name: "x", Labels: map[string]string{"service": "ok"}}
	early := splitIndex(full.Points, t0.Add(time.Hour))
	blip.Points = append(append([]core.Point{}, full.Points[:early]...), full.Points[early+40:]...) // gap long before

	got := Detect([]core.Series{full, stopped, resumed, blip}, Options{IncidentStart: incidentStart3})
	if len(got) != 2 {
		t.Fatalf("want 2 absent anomalies, got %+v", got)
	}
	for _, a := range got {
		if a.Direction != Absent || a.Service != "redis" || a.Score < 20 {
			t.Errorf("bad absent anomaly %+v", a)
		}
		if want := incidentStart3.Add(-2 * time.Minute); absDur(a.Onset.Sub(want)) > 15*time.Second {
			t.Errorf("onset %s, want %s", a.Onset.Format(time.TimeOnly), want.Format(time.TimeOnly))
		}
	}
}
