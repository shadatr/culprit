package correlate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
)

// Signal classes, by how directly they show a service is broken.
const (
	classErrors     = "errors"     // error ratios, failures
	classLatency    = "latency"    // durations, latency
	classAbsent     = "absent"     // series stopped reporting
	classSaturation = "saturation" // pools, queues, cache hit ratio, connections
	classTraffic    = "traffic"    // request rates
	classResource   = "resource"   // cpu, memory, disk, gc
	classOther      = "other"
	classLogs       = "logs"
)

// classWeight is how much a signal of each class says "this service itself is
// unhealthy" (as opposed to "this service is busy"). Errors, latency, absence
// and error/warn log templates are user-facing symptoms (1.0); saturation is
// a strong precursor (0.7); traffic and resource usage mostly follow load
// (0.5 / 0.4). Memory that keeps resetting (restarts) counts as 0.8.
func classWeight(class string) float64 {
	switch class {
	case classErrors, classLatency, classAbsent:
		return 1
	case classSaturation:
		return 0.7
	case classTraffic, classOther:
		return 0.5
	case classResource:
		return 0.4
	}
	return 0.5
}

// strongClass reports whether a class is a direct sign of trouble.
func strongClass(class string) bool {
	return class == classErrors || class == classLatency || class == classAbsent
}

// metricClass classifies a metric by name (PLAN.md §5 naming, plus common
// Prometheus conventions).
func metricClass(name string) string {
	n := strings.ToLower(name)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(n, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("error", "fail", "5xx", "exception"):
		return classErrors
	case has("duration", "latency", "_time", "response_time"):
		return classLatency
	case has("pool", "queue", "saturation", "cache_hit", "conn", "inflight", "in_flight", "backlog", "throttl", "utiliz"):
		return classSaturation
	case has("request", "rps", "qps", "throughput", "traffic"):
		return classTraffic
	case has("cpu", "memory", "mem_", "heap", "disk", "gc", "fd", "thread", "goroutine"):
		return classResource
	}
	return classOther
}

// signal is one observation on a service.
type signal struct {
	t        time.Time // zero when the signal has no meaningful onset
	what     string    // e.g. "catalog http_request_duration_seconds:p99 rose"
	class    string
	q        float64 // quality: classWeight * strength, in [0,1]
	traffic  bool    // request rate rose
	text     string  // log pattern and sample, for "callers name it"
	logLevel float64 // level weight for log signals
	anomaly  *anomaly.Anomaly
	shift    *logpattern.Shift
}

// minOnsetQuality: signals weaker than this do not set a service's onset, so a
// faint CPU wiggle cannot claim to be "first".
const minOnsetQuality = 0.15

// serviceSignals collects what we know about one service.
type serviceSignals struct {
	name     string
	sigs     []signal
	onset    time.Time // earliest onset of a signal with q >= minOnsetQuality
	onsetSig signal
	q        float64 // 1 - prod(1 - q_i)
	qStrong  float64 // same, over strong classes and error/warn logs only
	traffic  bool
	step     time.Duration
}

func anomalySignal(a anomaly.Anomaly) signal {
	class := metricClass(a.Metric)
	if a.Direction == anomaly.Absent {
		class = classAbsent
	}
	w := classWeight(class)
	if class == classResource && a.Resets >= 2 {
		w = 0.8
	}
	s := signal{
		t:       a.Onset,
		class:   class,
		q:       w * math.Min(1, a.Score/20),
		traffic: class == classTraffic && a.Direction == anomaly.Up,
		anomaly: &a,
	}
	switch {
	case a.Direction == anomaly.Absent:
		s.what = fmt.Sprintf("%s %s stopped reporting", a.Service, a.Metric)
	case a.Resets >= 2:
		s.what = fmt.Sprintf("%s %s %s with %d resets", a.Service, a.Metric, verb(a.Direction), a.Resets)
	default:
		s.what = fmt.Sprintf("%s %s %s", a.Service, a.Metric, verb(a.Direction))
	}
	return s
}

func shiftSignal(x logpattern.Shift) signal {
	lw := logpattern.LevelWeight(x.Template.Level)
	w := 0.3 // info and below: usually traffic-driven
	switch {
	case lw >= 3:
		w = 1
	case lw >= 2:
		w = 0.8
	}
	s := signal{
		class:    classLogs,
		q:        w * math.Min(1, x.Score/40),
		text:     strings.ToLower(x.Template.Pattern + " " + x.Template.Sample),
		logLevel: lw,
		shift:    &x,
	}
	if x.New && !x.FirstInIncident.IsZero() {
		s.t = x.FirstInIncident
		s.what = fmt.Sprintf("%s logged a new %s pattern", x.Template.Service, levelWord(x.Template.Level))
	}
	return s
}

func (s signal) strong() bool {
	if s.class == classLogs {
		return s.logLevel >= 2
	}
	return strongClass(s.class)
}

func collect(an []anomaly.Anomaly, sh []logpattern.Shift) map[string]*serviceSignals {
	m := map[string]*serviceSignals{}
	get := func(svc string) *serviceSignals {
		if m[svc] == nil {
			m[svc] = &serviceSignals{name: svc}
		}
		return m[svc]
	}
	for _, a := range an {
		if a.Service == "" {
			continue
		}
		s := get(a.Service)
		s.sigs = append(s.sigs, anomalySignal(a))
		if st := time.Duration(a.StepSeconds * float64(time.Second)); st > s.step {
			s.step = st
		}
	}
	for _, x := range sh {
		if x.Template.Service == "" {
			continue
		}
		s := get(x.Template.Service)
		s.sigs = append(s.sigs, shiftSignal(x))
	}
	for _, s := range m {
		sort.SliceStable(s.sigs, func(i, j int) bool { return s.sigs[i].q > s.sigs[j].q })
		miss, missStrong := 1.0, 1.0
		for _, g := range s.sigs {
			miss *= 1 - g.q
			if g.strong() {
				missStrong *= 1 - g.q
			}
			if g.traffic {
				s.traffic = true
			}
			if g.t.IsZero() || g.q < minOnsetQuality {
				continue
			}
			if s.onset.IsZero() || g.t.Before(s.onset) {
				s.onset, s.onsetSig = g.t, g
			}
		}
		s.q, s.qStrong = 1-miss, 1-missStrong
	}
	return m
}

func sortedKeys(m map[string]*serviceSignals) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
