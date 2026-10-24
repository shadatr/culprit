// Package anomaly finds metric series that changed around an incident and
// estimates when the change began.
//
// Method, per series:
//
//  1. Clean: drop NaN/Inf points and sort by time. Time (not sample index) is
//     the x axis, so gaps are handled naturally.
//  2. Absence: if the series had a regular scrape interval and then stops
//     reporting for more than 3 intervals (and at least 2 minutes) around the
//     incident, it is reported with Direction "absent" (see absentScore).
//  3. Onset: fit a two-segment piecewise-linear model over the WHOLE series
//     and pick the split with the best Gaussian log-likelihood, each segment
//     having its own line and its own noise variance. A step, a ramp (memory
//     leak), a variance change (sawtooth from restarts) and a trend change all
//     fit this model. Nothing is clamped: a soft prior (at most 2 log-lik
//     units, growing with distance) prefers splits within MaxLead of
//     IncidentStart only when the data barely distinguish them.
//  4. Baseline: points before the onset. Level = median, spread = 1.4826*MAD,
//     both on de-trended values when the baseline has a clear linear trend
//     (diurnal traffic growth), estimated robustly from the medians of its
//     first and last thirds and extrapolated at most one baseline span.
//  5. Score: robust z of the incident level against the (trend-adjusted)
//     baseline. The incident level is the 75th percentile of the incident
//     segment (points at or after IncidentStart) for a rise and the 25th
//     percentile for a drop: a quarter of the incident has to be shifted, so
//     single spikes do not count, but a sawtooth does.
//     The score is multiplied by a relevance factor 1/(1+h), where h is the
//     number of hours by which the onset precedes IncidentStart-MaxLead.
//  6. Resets: for rises, large drops after the onset (more than half the rise)
//     that never happened in the baseline are counted in Resets; repeated
//     resets of a memory-like metric mean the process is restarting.
//
// Scale rule (how relative and absolute change are combined):
//
//	scale = max(1.4826*MAD, 5% of the reference level, absolute floor)
//
// The reference level is |median| for ordinary metrics, and min(m, 1-m) for
// ratio metrics (name contains "ratio"), so a cache hit ratio dropping from
// 0.95 is judged against its miss rate. The absolute floor is 0.0025 (a
// quarter of a percentage point) for ratio metrics, and 1% of the series'
// largest |value| otherwise, so a series that sat at exactly zero still gets a
// finite score. A change smaller than 5% of the level, or an error ratio moving
// from 0.0001 to 0.0003, therefore cannot reach the default MinScore of 3,
// while a latency doubling scores about 20. Scores are capped at 100.
package anomaly

