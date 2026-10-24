// Package correlate ranks recent changes and services as likely causes.
//
// Both rankings are small, explainable formulas over a few factors in [0,1];
// every factor that contributes adds a short human-readable reason. Signals
// (anomalies and log shifts) are first grouped per service and graded by how
// directly they show trouble: errors, latency, absence and error/warn log
// templates outrank saturation, which outranks traffic and CPU/memory (see
// classWeight).
package correlate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
)

// Tuning constants.
const (
	timingHalfLife = 15 * time.Minute // decay of a change's timing score before onset
	afterHalfLife  = 10 * time.Minute // decay for changes after onset
	onsetGrace     = 2 * time.Minute  // a change this soon after an onset may still be its cause
	lagHalfLife    = 10 * time.Minute // SuspectServices: decay of lag behind the earliest onset
	defaultStep    = 15 * time.Second // onset resolution when anomalies do not report a step
)

// ScoredChange is a change with a likelihood score and the reasons for it.
type ScoredChange struct {
	Change  core.Change `json:"change"`
	Score   float64     `json:"score"` // 0..1
	Reasons []string    `json:"reasons"`
}

// ScoredService is a service with a likelihood of being the root cause.
type ScoredService struct {
	Service       string    `json:"service"`
	Score         float64   `json:"score"` // 0..1
	EarliestOnset time.Time `json:"earliest_onset,omitempty"`
	Reasons       []string  `json:"reasons"`
}

// RankChanges scores each change as timing * service * kind, each in [0,1]:
//
//   - timing: relative to a reference time: the onset of the change's own
//     service, else the earliest onset among services connected to it in the
//     topology, else the earliest onset anywhere, else incidentStart. Onsets
//     come from signals of at least modest quality (anomaly onsets, first line
//     of a new log template). A change d before the reference scores
//     0.05 + 0.95 * 0.5^(d/15m); within 2m after it 0.7 (onsets are
//     estimates); later than that 0.1 * 0.5^((d-2m)/10m).
//   - service: 1 if the service itself has anomalies or log shifts, 0.7 if it
//     is a direct dependency of such a service (0.5 transitive), 0.4 if it
//     calls one, 0.5 for changes with no service or when there are no signals
//     at all, otherwise 0.15.
//   - kind: deploy and config 1, flag 0.95, commit 0.7, infra 0.6, other 0.5.
//
// Sorted by Score desc, then most recent first, then ID.
func RankChanges(changes []core.Change, an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, incidentStart time.Time) []ScoredChange {
	sig := collect(an, sh)
	out := make([]ScoredChange, 0, len(changes))
	for _, c := range changes {
		var reasons []string

		ref, what, ok := referenceOnset(c.Service, sig, topo)
		if !ok {
			ref, what = incidentStart, "the incident started"
		}
		timing, why := timingScore(c, ref, what)
		reasons = append(reasons, why)

		svc, why := serviceScore(c.Service, sig, topo)
		if why != "" {
			reasons = append(reasons, why)
		}

		kind := kindWeight(c.Kind)
		if kind < 0.9 {
			reasons = append(reasons, fmt.Sprintf("%s is a weaker signal than a deploy or config change", kindNoun(c.Kind)))
		}

		out = append(out, ScoredChange{Change: c, Score: round(clamp01(timing * svc * kind)), Reasons: reasons})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.Change.T.Equal(b.Change.T) {
			return a.Change.T.After(b.Change.T)
		}
		return a.Change.ID < b.Change.ID
	})
	return out
}

func referenceOnset(svc string, sig map[string]*serviceSignals, topo core.Topology) (time.Time, string, bool) {
	if s, ok := sig[svc]; ok && svc != "" && !s.onset.IsZero() {
		return s.onset, s.onsetSig.what, true
	}
	var best *serviceSignals
	consider := func(s *serviceSignals) {
		if !s.onset.IsZero() && (best == nil || s.onset.Before(best.onset)) {
			best = s
		}
	}
	names := sortedKeys(sig)
	if svc != "" {
		for _, n := range names {
			if n != svc && (dependsOn(topo, n, svc) || dependsOn(topo, svc, n)) {
				consider(sig[n])
			}
		}
	}
	if best == nil {
		for _, n := range names {
			consider(sig[n])
		}
	}
	if best == nil {
		return time.Time{}, "", false
	}
	return best.onset, best.onsetSig.what, true
}

