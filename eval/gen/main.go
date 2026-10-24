// Command gen writes the fixture scenarios used by `culprit eval`.
//
// Every scenario is synthesised from a small model of a shop (frontend ->
// catalog, cart, search; catalog -> postgres; cart -> redis; search ->
// catalog). Output is fully deterministic: the PRNG is seeded from the
// scenario name and all timestamps hang off fixed alert times, so
//
//	go run ./eval/gen -out eval/scenarios
//
// regenerates byte-identical files.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

const (
	step            = 15 * time.Second
	defaultLookback = 2 * time.Hour
	after           = 30 * time.Minute

	accessLogsPerSec = 0.3 // sampled access-log lines per service at baseline traffic

	dbqBase   = 0.012 // catalog db_query p99 seconds
	poolBase  = 0.22  // catalog db pool utilisation
	cacheBase = 0.95  // cart cache hit ratio
)

func main() {
	set := flag.String("set", "dev", "scenario set: dev (eval/scenarios) or heldout (eval/heldout)")
	out := flag.String("out", "", "directory to write scenario directories into (default eval/scenarios or eval/heldout)")
	flag.Parse()
	if *out == "" {
		*out = "eval/scenarios"
		if *set == "heldout" {
			*out = "eval/heldout"
		}
	}
	if err := generate(*out, *set); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

// scenarioSet returns the scenarios of a named set.
func scenarioSet(set string) ([]scenario, error) {
	switch set {
	case "dev":
		return scenarios(), nil
	case "heldout":
		return heldoutScenarios(), nil
	}
	return nil, fmt.Errorf("unknown set %q (want dev or heldout)", set)
}

// generate writes every scenario of set under outDir.
func generate(outDir, set string) error {
	list, err := scenarioSet(set)
	if err != nil {
		return err
	}
	for _, sc := range list {
		files, err := render(sc)
		if err != nil {
			return fmt.Errorf("%s: %w", sc.name, err)
		}
		dir := filepath.Join(outDir, sc.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, name := range sortedKeys(files) {
			if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type metricKey int

const (
	mLat metricKey = iota
	mRate
	mErr
	mCPU
	mMem
	mPool
	mDBQ
	mCache
	mLag
	mDisk
)

var metricNames = map[metricKey]string{
	mLat:   "http_request_duration_seconds:p99",
	mRate:  "http_requests:rate1m",
	mErr:   "http_errors:ratio1m",
	mCPU:   "process_cpu:ratio",
	mMem:   "process_memory:bytes",
	mPool:  "db_pool_in_use:ratio",
	mDBQ:   "db_query_duration_seconds:p99",
	mCache: "cache_hit:ratio",
	mLag:   "kafka_consumer_lag:messages",
	mDisk:  "disk_used:ratio",
}

type svcSpec struct {
	name                     string
	rate, lat, err, cpu, mem float64
}

// world is a topology with its own services, metric model and log
// vocabulary. The dev set uses shop; the held-out set uses another world.
type world struct {
	http, infra []svcSpec
	topology    []topoEntry
	series      [][2]any
	compute     func(sc *scenario, t time.Time, rel time.Duration, phase float64) map[string]vals
	access      func(r *rand.Rand, svc string, v vals) (level, msg string)
	background  []periodic
	benign      []benignTmpl
	versions    map[string][2]int
	seedSalt    string
	logless     []string // topology nodes that write no logs (external APIs)
}

type topoEntry struct {
	svc   string
	calls []string
}

func (w *world) spec(name string) svcSpec {
	for _, s := range w.http {
		if s.name == name {
			return s
		}
	}
	for _, s := range w.infra {
		if s.name == name {
			return s
		}
	}
	panic("unknown service " + name)
}

var shop = &world{
	http:       httpSpecs,
	infra:      infraSpecs,
	topology:   topologyOrder,
	series:     seriesList(),
	compute:    computeShop,
	access:     accessLine,
	background: background,
	benign:     benignPool,
	versions:   baseVersion,
}

func (sc *scenario) world() *world {
	if sc.w != nil {
		return sc.w
	}
	return shop
}

var httpSpecs = []svcSpec{
	{"frontend", 120, 0.42, 0.002, 0.35, 310e6},
	{"catalog", 95, 0.16, 0.001, 0.30, 420e6},
	{"cart", 38, 0.07, 0.0015, 0.22, 250e6},
	{"search", 42, 0.28, 0.001, 0.40, 610e6},
}

var infraSpecs = []svcSpec{
	{name: "postgres", cpu: 0.22, mem: 2.4e9},
	{name: "redis", cpu: 0.07, mem: 380e6},
}

// spec looks up a shop service (kept free of shop to avoid an init cycle).
func spec(name string) svcSpec { return (&world{http: httpSpecs, infra: infraSpecs}).spec(name) }

// topologyOrder is the order topology keys are written in.
var topologyOrder = []topoEntry{
	{"frontend", []string{"catalog", "cart", "search"}},
	{"catalog", []string{"postgres"}},
	{"cart", []string{"redis"}},
	{"search", []string{"catalog"}},
}

// series is the fixed list of emitted series: (service, metric).
func seriesList() [][2]any {
	var out [][2]any
	for _, s := range httpSpecs {
		for _, m := range []metricKey{mLat, mRate, mErr, mCPU, mMem} {
			out = append(out, [2]any{s.name, m})
		}
	}
	out = append(out, [2]any{"catalog", mPool}, [2]any{"catalog", mDBQ}, [2]any{"cart", mCache})
	for _, s := range infraSpecs {
		out = append(out, [2]any{s.name, mCPU}, [2]any{s.name, mMem})
	}
	return out
}

// window describes when something is active, relative to the alert time.
// Weight ramps linearly from 0 at start to 1 at start+ramp, and back to 0
// over ramp after end (if hasEnd). shape, when set, overrides all of this.
type window struct {
	start, ramp, end time.Duration
	hasEnd           bool
	shape            func(rel time.Duration) float64
}

func (w window) weight(rel time.Duration) float64 {
	if w.shape != nil {
		return w.shape(rel)
	}
	if rel < w.start {
		return 0
	}
	f := 1.0
	if w.ramp > 0 {
		f = math.Min(1, float64(rel-w.start)/float64(w.ramp))
	}
	if w.hasEnd && rel > w.end {
		down := 0.0
		if w.ramp > 0 {
			down = 1 - float64(rel-w.end)/float64(w.ramp)
		}
		f = math.Min(f, math.Max(0, down))
	}
	return f
}

type opKind int

const (
	opMul opKind = iota // v *= 1 + (val-1)*w
	opAdd               // v += val*w
	opSet               // v moves towards val by w
)

// effect perturbs one service metric before propagation.
type effect struct {
	window
	svc string
	m   metricKey
	op  opKind
	val float64
}

func (e effect) apply(v float64, rel time.Duration) float64 {
	w := e.weight(rel)
	if w == 0 {
		return v
	}
	switch e.op {
	case opMul:
		return v * (1 + (e.val-1)*w)
	case opAdd:
		return v + e.val*w
	default:
		return v + (e.val-v)*w
	}
}

func eff(svc string, m metricKey, op opKind, val float64, start, ramp time.Duration) effect {
	return effect{window: window{start: start, ramp: ramp}, svc: svc, m: m, op: op, val: val}
}

func (e effect) until(end time.Duration) effect {
	e.end, e.hasEnd = end, true
	return e
}

func shaped(svc string, m metricKey, op opKind, val float64, fn func(time.Duration) float64) effect {
	return effect{window: window{shape: fn}, svc: svc, m: m, op: op, val: val}
}

// emitter writes log lines at rate lines/sec (scaled by its window weight).
type emitter struct {
	window
	svc, level string
	rate       float64
	msg        func(r *rand.Rand, t time.Time) string
}

func emit(svc, level string, rate float64, start, ramp time.Duration, msg func(r *rand.Rand, t time.Time) string) emitter {
	return emitter{window: window{start: start, ramp: ramp}, svc: svc, level: level, rate: rate, msg: msg}
}

func (e emitter) until(end time.Duration) emitter {
	e.end, e.hasEnd = end, true
	return e
}

// always is a window active for the whole scenario.
var always = window{start: -24 * time.Hour}

type oneshot struct {
	rel        time.Duration
	svc, level string
	msg        string
	labels     map[string]string
}

// gap removes a service's metric points (scrape target down).
type gap struct {
	svc        string
	start, end time.Duration
}

type changeDef struct {
	id, kind, svc   string
	rel             time.Duration
	author, summary string
	diff            string
	logMsg          string // overrides the auto-generated rollout log line
}

type truth struct {
	service, changeID, category string
	keywords                    []string
}

type scenario struct {
	name, description string
	title, symptom    string
	alert             time.Time
	truth             truth
	effects           []effect
	emitters          []emitter
	oneshots          []oneshot
	gaps              []gap
	changes           []changeDef
	benign            int           // number of random benign changes to add
	lookback          time.Duration // default 2h
	w                 *world        // default shop
	mute              []mute        // background log lines to drop
	accessRate        float64       // access-log lines/sec per service, default accessLogsPerSec
}

// mute drops log lines of svc containing contains between start and end
// (e.g. a periodic success message during an outage).
type mute struct {
	svc, contains string
	start, end    time.Duration
}

func (sc *scenario) muted(l core.LogLine) bool {
	rel := l.T.Sub(sc.alert)
	for _, m := range sc.mute {
		if l.Service == m.svc && rel >= m.start && rel < m.end && strings.Contains(l.Message, m.contains) {
			return true
		}
	}
	return false
}

func (sc *scenario) lb() time.Duration {
	if sc.lookback > 0 {
		return sc.lookback
	}
	return defaultLookback
}

// restartEffects models every deploy or infra change as a process restart:
// memory drops to ~55% and climbs back over 10 minutes.
func restartEffects(changes []changeDef) []effect {
	var out []effect
	for _, c := range changes {
		if c.kind != "deploy" && c.kind != "infra" {
			continue
		}
		t := c.rel + 6*time.Second
		out = append(out, shaped(c.svc, mMem, opMul, 0.55, func(rel time.Duration) float64 {
			if rel < t || rel > t+10*time.Minute {
				return 0
			}
			return 1 - float64(rel-t)/float64(10*time.Minute)
		}))
	}
	return out
}

// vals is one service's metric values at one step.
type vals map[metricKey]float64

// traffic is the global traffic factor: a diurnal curve peaking mid
// afternoon UTC plus a slow wiggle.
func traffic(t time.Time, phase float64) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60 + float64(t.Second())/3600
	d := 1 + 0.35*math.Sin(2*math.Pi*(h-9)/24)
	w := 1 + 0.04*math.Sin(2*math.Pi*float64(t.Unix()%3600)/900+phase)
	return d * w
}

func sat(cpu float64) float64 { return 1 + math.Max(0, cpu-0.75)*12 }

func satErr(cpu float64) float64 { return math.Max(0, cpu-0.9) * 1.5 }

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// computeShop returns noiseless metric values for every shop service at rel.
func computeShop(sc *scenario, t time.Time, rel time.Duration, phase float64) map[string]vals {
	tf := traffic(t, phase)
	own := func(svc string, m metricKey, v float64) float64 {
		for _, e := range sc.effects {
			if e.svc == svc && e.m == m {
				v = e.apply(v, rel)
			}
		}
		return v
	}
	mo := map[string]float64{}
	for _, s := range httpSpecs {
		mo[s.name] = own(s.name, mRate, 1)
	}
	mult := map[string]float64{}
	mult["frontend"] = mo["frontend"]
	mult["cart"] = mo["cart"] * mult["frontend"]
	mult["search"] = mo["search"] * mult["frontend"]
	mult["catalog"] = mo["catalog"] * (0.7*mult["frontend"] + 0.3*mult["search"])

	out := map[string]vals{}
	dbq := own("catalog", mDBQ, dbqBase*(1+0.15*math.Max(0, mult["catalog"]-1)))
	pool := own("catalog", mPool, poolBase*tf*mult["catalog"]*math.Sqrt(dbq/dbqBase))
	cache := own("cart", mCache, cacheBase)

	base := map[string]float64{}
	for _, s := range httpSpecs {
		v := vals{}
		v[mRate] = s.rate * tf * mult[s.name]
		v[mCPU] = clamp(own(s.name, mCPU, s.cpu*tf*math.Pow(math.Max(mult[s.name], 0), 0.9)), 0.005, 0.995)
		base[s.name] = s.lat * (0.85 + 0.15*tf)
		lat := base[s.name] * sat(v[mCPU])
		switch s.name {
		case "catalog":
			lat += math.Max(0, dbq-dbqBase) * 8
		case "cart":
			lat += math.Max(0, cacheBase-cache) * 0.9
		}
		v[mLat] = own(s.name, mLat, lat)
		v[mErr] = own(s.name, mErr, s.err) + satErr(v[mCPU])
		v[mMem] = own(s.name, mMem, s.mem*(1+0.02*tf))
		out[s.name] = v
	}
	dl := func(s string) float64 { return math.Max(0, out[s][mLat]-base[s]) }
	de := func(s string) float64 { return math.Max(0, out[s][mErr]-spec(s).err) }
	// search calls catalog.
	out["search"][mLat] += 0.9 * dl("catalog")
	out["search"][mErr] += 0.8 * de("catalog")
	// frontend calls everyone.
	ls := []float64{0.9 * dl("catalog"), 0.8 * dl("cart"), 0.8 * dl("search")}
	out["frontend"][mLat] += 0.85*math.Max(ls[0], math.Max(ls[1], ls[2])) + 0.15*(ls[0]+ls[1]+ls[2])
	out["frontend"][mErr] += 0.55*de("catalog") + 0.25*de("cart") + 0.2*de("search")
	for _, s := range httpSpecs {
		out[s.name][mLat] = clamp(out[s.name][mLat], 0.001, 30)
		out[s.name][mErr] = clamp(out[s.name][mErr], 0, 1)
	}
	out["catalog"][mPool] = clamp(pool, 0, 1)
	out["catalog"][mDBQ] = dbq
	out["cart"][mCache] = clamp(cache, 0, 1)

	pg, rd := spec("postgres"), spec("redis")
	out["postgres"] = vals{
		mCPU: clamp(own("postgres", mCPU, pg.cpu*tf*mult["catalog"]*math.Pow(dbq/dbqBase, 0.3)), 0.005, 0.995),
		mMem: own("postgres", mMem, pg.mem),
	}
	out["redis"] = vals{
		mCPU: clamp(own("redis", mCPU, rd.cpu*tf*mult["cart"]), 0.005, 0.995),
		mMem: own("redis", mMem, rd.mem),
	}
	return out
}

// noise applies measurement noise and rounds to a sensible precision.
func noise(r *rand.Rand, errBase float64, m metricKey, v float64) float64 {
	n := r.NormFloat64()
	switch m {
	case mRate:
		return round(math.Max(0, v*(1+0.03*n)), 2)
	case mLat:
		return round(math.Max(0.001, v*(1+0.07*n)), 4)
	case mErr:
		return round(clamp(v*(1+0.1*r.NormFloat64())+errBase*0.5*n, 0, 1), 5)
	case mCPU:
		return round(clamp(v*(1+0.03*n), 0.001, 1), 3)
	case mMem:
		return math.Round(v * (1 + 0.004*n))
	case mPool:
		return round(clamp(v+0.02*n, 0, 1), 3)
	case mDBQ:
		return round(math.Max(0.0005, v*(1+0.08*n)), 5)
	case mLag:
		return math.Round(math.Max(0, v*(1+0.1*n)))
	case mDisk:
		return round(clamp(v+0.001*n, 0, 1), 4)
	default: // cache
		return round(clamp(v+0.005*n, 0, 1), 4)
	}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func seedFor(name string) (uint64, uint64) {
	h := fnv.New64a()
	h.Write([]byte(name))
	a := h.Sum64()
	h.Write([]byte("/second"))
	return a, h.Sum64()
}

func render(sc scenario) (map[string][]byte, error) {
	w := sc.world()
	s1, s2 := seedFor(w.seedSalt + sc.name)
	r := rand.New(rand.NewPCG(s1, s2))
	phase := r.Float64() * 2 * math.Pi
	start := sc.alert.Add(-sc.lb())
	changes := sc.allChanges(r)
	sc.effects = append(restartEffects(changes), sc.effects...)
	end := sc.alert.Add(after)

	// Metrics.
	list := w.series
	points := make([][][2]float64, len(list))
	var stepVals []map[string]vals // noiseless, for logs
	for t := start; !t.After(end); t = t.Add(step) {
		rel := t.Sub(sc.alert)
		v := w.compute(&sc, t, rel, phase)
		stepVals = append(stepVals, v)
		for i, k := range list {
			svc, m := k[0].(string), k[1].(metricKey)
			errBase := 0.0
			if m == mErr {
				errBase = w.spec(svc).err
			}
			x := noise(r, errBase, m, v[svc][m])
			if sc.inGap(svc, rel) {
				continue
			}
			points[i] = append(points[i], [2]float64{float64(t.Unix()), x})
		}
	}
	var mb bytes.Buffer
	mb.WriteString("[\n")
	for i, k := range list {
		svc, m := k[0].(string), k[1].(metricKey)
		labels, _ := json.Marshal(map[string]string{"service": svc, "env": "prod"})
		fmt.Fprintf(&mb, `{"name":%q,"labels":%s,"points":[`, metricNames[m], labels)
		for j, p := range points[i] {
			if j > 0 {
				mb.WriteByte(',')
			}
			mb.WriteByte('[')
			mb.WriteString(strconv.FormatInt(int64(p[0]), 10))
			mb.WriteByte(',')
			mb.WriteString(strconv.FormatFloat(p[1], 'f', -1, 64))
			mb.WriteByte(']')
		}
		mb.WriteString("]}")
		if i < len(list)-1 {
			mb.WriteByte(',')
		}
		mb.WriteByte('\n')
	}
	mb.WriteString("]\n")

	// Changes.
	cs := make([]core.Change, 0, len(changes))
	for _, c := range changes {
		cs = append(cs, core.Change{
			ID: c.id, Kind: c.kind, Service: c.svc, T: sc.alert.Add(c.rel),
			Author: c.author, Summary: c.summary, Diff: c.diff, URL: changeURL(c),
		})
	}
	cb, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return nil, err
	}
	cb = append(cb, '\n')

	// Logs.
	lines := sc.logs(r, start, stepVals, changes)
	var lb bytes.Buffer
	bw := bufio.NewWriter(&lb)
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		bw.Write(b)
		bw.WriteByte('\n')
	}
	bw.Flush()

	return map[string][]byte{
		"scenario.yaml": sc.yaml(),
		"metrics.json":  mb.Bytes(),
		"changes.json":  cb,
		"logs.jsonl":    lb.Bytes(),
	}, nil
}

func (sc *scenario) inGap(svc string, rel time.Duration) bool {
	for _, g := range sc.gaps {
		if g.svc == svc && rel >= g.start && rel < g.end {
			return true
		}
	}
	return false
}

func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (sc *scenario) yaml() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "name: %s\n", sc.name)
	fmt.Fprintf(&b, "description: %s\n", q(sc.description))
	b.WriteString("incident:\n")
	fmt.Fprintf(&b, "  title: %s\n", q(sc.title))
	fmt.Fprintf(&b, "  service: %s\n", sc.symptom)
	fmt.Fprintf(&b, "  started_at: %q\n", sc.alert.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  lookback: %s\n", strings.TrimSuffix(strings.TrimSuffix(sc.lb().String(), "0s"), "0m"))
	fmt.Fprintf(&b, "  after: %s\n", "30m")
	b.WriteString("topology:\n")
	for _, e := range sc.world().topology {
		fmt.Fprintf(&b, "  %s: [%s]\n", e.svc, strings.Join(e.calls, ", "))
	}
	b.WriteString("truth:\n")
	fmt.Fprintf(&b, "  service: %s\n", sc.truth.service)
	if sc.truth.changeID != "" {
		fmt.Fprintf(&b, "  change_id: %s\n", sc.truth.changeID)
	}
	fmt.Fprintf(&b, "  category: %s\n", sc.truth.category)
	kw := make([]string, len(sc.truth.keywords))
	for i, k := range sc.truth.keywords {
		kw[i] = q(k)
	}
	fmt.Fprintf(&b, "  keywords: [%s]\n", strings.Join(kw, ", "))
	return b.Bytes()
}

func changeURL(c changeDef) string {
	switch c.kind {
	case "deploy":
		return fmt.Sprintf("https://deploy.example.internal/%s/releases/%s", c.svc, versionOf(c.id))
	case "commit":
		return fmt.Sprintf("https://git.example.internal/shop/%s/commit/%s", c.svc, strings.TrimPrefix(c.id, "commit-"))
	case "flag":
		return "https://flags.example.internal/audit/" + c.id
	default:
		return "https://git.example.internal/shop/infra/changes/" + c.id
	}
}

func versionOf(id string) string {
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func hexID(r *rand.Rand, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[r.IntN(16)]
	}
	return string(b)
}

// allChanges returns the scenario's explicit changes plus random benign ones,
// sorted by time.
func (sc *scenario) allChanges(r *rand.Rand) []changeDef {
	out := append([]changeDef{}, sc.changes...)
	used := map[string]bool{}
	for _, c := range out {
		used[c.id] = true
	}
	for i := 0; i < sc.benign; i++ {
		c := benignChange(r, sc.world())
		for used[c.id] {
			c = benignChange(r, sc.world())
		}
		used[c.id] = true
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rel != out[j].rel {
			return out[i].rel < out[j].rel
		}
		return out[i].id < out[j].id
	})
	return out
}

var authors = []string{"maria.k", "dev.patel", "j.okafor", "lin.chen", "sam.r", "a.novak", "deploy-bot"}

type benignTmpl struct {
	kind, svc, summary, diff string
}

var benignPool = []benignTmpl{
	{"deploy", "frontend", "frontend %s: update product card copy and footer links", "--- a/web/templates/product_card.html\n+++ b/web/templates/product_card.html\n@@ -12,7 +12,7 @@\n-  <span class=\"cta\">Add to basket</span>\n+  <span class=\"cta\">Add to cart</span>\n"},
	{"deploy", "frontend", "frontend %s: bump golang.org/x/net to v0.38.0", "--- a/go.mod\n+++ b/go.mod\n@@ -5,7 +5,7 @@\n-\tgolang.org/x/net v0.37.0\n+\tgolang.org/x/net v0.38.0\n"},
	{"deploy", "catalog", "catalog %s: add request id to access logs", "--- a/internal/http/middleware.go\n+++ b/internal/http/middleware.go\n@@ -40,6 +40,7 @@ func accessLog(next http.Handler) http.Handler {\n \t\tslog.Info(\"request\",\n+\t\t\t\"req\", RequestID(r.Context()),\n \t\t\t\"path\", r.URL.Path,\n"},
	{"deploy", "catalog", "catalog %s: refactor category handler tests", "--- a/internal/http/category_test.go\n+++ b/internal/http/category_test.go\n@@ -1,5 +1,5 @@\n-func TestCategory(t *testing.T) {\n+func TestCategoryHandler(t *testing.T) {\n"},
	{"deploy", "cart", "cart %s: add checkout funnel step metric", "--- a/internal/checkout/metrics.go\n+++ b/internal/checkout/metrics.go\n@@ -8,3 +8,8 @@\n+var funnelStep = prometheus.NewCounterVec(\n+\tprometheus.CounterOpts{Name: \"checkout_funnel_step_total\"},\n+\t[]string{\"step\"},\n+)\n"},
	{"deploy", "cart", "cart %s: increase readiness probe initialDelaySeconds 5 -> 10", "--- a/deploy/cart.yaml\n+++ b/deploy/cart.yaml\n@@ -31,7 +31,7 @@\n         readinessProbe:\n-          initialDelaySeconds: 5\n+          initialDelaySeconds: 10\n"},
	{"deploy", "search", "search %s: update synonyms list for outdoor category", "--- a/data/synonyms.txt\n+++ b/data/synonyms.txt\n@@ -88,3 +88,5 @@\n+waterproof, rainproof\n+beanie, wool hat\n"},
	{"deploy", "search", "search %s: upgrade tokenizer library to v2.3.1", "--- a/go.mod\n+++ b/go.mod\n@@ -9,7 +9,7 @@\n-\tgithub.com/example/tokenize v2.3.0\n+\tgithub.com/example/tokenize v2.3.1\n"},
	{"commit", "frontend", "Fix typo in checkout confirmation email", "--- a/templates/email/confirm.txt\n+++ b/templates/email/confirm.txt\n@@ -3 +3 @@\n-Thanks for you order!\n+Thanks for your order!\n"},
	{"commit", "catalog", "Add index hint comment to products_by_category query", "--- a/internal/store/queries.sql\n+++ b/internal/store/queries.sql\n@@ -20,6 +20,7 @@\n+-- uses idx_products_category_id\n SELECT p.id, p.name, p.price FROM products p WHERE p.category_id = $1\n"},
	{"commit", "cart", "Remove unused coupon v1 code path", "--- a/internal/coupon/v1.go\n+++ /dev/null\n@@ -1,42 +0,0 @@\n-package coupon\n-// Deprecated: v1 coupons were retired in 2025.\n"},
	{"commit", "search", "Document ranking parameters in README", "--- a/README.md\n+++ b/README.md\n@@ -40,3 +40,9 @@\n+## Ranking\n+BM25 with k1=1.2, b=0.75.\n"},
	{"flag", "frontend", "frontend: enable new-footer for 25% of sessions", "new_footer:\n-  rollout: 0\n+  rollout: 25\n"},
	{"flag", "catalog", "catalog: enable recs-v2 in shadow mode (no user impact)", "recs_v2:\n-  mode: off\n+  mode: shadow\n"},
	{"flag", "search", "search: enable typo_tolerance for 10% of queries", "typo_tolerance:\n-  rollout: 0\n+  rollout: 10\n"},
	{"config", "search", "search: raise index refresh interval 60s -> 120s", "--- a/config/search.yaml\n+++ b/config/search.yaml\n@@ -4 +4 @@\n-index_refresh: 60s\n+index_refresh: 120s\n"},
	{"config", "cart", "cart: raise log sampling 1% -> 5%", "--- a/config/cart.yaml\n+++ b/config/cart.yaml\n@@ -2 +2 @@\n-log_sample: 0.01\n+log_sample: 0.05\n"},
}

var baseVersion = map[string][2]int{"frontend": {4, 10}, "catalog": {1, 7}, "cart": {2, 2}, "search": {3, 1}}

func benignChange(r *rand.Rand, w *world) changeDef {
	t := w.benign[r.IntN(len(w.benign))]
	rel := -time.Duration(30+r.IntN(88))*time.Minute - time.Duration(r.IntN(60))*time.Second
	c := changeDef{kind: t.kind, svc: t.svc, rel: rel, author: authors[r.IntN(len(authors))], diff: t.diff}
	switch t.kind {
	case "deploy":
		v := w.versions[t.svc]
		ver := fmt.Sprintf("%d.%d.%d", v[0], v[1], 1+r.IntN(9))
		c.id = fmt.Sprintf("deploy-%s-%s", t.svc, ver)
		c.summary = fmt.Sprintf(t.summary, ver)
	case "commit":
		c.id = "commit-" + hexID(r, 7)
		c.summary = t.summary
	case "flag":
		c.id = fmt.Sprintf("flag-%s-%s", t.svc, hexID(r, 4))
		c.summary = t.summary
	default:
		c.id = fmt.Sprintf("cfg-%s-%s", t.svc, hexID(r, 4))
		c.summary = t.summary
	}
	return c
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

var (
	products   = func(r *rand.Rand) int { return 1000 + r.IntN(9000) }
	terms      = []string{"boots", "rain+jacket", "wool+socks", "backpack", "headlamp", "tent", "gloves", "thermos", "kettle", "trail+shoes"}
	categories = []string{"outdoor", "kitchen", "garden", "toys", "books", "apparel"}
)

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }

func accessLine(r *rand.Rand, svc string, v vals) (level, msg string) {
	var method, path string
	notFound := false
	switch svc {
	case "frontend":
		switch x := r.IntN(10); {
		case x < 3:
			method, path = "GET", "/"
		case x < 6:
			method, path = "GET", fmt.Sprintf("/products/%d", products(r))
		case x < 8:
			method, path = "GET", "/search?q="+pick(r, terms)
		case x < 9:
			method, path = "POST", "/cart/items"
		default:
			method, path = "POST", "/checkout"
		}
	case "catalog":
		switch x := r.IntN(10); {
		case x < 6:
			method, path = "GET", fmt.Sprintf("/api/products/%d", products(r))
		case x < 9:
			method, path = "GET", "/api/products?category="+pick(r, categories)
		default:
			method, path = "GET", "/api/categories"
		}
	case "cart":
		id := hexID(r, 8)
		switch x := r.IntN(10); {
		case x < 6:
			method, path = "GET", "/api/cart/"+id
		case x < 9:
			method, path = "POST", "/api/cart/"+id+"/items"
		default:
			method, path = "DELETE", fmt.Sprintf("/api/cart/%s/items/%d", id, products(r))
		}
	default:
		if r.IntN(3) == 0 {
			method, path = "GET", "/v1/suggest?prefix="+pick(r, terms)[:3]
		} else {
			method, path = "GET", "/v1/query?q="+pick(r, terms)
		}
	}
	status := 200
	if method == "POST" {
		status = 201
	}
	x := r.Float64()
	switch {
	case x < v[mErr]:
		status = pick(r, []int{500, 502, 503, 504})
		if svc != "frontend" {
			status = pick(r, []int{500, 503})
		}
	case x < v[mErr]+0.012 && method == "GET" && strings.Contains(path, "/products/"):
		status, notFound = 404, true
	}
	ms := int(v[mLat] * 1000 * (0.1 + 0.7*r.Float64()))
	if notFound {
		ms = 2 + r.IntN(8)
	}
	level = "info"
	if status >= 500 {
		level = "error"
	}
	return level, fmt.Sprintf("%s %s %d %dms", method, path, status, ms)
}

// periodic is a background message emitted every ~every.
type periodic struct {
	svc, level string
	every      time.Duration
	msg        func(r *rand.Rand) string
}

var background = []periodic{
	{"frontend", "info", 5 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("session store: pruned %d expired sessions", 150+r.IntN(200))
	}},
	{"frontend", "info", 3 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("GET %s 404 %dms", pick(r, []string{"/wp-login.php", "/.env", "/admin/config.php", "/favicon.ico"}), 1+r.IntN(3))
	}},
	{"frontend", "warn", 17 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("slow template render for /products/%d: %dms", products(r), 900+r.IntN(500))
	}},
	{"catalog", "info", 2 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("price cache refreshed: %d items in %dms", 4700+r.IntN(200), 180+r.IntN(80))
	}},
	{"catalog", "warn", 11 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("slow query products_by_category took %dms", 450+r.IntN(300))
	}},
	{"cart", "info", 10 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("abandoned cart sweep removed %d carts", 20+r.IntN(40))
	}},
	{"cart", "warn", 14 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("redis command retried after timeout (attempt 2, %dms)", 40+r.IntN(60))
	}},
	{"search", "info", 5 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("index refresh completed: %d docs in %d.%ds", 48000+r.IntN(500), 2+r.IntN(3), r.IntN(10))
	}},
	{"search", "warn", 6 * time.Minute, func(r *rand.Rand) string {
		return "suggest: prefix too short, skipping"
	}},
	{"postgres", "info", 5 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("checkpoint complete: wrote %d buffers (%.1f%%); 0 WAL file(s) added, 0 removed, 1 recycled; write=%d.%03d s", 1500+r.IntN(900), 1.5+r.Float64(), 25+r.IntN(5), r.IntN(1000))
	}},
	{"postgres", "info", 40 * time.Second, func(r *rand.Rand) string {
		return fmt.Sprintf("connection authorized: user=%s database=shop application_name=%s", "catalog", "catalog")
	}},
	{"postgres", "info", 7 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("duration: %d.%03d ms  statement: SELECT count(*) FROM orders WHERE created_at > now() - interval '1 hour'", 120+r.IntN(80), r.IntN(1000))
	}},
	{"redis", "info", 15 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("%d changes in 60 seconds. Saving...", 1000+r.IntN(4000))
	}},
	{"redis", "info", 15 * time.Minute, func(r *rand.Rand) string {
		return "DB saved on disk"
	}},
}