import (
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// Directions.
const (
	Up     = "up"
	Down   = "down"
	Absent = "absent" // the series stopped reporting
)

// Defaults and limits used by Detect.
const (
	DefaultMinScore = 3.0
	DefaultMaxLead  = 30 * time.Minute

	minBaselinePoints = 6
	minIncidentPoints = 3
	relFloor          = 0.05
	ratioAbsFloor     = 0.0025
	maxAbsFrac        = 0.01
	scoreCap          = 100.0
	madToSigma        = 1.4826
	priorWeight       = 2.0 // log-likelihood units
	minTrendSpan      = 30 * time.Minute
	minAbsentGap      = 2 * time.Minute
)

// Options controls detection.
type Options struct {
	IncidentStart time.Time     // baseline is everything before the estimated onset; the incident segment starts here
	MinScore      float64       // drop anomalies scoring below this; 0 means default (3)
	MaxResults    int           // 0 means no limit
	MaxLead       time.Duration // onsets within this before IncidentStart are mildly preferred on near-ties; 0 means 30m. Never a hard limit.
	WindowEnd     time.Time     // end of the queried window, used to notice series that stopped reporting; zero means the latest point of any series
}

// Anomaly is a series whose incident behaviour differs from its baseline.
type Anomaly struct {
	Metric      string            `json:"metric"`
	Labels      map[string]string `json:"labels"`
	Service     string            `json:"service,omitempty"` // Labels["service"]
	Direction   string            `json:"direction"`         // up | down | absent (stopped reporting)
	Onset       time.Time         `json:"onset"`             // estimated start of the change (for absent: first missing sample)
	Baseline    float64           `json:"baseline"`          // typical value before
	Peak        float64           `json:"peak"`              // most extreme value after onset (for absent: last value seen)
	Score       float64           `json:"score"`             // robust z-score style magnitude
	Resets      int               `json:"resets,omitempty"`  // large drops after onset not seen in baseline (restarts)
	StepSeconds float64           `json:"step_seconds,omitempty"`
}

// Stats describes one series before and after the incident start.
type Stats struct {
	Metric        string            `json:"metric"`
	Labels        map[string]string `json:"labels"`
	BeforeMean    float64           `json:"before_mean"`
	BeforeP95     float64           `json:"before_p95"`
	AfterMean     float64           `json:"after_mean"`
	AfterMax      float64           `json:"after_max"`
	ChangePercent float64           `json:"change_percent"` // 0 when BeforeMean is 0
	Anomaly       *Anomaly          `json:"anomaly,omitempty"`
}

// Detect returns anomalies sorted by Score, highest first. Ties are broken by
// metric name, then by the sorted label set. Series with fewer than 6 points
// before IncidentStart (after dropping NaN/Inf) are ignored; series with fewer
// than 3 points from IncidentStart are only checked for absence.
func Detect(series []core.Series, opts Options) []Anomaly {
	opts = withDefaults(opts)
	if opts.WindowEnd.IsZero() {
		for _, s := range series {
			for _, p := range s.Points {
				if p.T.After(opts.WindowEnd) {
					opts.WindowEnd = p.T
				}
			}
		}
	}
	var out []Anomaly
	for _, s := range series {
		if a, ok := detectOne(s, opts); ok && a.Score >= opts.MinScore {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Metric != out[j].Metric {
			return out[i].Metric < out[j].Metric
		}
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	if opts.MaxResults > 0 && len(out) > opts.MaxResults {
		out = out[:opts.MaxResults]
	}
	return out
}

// Describe summarises one series around incidentStart: mean and p95 before,
// mean and max at/after, the change in mean as a percentage, and the Anomaly
// that Detect (with default options) would report, if any.
func Describe(s core.Series, incidentStart time.Time) Stats {
	st := Stats{Metric: s.Name, Labels: copyLabels(s.Labels)}
	pts := clean(s.Points)
	b := splitIndex(pts, incidentStart)
	before, after := values(pts[:b]), values(pts[b:])
	if len(before) > 0 {
		st.BeforeMean = mean(before)
		st.BeforeP95 = quantile(sorted(before), 0.95)
	}
	if len(after) > 0 {
		st.AfterMean = mean(after)
		st.AfterMax = slices.Max(after)
	}
	if len(before) > 0 && len(after) > 0 && st.BeforeMean != 0 {
		st.ChangePercent = round((st.AfterMean-st.BeforeMean)/math.Abs(st.BeforeMean)*100, 2)
	}
	opts := withDefaults(Options{IncidentStart: incidentStart})
	if len(pts) > 0 {
		opts.WindowEnd = pts[len(pts)-1].T
	}
	if a, ok := detectOne(s, opts); ok && a.Score >= opts.MinScore {
		st.Anomaly = &a
	}
	return st
}

func withDefaults(o Options) Options {
	if o.MinScore == 0 {
		o.MinScore = DefaultMinScore
	}
	if o.MaxLead == 0 {
		o.MaxLead = DefaultMaxLead
	}
	return o
}

// detectOne scores a single series. ok is false when the series has too few
// usable points.
func detectOne(s core.Series, opts Options) (Anomaly, bool) {
	pts := clean(s.Points)
	b := splitIndex(pts, opts.IncidentStart)
	if b < minBaselinePoints {
		return Anomaly{}, false
	}
	base := Anomaly{Metric: s.Name, Labels: copyLabels(s.Labels), Service: s.Labels["service"]}
	step := typicalStep(pts[:b])
	if step > 0 {
		base.StepSeconds = step.Seconds()
	}
	if a, ok := absent(pts, b, step, opts); ok {
		a.Metric, a.Labels, a.Service, a.StepSeconds = base.Metric, base.Labels, base.Service, base.StepSeconds
		return a, true
	}
	if len(pts)-b < minIncidentPoints {
		return Anomaly{}, false
	}
	isRatio := strings.Contains(strings.ToLower(s.Name), "ratio")

	k := changePoint(pts, opts.IncidentStart.Add(-opts.MaxLead))
	if k < minBaselinePoints {
		k = b
	}
	bl := fitBaseline(pts[:k], isRatio, pts)

	inc := make([]float64, 0, len(pts)-b)
	for _, p := range pts[b:] {
		inc = append(inc, p.V-bl.trendAt(p.T))
	}
	slices.Sort(inc)
	zUp := (quantile(inc, 0.75) - bl.med) / bl.scale
	zDown := (bl.med - quantile(inc, 0.25)) / bl.scale

	a := base
	a.Direction, a.Baseline, a.Score = Up, bl.med, zUp
	if zDown > zUp {
		a.Direction, a.Score = Down, zDown
	}
	a.Score = math.Max(a.Score, 0)

	// Sanity check the split: the post-onset segment must actually have moved
	// in the detected direction. Otherwise fall back to the first incident
	// point beyond 3 scales.
	if !movedFrom(pts[k:], bl, a.Direction) {
		k = b
		for i := b; i < len(pts); i++ {
			if beyond(pts[i].V-bl.trendAt(pts[i].T), bl.med, 3*bl.scale, a.Direction) {
				k = i
				break
			}
		}
	}
	a.Onset = pts[k].T
	a.Peak = pts[k].V
	for _, p := range pts[k:] {
		if (a.Direction == Up && p.V > a.Peak) || (a.Direction == Down && p.V < a.Peak) {
			a.Peak = p.V
		}
	}
	if a.Direction == Up {
		a.Resets = countResets(pts, k, 0.5*(a.Peak-bl.med))
	}
	a.Score = round(math.Min(a.Score*relevance(a.Onset, opts), scoreCap), 2)
	return a, true
}

// relevance down-weights changes that began long before the incident:
// 1 / (1 + hours by which the onset precedes IncidentStart-MaxLead). A slow
// change that started hours earlier must be correspondingly larger to be
// reported, which filters diurnal drift but keeps strong leaks.
func relevance(onset time.Time, opts Options) float64 {
	d := opts.IncidentStart.Add(-opts.MaxLead).Sub(onset)
	if d <= 0 {
		return 1
	}
	return 1 / (1 + d.Hours())
}

// countResets counts drops larger than d after index k, provided the
// baseline (before k) has none.
func countResets(pts []core.Point, k int, d float64) int {
	if d <= 0 {
		return 0
	}
	for i := 1; i < k; i++ {
		if pts[i].V < pts[i-1].V-d {
			return 0
		}
	}
	n := 0
	for i := max(k, 1); i < len(pts); i++ {
		if pts[i].V < pts[i-1].V-d {
			n++
		}
	}
	return n
}

// typicalStep is the median interval between points, or 0.
func typicalStep(pts []core.Point) time.Duration {
	if len(pts) < 2 {
		return 0
	}
	d := make([]float64, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		d = append(d, float64(pts[i].T.Sub(pts[i-1].T)))
	}
	slices.Sort(d)
	return time.Duration(median(d))
}

// absent reports a series that stopped reporting. The baseline must be
// regular (at least 90% of intervals within 1.5 steps); the gap must exceed
// 3 steps and 2 minutes, and still be open at IncidentStart. A gap
// running to the window end counts. Score = min(100, 20 + 2*missing samples),
// so even a short outage outranks most value changes: a target that vanishes
// is rarely benign.
func absent(pts []core.Point, b int, step time.Duration, opts Options) (Anomaly, bool) {
	if step <= 0 {
		return Anomaly{}, false
	}
	regular := 0
	for i := 1; i < b; i++ {
		if pts[i].T.Sub(pts[i-1].T) <= step*3/2 {
			regular++
		}
	}
	if float64(regular) < 0.9*float64(b-1) {
		return Anomaly{}, false
	}
	minGap := max(3*step, minAbsentGap)
	best, bestGap := -1, time.Duration(0)
	for i := minBaselinePoints; i <= len(pts); i++ {
		var gap time.Duration
		var end time.Time
		if i == len(pts) {
			end = opts.WindowEnd
			gap = end.Sub(pts[i-1].T)
		} else {
			end = pts[i].T
			gap = end.Sub(pts[i-1].T)
		}
		if gap > minGap && gap > bestGap && end.After(opts.IncidentStart) {
			best, bestGap = i, gap
		}
	}
	if best < 0 {
		return Anomaly{}, false
	}
	last := pts[best-1]
	missing := int(bestGap/step) - 1
	before := sorted(values(pts[:best]))
	return Anomaly{
		Direction: Absent,
		Onset:     last.T.Add(step),
		Baseline:  median(before),
		Peak:      last.V,
		Score:     math.Min(scoreCap, float64(20+2*missing)),
	}, true
}

// changePoint returns the index k of the first point of the right segment of
// the best two-segment piecewise-linear fit (each segment with its own
// variance), or -1 when no split is significant. The left segment keeps at
// least max(6, n/10) points (a usable baseline), the right at least 3. A split
// must beat a single line by a BIC-style margin of 3*ln(n) log-likelihood
// units per extra parameter (3 extra: line and variance). Splits before
// preferFrom pay a small penalty.
func changePoint(pts []core.Point, preferFrom time.Time) int {
	n := len(pts)
	if n < minBaselinePoints+minIncidentPoints {
		return -1
	}
	t0 := pts[0].T
	center := mean(values(pts))
	px, py, pxx, pxy, pyy := make([]float64, n+1), make([]float64, n+1), make([]float64, n+1), make([]float64, n+1), make([]float64, n+1)
	for i, p := range pts {
		x := p.T.Sub(t0).Minutes()
		y := p.V - center
		px[i+1] = px[i] + x
		py[i+1] = py[i] + y
		pxx[i+1] = pxx[i] + x*x
		pxy[i+1] = pxy[i] + x*y
		pyy[i+1] = pyy[i] + y*y
	}
	total := pyy[n] / float64(n)
	if total <= 0 {
		return -1
	}
	eps := 1e-6 * total
	span := pts[n-1].T.Sub(t0).Minutes()
	sse := func(i, j int) float64 {
		m := float64(j - i)
		sx, sy := px[j]-px[i], py[j]-py[i]
		sxx, sxy, syy := pxx[j]-pxx[i], pxy[j]-pxy[i], pyy[j]-pyy[i]
		vx := sxx - sx*sx/m
		cxy := sxy - sx*sy/m
		r := syy - sy*sy/m
		if vx > 1e-12 {
			r -= cxy * cxy / vx
		}
		return math.Max(r, 0)
	}
	best, bestCost, bestRaw := -1, math.Inf(1), math.Inf(1)
	for k := max(minBaselinePoints, n/10); k <= n-minIncidentPoints; k++ {
		nl, nr := float64(k), float64(n-k)
		raw := nl*math.Log(sse(0, k)/nl+eps) + nr*math.Log(sse(k, n)/nr+eps)
		cost := raw
		if d := preferFrom.Sub(pts[k].T).Minutes(); d > 0 && span > 0 {
			cost += priorWeight * d / span
		}
		if best < 0 || cost < bestCost-1e-9 {
			best, bestCost, bestRaw = k, cost, raw
		}
	}
	single := float64(n) * math.Log(sse(0, n)/float64(n)+eps)
	if single-bestRaw < 3*math.Log(float64(n)) {
		return -1
	}
	return best
}

// baseline is a robust description of the pre-onset segment.
type baseline struct {
	med, scale float64
	slope      float64   // per minute; 0 when there is no clear trend
	tc         time.Time // time the median refers to
	tmax       time.Time // trend is not extrapolated beyond this
}

// trendAt is the trend offset to subtract from a value at t.
func (b baseline) trendAt(t time.Time) float64 {
	if b.slope == 0 {
		return 0
	}
	if t.After(b.tmax) {
		t = b.tmax
	}
	return b.slope * t.Sub(b.tc).Minutes()
}

func fitBaseline(pts []core.Point, isRatio bool, all []core.Point) baseline {
	bl := baseline{tc: pts[len(pts)/2].T}
	vals := sorted(values(pts))
	bl.med = median(vals)
	bl.scale = robustScale(vals, bl.med, isRatio, all)

	span := pts[len(pts)-1].T.Sub(pts[0].T)
	if span < minTrendSpan || len(pts) < 12 {
		return bl
	}
	third := len(pts) / 3
	first, last := pts[:third], pts[len(pts)-third:]
	dt := last[len(last)/2].T.Sub(first[len(first)/2].T).Minutes()
	slope := (median(sorted(values(last))) - median(sorted(values(first)))) / dt
	// Only de-trend a clear trend: one that moves the level by more than two
	// noise scales across the baseline.
	if math.Abs(slope)*span.Minutes() <= 2*bl.scale {
		return bl
	}
	bl.slope = slope
	bl.tmax = pts[len(pts)-1].T.Add(span)
	res := make([]float64, len(pts))
	for i, p := range pts {
		res[i] = p.V - bl.trendAt(p.T)
	}
	slices.Sort(res)
	bl.med = median(res)
	bl.scale = robustScale(res, bl.med, isRatio, all)
	return bl
}

// robustScale is max(1.4826*MAD, 5% of the reference level, absolute floor);
// see the package doc for the rule.
func robustScale(base []float64, med float64, isRatio bool, all []core.Point) float64 {
	dev := make([]float64, len(base))
	for i, v := range base {
		dev[i] = math.Abs(v - med)
	}
	slices.Sort(dev)
	scale := madToSigma * median(dev)
	ref := math.Abs(med)
	floor := 0.0
	if isRatio {
		if med >= 0 && med <= 1 {
			ref = math.Min(med, 1-med)
		}
		floor = ratioAbsFloor
	} else {
		for _, p := range all {
			floor = math.Max(floor, math.Abs(p.V))
		}
		floor *= maxAbsFrac
	}
	scale = math.Max(scale, relFloor*ref)
	scale = math.Max(scale, floor)
	return math.Max(scale, 1e-12)
}

func movedFrom(seg []core.Point, bl baseline, dir string) bool {
	m := 0.0
	for _, p := range seg {
		m += p.V - bl.trendAt(p.T)
	}
	m /= float64(len(seg))
	if dir == Up {
		return m > bl.med
	}
	return m < bl.med
}

func beyond(v, med, d float64, dir string) bool {
	if dir == Up {
		return v > med+d
	}
	return v < med-d
}

// clean drops non-finite points and returns a time-sorted copy.
func clean(in []core.Point) []core.Point {
	out := make([]core.Point, 0, len(in))
	for _, p := range in {
		if !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

// splitIndex returns the index of the first point at or after t.
func splitIndex(pts []core.Point, t time.Time) int {
	return sort.Search(len(pts), func(i int) bool { return !pts[i].T.Before(t) })
}

func values(pts []core.Point) []float64 {
	v := make([]float64, len(pts))
	for i, p := range pts {
		v[i] = p.V
	}
	return v
}

func sorted(v []float64) []float64 {
	c := slices.Clone(v)
	slices.Sort(c)
	return c
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func median(s []float64) float64 { return quantile(s, 0.5) }

// quantile of an already sorted slice, with linear interpolation.
func quantile(s []float64, q float64) float64 {
	if len(s) == 0 {
		return 0
	}
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func copyLabels(l map[string]string) map[string]string {
	if l == nil {
		return nil
	}
	c := make(map[string]string, len(l))
	for k, v := range l {
		c[k] = v
	}
	return c
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
		b.WriteByte(',')
	}
	return b.String()
}