func timingScore(c core.Change, ref time.Time, what string) (float64, string) {
	d := ref.Sub(c.T)
	v := kindVerb(c.Kind)
	switch {
	case d >= 0:
		s := 0.05 + 0.95*math.Pow(0.5, float64(d)/float64(timingHalfLife))
		if d < 30*time.Second {
			return s, fmt.Sprintf("%s just before %s", v, what)
		}
		return s, fmt.Sprintf("%s %s before %s", v, human(d), what)
	case -d <= onsetGrace:
		return 0.7, fmt.Sprintf("%s %s after %s (within onset uncertainty)", v, human(-d), what)
	default:
		s := 0.1 * math.Pow(0.5, float64(-d-onsetGrace)/float64(afterHalfLife))
		return s, fmt.Sprintf("%s %s after %s, so unlikely to have caused it", v, human(-d), what)
	}
}

func serviceScore(svc string, sig map[string]*serviceSignals, topo core.Topology) (float64, string) {
	if len(sig) == 0 {
		return 0.5, "no anomalies or log shifts to match against"
	}
	if svc == "" {
		return 0.5, "change is not tied to a service"
	}
	if s, ok := sig[svc]; ok {
		return 1, describeSignals(s)
	}
	names := sortedKeys(sig)
	for _, name := range names {
		if direct(topo, name, svc) {
			return 0.7, fmt.Sprintf("%s is a direct dependency of affected %s", svc, name)
		}
	}
	for _, name := range names {
		if dependsOn(topo, name, svc) {
			return 0.5, fmt.Sprintf("%s is an indirect dependency of affected %s", svc, name)
		}
	}
	for _, name := range names {
		if dependsOn(topo, svc, name) {
			return 0.4, fmt.Sprintf("%s calls affected %s", svc, name)
		}
	}
	return 0.15, fmt.Sprintf("%s shows no anomaly or log shift and is not connected to affected services", svc)
}

// describeSignals names the most telling signal of a service.
func describeSignals(s *serviceSignals) string {
	for _, g := range s.sigs {
		if g.shift != nil && g.shift.New && g.logLevel >= 3 {
			return fmt.Sprintf("%s shows a new %s pattern", s.name, levelWord(g.shift.Template.Level))
		}
	}
	for _, g := range s.sigs { // sorted by quality
		switch {
		case g.anomaly != nil && g.anomaly.Direction == anomaly.Absent:
			return fmt.Sprintf("%s %s stopped reporting", s.name, g.anomaly.Metric)
		case g.anomaly != nil:
			return fmt.Sprintf("%s has anomalous %s (%s, score %.1f)", s.name, g.anomaly.Metric, g.anomaly.Direction, g.anomaly.Score)
		case g.shift != nil && g.shift.New:
			return fmt.Sprintf("%s shows a new %s pattern", s.name, levelWord(g.shift.Template.Level))
		case g.shift != nil:
			return fmt.Sprintf("%s %s pattern rate rose", s.name, levelWord(g.shift.Template.Level))
		}
	}
	return s.name + " is affected"
}