func poisson(r *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 { // normal approximation
		return int(math.Max(0, math.Round(lambda+math.Sqrt(lambda)*r.NormFloat64())))
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

func jitter(r *rand.Rand, t time.Time) time.Time {
	return t.Add(time.Duration(r.IntN(int(step/time.Millisecond))) * time.Millisecond)
}

func (sc *scenario) logs(r *rand.Rand, start time.Time, stepVals []map[string]vals, changes []changeDef) []core.LogLine {
	var lines []core.LogLine
	add := func(t time.Time, svc, level, msg string, labels map[string]string) {
		lines = append(lines, core.LogLine{T: t.UTC().Truncate(time.Millisecond), Service: svc, Level: level, Message: msg, Labels: labels})
	}
	silenced := func(svc string, rel time.Duration) bool { return sc.inGap(svc, rel) }

	// Access logs and incident emitters, step by step.
	accessRate := sc.accessRate
	if accessRate == 0 {
		accessRate = accessLogsPerSec
	}
	for i, v := range stepVals {
		t := start.Add(time.Duration(i) * step)
		rel := t.Sub(sc.alert)
		for _, s := range sc.world().http {
			n := poisson(r, accessRate*step.Seconds()*v[s.name][mRate]/s.rate)
			for k := 0; k < n; k++ {
				lvl, msg := sc.world().access(r, s.name, v[s.name])
				add(jitter(r, t), s.name, lvl, msg, nil)
			}
		}
		for _, e := range sc.emitters {
			n := poisson(r, e.rate*step.Seconds()*e.weight(rel))
			for k := 0; k < n; k++ {
				at := jitter(r, t)
				add(at, e.svc, e.level, e.msg(r, at), nil)
			}
		}
	}
	// Background periodic messages.
	end := sc.alert.Add(after)
	for _, p := range sc.world().background {
		t := start.Add(time.Duration(r.Int64N(int64(p.every))))
		for !t.After(end) {
			if !silenced(p.svc, t.Sub(sc.alert)) {
				add(t, p.svc, p.level, p.msg(r), nil)
			}
			t = t.Add(p.every + time.Duration(r.Int64N(int64(p.every/5))) - p.every/10)
		}
	}
	// Rollout logs for changes.
	for _, c := range changes {
		t := sc.alert.Add(c.rel)
		switch c.kind {
		case "deploy":
			sha := hexID(r, 7)
			add(t.Add(2*time.Second), c.svc, "info", "received SIGTERM, draining connections", nil)
			msg := fmt.Sprintf("starting %s version %s (commit %s)", c.svc, versionOf(c.id), sha)
			if c.logMsg != "" {
				msg = c.logMsg
			}
			add(t.Add(6*time.Second), c.svc, "info", msg, nil)
			add(t.Add(7*time.Second), c.svc, "info", "listening on :8080", nil)
		case "config":
			msg := fmt.Sprintf("config reloaded from /etc/%s/config.yaml (%s)", c.svc, c.id)
			if c.logMsg != "" {
				msg = c.logMsg
			}
			add(t.Add(3*time.Second), c.svc, "info", msg, nil)
		case "flag":
			msg := fmt.Sprintf("feature flags updated (%s)", c.id)
			if c.logMsg != "" {
				msg = c.logMsg
			}
			add(t.Add(4*time.Second), c.svc, "info", msg, nil)
		case "infra":
			if c.logMsg != "" {
				add(t.Add(5*time.Second), c.svc, "info", c.logMsg, nil)
			}
		}
	}
	for _, o := range sc.oneshots {
		add(sc.alert.Add(o.rel), o.svc, o.level, o.msg, o.labels)
	}

	// Window filter + deterministic order.
	ws, we := sc.alert.Add(-sc.lb()), sc.alert.Add(after)
	kept := lines[:0]
	for _, l := range lines {
		if !l.T.Before(ws) && !l.T.After(we) && !sc.muted(l) {
			kept = append(kept, l)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if !a.T.Equal(b.T) {
			return a.T.Before(b.T)
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Message < b.Message
	})
	return kept
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