// SuspectServices localises the likely root-cause service among services with
// anomalies or log shifts:
//
//		score = 0.7*earliness + 0.2*quality + 0.1*causal
//
//	  - earliness: 1 when the service's onset is within one sample step of the
//	    earliest onset (a tie: onset estimates are only that precise), else
//	    0.5 * 0.5^(lag/10m); 0.2 without an onset. Because a later service gets
//	    at most 0.5, being earlier by more than a step always dominates; the
//	    other terms only order near-ties.
//	  - quality: 1 - prod(1 - q_i) over its signals, q_i = class weight * strength
//	    (z/20 for anomalies, score/40 for log shifts).
//	  - causal (topology tie-break): 0.5 for the symptomatic service or when the
//	    topology/symptom is unknown; 0.6 + 0.4*strong for a dependency of the
//	    symptom, where strong is the quality of its own error/latency/absence/
//	    error-log signals (a busy but healthy dependency gets little); 0.3 off
//	    the symptom's dependency path. +0.2 when another service's error/warn
//	    logs name it and it has strong signals of its own. Set to 1 for an entry
//	    service (nobody calls it) whose request rate rose with the first signals:
//	    load entered there. Halved when the service looks like a victim: one of
//	    its dependencies has strong signals from no later than its own onset, or
//	    its own traffic rise is explained by a caller's traffic rise.
//
// If nothing is anomalous but symptom is set, the symptom is returned alone
// with a low score. Sorted by Score desc, then earliest onset, then name.
func SuspectServices(an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, symptom string) []ScoredService {
	sig := collect(an, sh)
	if len(sig) == 0 {
		if symptom == "" {
			return nil
		}
		return []ScoredService{{Service: symptom, Score: 0.1, Reasons: []string{"symptomatic service; no anomalies or log shifts found"}}}
	}
	names := sortedKeys(sig)

	var global time.Time
	step := time.Duration(0)
	for _, n := range names {
		s := sig[n]
		if !s.onset.IsZero() && (global.IsZero() || s.onset.Before(global)) {
			global = s.onset
		}
		step = max(step, s.step)
	}
	if step == 0 {
		step = defaultStep
	}
	tied := func(s *serviceSignals) bool { return !s.onset.IsZero() && s.onset.Sub(global) <= step }
	useTopo := len(topo) > 0 && symptom != ""
	called := map[string]bool{}
	for _, deps := range topo {
		for _, d := range deps {
			called[d] = true
		}
	}

	out := make([]ScoredService, 0, len(names))
	for _, n := range names {
		s := sig[n]
		var reasons []string
		sc := ScoredService{Service: n, EarliestOnset: s.onset}

		earliness := 0.2
		if !s.onset.IsZero() {
			lag := s.onset.Sub(global)
			when := s.onset.UTC().Format(time.RFC3339)
			switch {
			case lag == 0 && len(names) > 1:
				earliness = 1
				reasons = append(reasons, fmt.Sprintf("earliest onset among %d affected services (%s: %s)", len(names), when, s.onsetSig.what))
			case lag == 0:
				earliness = 1
				reasons = append(reasons, fmt.Sprintf("onset %s: %s", when, s.onsetSig.what))
			case lag <= step:
				earliness = 1
				reasons = append(reasons, fmt.Sprintf("onset %s after the earliest, within one sample step (%s)", human(lag), s.onsetSig.what))
			default:
				earliness = 0.5 * math.Pow(0.5, float64(lag)/float64(lagHalfLife))
				reasons = append(reasons, fmt.Sprintf("onset %s after the earliest affected service (%s)", human(lag), s.onsetSig.what))
			}
		}

		reasons = append(reasons, strengthReasons(s)...)

		causal := 0.5
		if useTopo {
			switch {
			case n == symptom:
				reasons = append(reasons, "symptomatic service")
			case dependsOn(topo, symptom, n):
				causal = 0.6 + 0.4*s.qStrong
				if s.qStrong >= 0.3 {
					reasons = append(reasons, fmt.Sprintf("dependency of symptomatic %s with its own errors/latency", symptom))
				} else {
					reasons = append(reasons, fmt.Sprintf("dependency of symptomatic %s, but only load/resource signals", symptom))
				}
			default:
				causal = 0.3
				reasons = append(reasons, fmt.Sprintf("not on the dependency path of %s", symptom))
			}
		}
		if s.qStrong > 0 {
			if by := namedBy(n, sig); by != "" {
				causal = math.Min(1, causal+0.2)
				reasons = append(reasons, fmt.Sprintf("%s error logs name %s", by, n))
			}
		}
		quality := s.q
		if len(topo) > 0 && s.traffic && !called[n] && tied(s) {
			causal = 1
			reasons = append(reasons, "entry service whose request rate rose first: load entered here")
		} else if v := victimOf(n, s, sig, topo, step); v != "" {
			causal *= 0.5
			quality *= 0.5
			reasons = append(reasons, v)
		}

		sc.Score = round(clamp01(0.7*earliness + 0.2*quality + 0.1*causal))
		sc.Reasons = reasons
		out = append(out, sc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.EarliestOnset.Equal(b.EarliestOnset) {
			if a.EarliestOnset.IsZero() || b.EarliestOnset.IsZero() {
				return !a.EarliestOnset.IsZero()
			}
			return a.EarliestOnset.Before(b.EarliestOnset)
		}
		return a.Service < b.Service
	})
	return out
}

// victimOf explains why service n is probably downstream of the real cause,
// or returns "".
func victimOf(n string, s *serviceSignals, sig map[string]*serviceSignals, topo core.Topology, step time.Duration) string {
	for _, d := range sortedKeys(sig) {
		ds := sig[d]
		if d == n || ds.qStrong < 0.3 || !dependsOn(topo, n, d) {
			continue
		}
		if s.onset.IsZero() || (!ds.onset.IsZero() && ds.onset.Sub(s.onset) <= step) {
			return fmt.Sprintf("its dependency %s shows errors/latency from no later", d)
		}
	}
	if s.traffic {
		for _, c := range sortedKeys(sig) {
			if c != n && sig[c].traffic && dependsOn(topo, c, n) {
				return fmt.Sprintf("its traffic rise follows caller %s's traffic rise", c)
			}
		}
	}
	return ""
}

// namedBy returns a service whose error/warn log shifts mention svc by name.
func namedBy(svc string, sig map[string]*serviceSignals) string {
	needle := strings.ToLower(svc)
	for _, n := range sortedKeys(sig) {
		if n == svc {
			continue
		}
		for _, g := range sig[n].sigs {
			if g.class == classLogs && g.logLevel >= 2 && containsWord(g.text, needle) {
				return n
			}
		}
	}
	return ""
}

// containsWord reports whether w occurs in s delimited by non-alphanumerics.
func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isAlnum(s[j-1])
		after := j+len(w) == len(s) || !isAlnum(s[j+len(w)])
		if before && after {
			return true
		}
		i = j + 1
	}
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func strengthReasons(s *serviceSignals) []string {
	var reasons []string
	nAn := 0
	var top *anomaly.Anomaly
	var topShift *logpattern.Shift
	for _, g := range s.sigs { // sorted by quality
		if g.anomaly != nil {
			nAn++
			if top == nil {
				top = g.anomaly
			}
		}
		if g.shift != nil && topShift == nil {
			topShift = g.shift
		}
	}
	if top != nil {
		var r string
		switch {
		case top.Direction == anomaly.Absent:
			r = fmt.Sprintf("%s stopped reporting", top.Metric)
		case top.Resets >= 2:
			r = fmt.Sprintf("%s %s (score %.1f) with %d resets (restarts)", top.Metric, verb(top.Direction), top.Score, top.Resets)
		default:
			r = fmt.Sprintf("%s %s (score %.1f)", top.Metric, verb(top.Direction), top.Score)
		}
		if nAn > 1 {
			r += fmt.Sprintf(" and %d more anomalous series", nAn-1)
		}
		reasons = append(reasons, r)
	}
	if topShift != nil {
		x := topShift
		if x.New {
			reasons = append(reasons, fmt.Sprintf("new %s log pattern %q", levelWord(x.Template.Level), x.Template.Pattern))
		} else {
			reasons = append(reasons, fmt.Sprintf("%s log pattern %q rose from %.2f to %.2f/min", levelWord(x.Template.Level), x.Template.Pattern, x.BaselineRate, x.IncidentRate))
		}
	}
	return reasons
}

// direct reports whether from calls to directly.
func direct(topo core.Topology, from, to string) bool {
	for _, d := range topo[from] {
		if d == to {
			return true
		}
	}
	return false
}

// dependsOn reports whether from (transitively) calls to. Cycles are safe.
func dependsOn(topo core.Topology, from, to string) bool {
	if len(topo) == 0 || from == to {
		return false
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range topo[cur] {
			if d == to {
				return true
			}
			if !seen[d] {
				seen[d] = true
				queue = append(queue, d)
			}
		}
	}
	return false
}

func kindWeight(kind string) float64 {
	switch strings.ToLower(kind) {
	case "deploy", "config":
		return 1
	case "flag":
		return 0.95
	case "commit":
		return 0.7
	case "infra":
		return 0.6
	}
	return 0.5
}

func kindVerb(kind string) string {
	switch strings.ToLower(kind) {
	case "deploy":
		return "deployed"
	case "config":
		return "config changed"
	case "flag":
		return "flag flipped"
	case "commit":
		return "committed"
	case "infra":
		return "infra changed"
	}
	return "changed"
}

func kindNoun(kind string) string {
	if kind == "" {
		return "an untyped change"
	}
	k := strings.ToLower(kind)
	if strings.ContainsAny(k[:1], "aeiou") {
		return "an " + k
	}
	return "a " + k
}

func verb(dir string) string {
	if dir == "down" {
		return "fell"
	}
	return "rose"
}

func levelWord(level string) string {
	switch w := logpattern.LevelWeight(level); {
	case w >= 4:
		return "fatal"
	case w >= 3:
		return "error"
	case w >= 2:
		return "warning"
	case w <= 0.5:
		return "debug"
	}
	return "info"
}

// human formats a duration compactly: 45s, 4m, 1h5m.
func human(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func round(v float64) float64 { return math.Round(v*1e4) / 1e4 }
