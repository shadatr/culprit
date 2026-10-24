package main

// The held-out set: a different, larger topology (gateway, auth, orders,
// profile, inventory, payments, a kafka-fed fulfillment worker,
// notifications, mysql, valkey, kafka and two external APIs) and different
// failure modes. Nothing in Culprit may be tuned against these scenarios;
// see EVAL.md.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

const (
	invDBQ     = 0.006
	invPool    = 0.25
	profDBQ    = 0.009
	profPool   = 0.18
	authCache  = 0.97
	lagBase    = 40.0
	diskBase   = 0.61
	heldAccess = 0.11
)

var heldHTTP = []svcSpec{
	{"gateway", 150, 0.30, 0.002, 0.33, 280e6},
	{"auth", 25, 0.09, 0.001, 0.18, 210e6},
	{"orders", 30, 0.22, 0.0015, 0.28, 390e6},
	{"profile", 45, 0.12, 0.001, 0.20, 260e6},
	{"inventory", 50, 0.08, 0.001, 0.25, 340e6},
	{"payments", 12, 0.45, 0.003, 0.20, 300e6},
	{"fulfillment", 28, 0.35, 0.002, 0.30, 450e6},
	{"notifications", 20, 0.25, 0.002, 0.15, 230e6},
}

var heldInfra = []svcSpec{
	{name: "mysql", cpu: 0.25, mem: 6.1e9},
	{name: "valkey", cpu: 0.05, mem: 520e6},
	{name: "kafka", cpu: 0.20, mem: 3.2e9},
}

var heldTopology = []topoEntry{
	{"gateway", []string{"auth", "orders", "profile"}},
	{"auth", []string{"valkey"}},
	{"orders", []string{"inventory", "payments", "kafka"}},
	{"profile", []string{"mysql"}},
	{"inventory", []string{"mysql"}},
	{"payments", []string{"fx-api"}},
	{"fulfillment", []string{"kafka", "inventory", "notifications"}},
	{"notifications", []string{"mail-relay"}},
}

func heldSeries() [][2]any {
	var out [][2]any
	for _, s := range heldHTTP {
		for _, m := range []metricKey{mLat, mRate, mErr, mCPU, mMem} {
			out = append(out, [2]any{s.name, m})
		}
	}
	out = append(out,
		[2]any{"inventory", mPool}, [2]any{"inventory", mDBQ},
		[2]any{"profile", mPool}, [2]any{"profile", mDBQ},
		[2]any{"auth", mCache}, [2]any{"fulfillment", mLag},
		[2]any{"mysql", mCPU}, [2]any{"mysql", mMem}, [2]any{"mysql", mDisk},
		[2]any{"valkey", mCPU}, [2]any{"valkey", mMem},
		[2]any{"kafka", mCPU}, [2]any{"kafka", mMem},
	)
	return out
}

var heldout = &world{
	http:       heldHTTP,
	infra:      heldInfra,
	topology:   heldTopology,
	series:     heldSeries(),
	compute:    computeHeld,
	access:     heldAccessLine,
	background: heldBackground,
	benign:     heldBenign,
	versions: map[string][2]int{"gateway": {2, 14}, "auth": {5, 3}, "orders": {3, 8}, "profile": {1, 22},
		"inventory": {3, 1}, "payments": {5, 0}, "fulfillment": {0, 19}, "notifications": {2, 6}},
	seedSalt: "heldout/v1/",
	logless:  []string{"fx-api", "mail-relay"},
}

func heldSpec(name string) svcSpec {
	return (&world{http: heldHTTP, infra: heldInfra}).spec(name)
}

type dep struct {
	svc        string
	latW, errW float64
}

func computeHeld(sc *scenario, t time.Time, rel time.Duration, phase float64) map[string]vals {
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
	for _, s := range heldHTTP {
		mo[s.name] = own(s.name, mRate, 1)
	}
	mult := map[string]float64{}
	mult["gateway"] = mo["gateway"]
	for _, s := range []string{"auth", "orders", "profile"} {
		mult[s] = mo[s] * mult["gateway"]
	}
	mult["payments"] = mo["payments"] * mult["orders"]
	mult["fulfillment"] = mo["fulfillment"] * mult["orders"]
	mult["inventory"] = mo["inventory"] * (0.6*mult["orders"] + 0.4*mult["fulfillment"])
	mult["notifications"] = mo["notifications"] * mult["fulfillment"]

	dbq := map[string]float64{
		"inventory": own("inventory", mDBQ, invDBQ*(1+0.15*math.Max(0, mult["inventory"]-1))),
		"profile":   own("profile", mDBQ, profDBQ*(1+0.15*math.Max(0, mult["profile"]-1))),
	}
	pool := map[string]float64{
		"inventory": own("inventory", mPool, invPool*tf*mult["inventory"]*math.Sqrt(dbq["inventory"]/invDBQ)),
		"profile":   own("profile", mPool, profPool*tf*mult["profile"]*math.Sqrt(dbq["profile"]/profDBQ)),
	}
	dbqBaseOf := map[string]float64{"inventory": invDBQ, "profile": profDBQ}
	cache := own("auth", mCache, authCache)

	out := map[string]vals{}
	base := map[string]float64{}
	for _, s := range heldHTTP {
		v := vals{}
		v[mRate] = s.rate * tf * mult[s.name]
		v[mCPU] = clamp(own(s.name, mCPU, s.cpu*tf*math.Pow(math.Max(mult[s.name], 0), 0.9)), 0.005, 0.995)
		base[s.name] = s.lat * (0.85 + 0.15*tf)
		lat := base[s.name] * sat(v[mCPU])
		if b, ok := dbqBaseOf[s.name]; ok {
			lat += math.Max(0, dbq[s.name]-b) * 6
		}
		if s.name == "auth" {
			lat += math.Max(0, authCache-cache) * 0.3
		}
		v[mLat] = own(s.name, mLat, lat)
		v[mErr] = own(s.name, mErr, s.err) + satErr(v[mCPU])
		v[mMem] = own(s.name, mMem, s.mem*(1+0.02*tf))
		out[s.name] = v
	}
	dl := func(s string) float64 { return math.Max(0, out[s][mLat]-base[s]) }
	de := func(s string) float64 { return math.Max(0, out[s][mErr]-heldSpec(s).err) }
	propagate := func(svc string, deps []dep) {
		var mx, sum, errs float64
		for _, d := range deps {
			x := d.latW * dl(d.svc)
			mx = math.Max(mx, x)
			sum += x
			errs += d.errW * de(d.svc)
		}
		out[svc][mLat] += 0.85*mx + 0.15*sum
		out[svc][mErr] += errs
	}
	propagate("fulfillment", []dep{{"inventory", 0.6, 0.5}, {"notifications", 0.5, 0.6}})
	propagate("orders", []dep{{"inventory", 0.8, 0.6}, {"payments", 0.9, 0.7}})
	propagate("gateway", []dep{{"auth", 0.9, 0.4}, {"orders", 0.8, 0.4}, {"profile", 0.6, 0.2}})
	for _, s := range heldHTTP {
		out[s.name][mLat] = clamp(out[s.name][mLat], 0.001, 30)
		out[s.name][mErr] = clamp(out[s.name][mErr], 0, 1)
	}
	for _, s := range []string{"inventory", "profile"} {
		out[s][mPool] = clamp(pool[s], 0, 1)
		out[s][mDBQ] = dbq[s]
	}
	out["auth"][mCache] = clamp(cache, 0, 1)
	span := float64(sc.lb() + after)
	out["fulfillment"][mLag] = math.Max(0, own("fulfillment", mLag, lagBase*(0.8+0.2*tf)))

	my, vk, kf := heldSpec("mysql"), heldSpec("valkey"), heldSpec("kafka")
	load := 0.6*mult["inventory"]*math.Pow(dbq["inventory"]/invDBQ, 0.3) + 0.4*mult["profile"]*math.Pow(dbq["profile"]/profDBQ, 0.3)
	out["mysql"] = vals{
		mCPU:  clamp(own("mysql", mCPU, my.cpu*tf*load), 0.005, 0.995),
		mMem:  own("mysql", mMem, my.mem),
		mDisk: clamp(own("mysql", mDisk, diskBase+0.01*float64(rel+sc.lb())/span), 0, 1),
	}
	out["valkey"] = vals{
		mCPU: clamp(own("valkey", mCPU, vk.cpu*tf*mult["auth"]), 0.005, 0.995),
		mMem: own("valkey", mMem, vk.mem),
	}
	out["kafka"] = vals{
		mCPU: clamp(own("kafka", mCPU, kf.cpu*tf*mult["orders"]), 0.005, 0.995),
		mMem: own("kafka", mMem, kf.mem),
	}
	return out
}

var skus = []string{"TB-2201", "KX-0193", "WL-7710", "PN-3302", "RB-0045", "GL-8812", "HM-1290"}

func heldAccessLine(r *rand.Rand, svc string, v vals) (level, msg string) {
	id := func() int { return 100000 + r.IntN(900000) }
	var method, path string
	switch svc {
	case "gateway":
		switch x := r.IntN(10); {
		case x < 3:
			method, path = "GET", fmt.Sprintf("/api/v2/orders/%d", id())
		case x < 5:
			method, path = "GET", "/api/v2/me"
		case x < 7:
			method, path = "POST", "/api/v2/orders"
		case x < 8:
			method, path = "POST", "/api/v2/auth/refresh"
		default:
			method, path = "GET", "/api/v2/orders?page=1"
		}
	case "auth":
		switch x := r.IntN(10); {
		case x < 6:
			method, path = "POST", "/oauth/token"
		case x < 8:
			method, path = "POST", "/sessions/introspect"
		default:
			method, path = "GET", "/.well-known/jwks.json"
		}
	case "orders":
		switch x := r.IntN(10); {
		case x < 4:
			method, path = "POST", "/orders"
		default:
			method, path = "GET", fmt.Sprintf("/orders/%d", id())
		}
	case "profile":
		if r.IntN(5) == 0 {
			method, path = "PATCH", fmt.Sprintf("/profiles/%d", id())
		} else {
			method, path = "GET", fmt.Sprintf("/profiles/%d", id())
		}
	case "inventory":
		switch x := r.IntN(10); {
		case x < 5:
			method, path = "POST", "/reservations"
		case x < 9:
			method, path = "GET", "/stock/"+pick(r, skus)
		default:
			method, path = "DELETE", fmt.Sprintf("/reservations/%s", hexID(r, 10))
		}
	case "payments":
		if r.IntN(3) == 0 {
			method, path = "POST", fmt.Sprintf("/charges/ch_%s/capture", hexID(r, 12))
		} else {
			method, path = "POST", "/charges"
		}
	case "fulfillment":
		dur := int(v[mLat] * 1000 * (0.1 + 0.7*r.Float64()))
		if r.Float64() < v[mErr] {
			return "error", fmt.Sprintf("handler failed topic=order-events partition=%d offset=%d event=order.created duration_ms=%d", r.IntN(6), 4000000+r.IntN(900000), dur)
		}
		return "info", fmt.Sprintf("processed topic=order-events partition=%d offset=%d event=%s duration_ms=%d", r.IntN(6), 4000000+r.IntN(900000), pick(r, []string{"order.created", "order.paid", "order.cancelled"}), dur)
	default: // notifications
		method, path = "POST", "/send"
	}
	status := 200
	if method == "POST" {
		status = 201
	}
	if svc == "notifications" {
		status = 202
	}
	x := r.Float64()
	switch {
	case x < v[mErr]:
		status = pick(r, []int{500, 502, 503, 504})
	case x < v[mErr]+0.01 && method == "GET" && svc != "auth":
		status = 404
	}
	ms := int(v[mLat] * 1000 * (0.1 + 0.7*r.Float64()))
	level = "info"
	if status >= 500 {
		level = "error"
	}
	return level, fmt.Sprintf("method=%s path=%s status=%d duration_ms=%d", method, path, status, ms)
}

var heldBackground = []periodic{
	{"gateway", "info", 4 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("method=GET path=%s status=404 duration_ms=%d", pick(r, []string{"/.git/config", "/actuator/env", "/phpmyadmin/", "/server-status"}), 1+r.IntN(3))
	}},
	{"gateway", "info", 9 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("upstream health: auth=ok orders=ok profile=ok (p50 %dms)", 3+r.IntN(5))
	}},
	{"auth", "info", 10 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("token cleanup: deleted %d expired refresh tokens", 1500+r.IntN(700))
	}},
	{"auth", "warn", 8 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("login failed for user id=%d: invalid password (attempt %d)", 100000+r.IntN(900000), 1+r.IntN(3))
	}},
	{"orders", "info", time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("outbox relay: published %d events to order-events", 1500+r.IntN(400))
	}},
	{"profile", "info", 15 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("avatar cache warmed (%d entries)", 480+r.IntN(60))
	}},
	{"inventory", "info", 5 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("warehouse stock sync completed: %d skus in %dms", 3100+r.IntN(40), 800+r.IntN(400))
	}},
	{"inventory", "warn", 7 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("reservation %s expired before confirmation", hexID(r, 10))
	}},
	{"payments", "info", 2 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("fx: refreshed EUR/USD quote from https://api.fx-rates.example (%.4f)", 1.07+0.01*r.Float64())
	}},
	{"payments", "warn", 12 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("charge ch_%s declined by issuer: insufficient_funds", hexID(r, 12))
	}},
	{"fulfillment", "info", time.Minute, func(r *rand.Rand) string {
		return "consumer group fulfillment: committed offsets for 6 partitions of order-events"
	}},
	{"fulfillment", "warn", 13 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("slow handler for order.created: %dms (label printer API)", 1500+r.IntN(900))
	}},
	{"notifications", "info", 2 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("mail-relay smtp.mail-relay.example:587: %d messages delivered (queue=0)", 2000+r.IntN(500))
	}},
	{"mysql", "warn", 9 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("[Note] [MY-011953] [InnoDB] Page cleaner took %dms to flush %d pages", 1010+r.IntN(400), 900+r.IntN(400))
	}},
	{"mysql", "info", 6 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("[Note] Aborted connection %d to db: 'shop' user: 'profile' host: '10.0.4.%d' (Got timeout reading communication packets)", 20000+r.IntN(9000), 2+r.IntN(40))
	}},
	{"valkey", "info", 15 * time.Minute, func(r *rand.Rand) string {
		return "Background saving started by pid 77"
	}},
	{"valkey", "info", 15 * time.Minute, func(r *rand.Rand) string {
		return "DB saved on disk"
	}},
	{"kafka", "info", 8 * time.Minute, func(r *rand.Rand) string {
		return fmt.Sprintf("[ProducerStateManager partition=order-events-%d] Wrote producer snapshot at offset %d", r.IntN(6), 4000000+r.IntN(900000))
	}},
	{"kafka", "info", 20 * time.Minute, func(r *rand.Rand) string {
		return "[GroupCoordinator 1]: Member fulfillment-7c9d heartbeat ok, group fulfillment stable"
	}},
}

var heldBenign = []benignTmpl{
	{"deploy", "gateway", "gateway %s: add request id header to upstream calls", "--- a/proxy/headers.go\n+++ b/proxy/headers.go\n@@ -11,3 +11,4 @@\n \treq.Header.Set(\"X-Forwarded-For\", ip)\n+\treq.Header.Set(\"X-Request-Id\", id)\n"},
	{"deploy", "auth", "auth %s: bump golang.org/x/crypto to v0.41.0", "--- a/go.mod\n+++ b/go.mod\n@@ -6 +6 @@\n-\tgolang.org/x/crypto v0.40.0\n+\tgolang.org/x/crypto v0.41.0\n"},
	{"deploy", "orders", "orders %s: include currency in order summary response", "--- a/api/order.go\n+++ b/api/order.go\n@@ -20,3 +20,4 @@\n \tTotal int64 `json:\"total\"`\n+\tCurrency string `json:\"currency\"`\n"},
	{"deploy", "profile", "profile %s: validate phone numbers with libphonenumber", "--- a/validate/phone.go\n+++ b/validate/phone.go\n@@ -1,3 +1,6 @@\n+func ValidPhone(s string) bool {\n+\treturn phonenumbers.IsValid(s)\n+}\n"},
	{"deploy", "inventory", "inventory %s: expose reservation TTL metric", "--- a/metrics.go\n+++ b/metrics.go\n@@ -4,3 +4,6 @@\n+var reservationTTL = prometheus.NewHistogram(prometheus.HistogramOpts{Name: \"reservation_ttl_seconds\"})\n"},
	{"deploy", "notifications", "notifications %s: add Dutch translations", "--- a/i18n/nl.yaml\n+++ b/i18n/nl.yaml\n@@ -0,0 +1,3 @@\n+order_confirmed: \"Bestelling bevestigd\"\n"},
	{"deploy", "fulfillment", "fulfillment %s: log carrier name on label creation", "--- a/labels.go\n+++ b/labels.go\n@@ -30,3 +30,4 @@\n+\tslog.Info(\"label created\", \"carrier\", l.Carrier)\n"},
	{"commit", "payments", "Clarify refund docs", "--- a/docs/refunds.md\n+++ b/docs/refunds.md\n@@ -3 +3 @@\n-Refunds take 5 days.\n+Refunds take 5-10 business days.\n"},
	{"commit", "orders", "Rename OrderSvc to OrderService", "--- a/service.go\n+++ b/service.go\n@@ -1,3 +1,3 @@\n-type OrderSvc struct{}\n+type OrderService struct{}\n"},
	{"flag", "gateway", "gateway: enable http3 for 5% of clients", "http3:\n-  rollout: 0\n+  rollout: 5\n"},
	{"flag", "profile", "profile: enable avatar upload v2 for staff", "avatar_v2:\n-  audience: none\n+  audience: staff\n"},
	{"config", "notifications", "notifications: raise smtp pool size 8 -> 12", "--- a/config.yaml\n+++ b/config.yaml\n@@ -3 +3 @@\n-smtp_pool: 8\n+smtp_pool: 12\n"},
	{"config", "inventory", "inventory: warehouse sync interval 5m -> 4m", "--- a/config.yaml\n+++ b/config.yaml\n@@ -7 +7 @@\n-sync_interval: 5m\n+sync_interval: 4m\n"},
}

func nov(day, hour, minute int) time.Time {
	return time.Date(2026, time.November, day, hour, minute, 0, 0, time.UTC)
}

func heldoutScenarios() []scenario {
	list := []scenario{
		hInventoryIndexRegression(),
		hFulfillmentConcurrency(),
		hKafkaLeaderChurn(),
		hAuthKeyRotation(),
		hFxDNSFailure(),
		hMySQLDiskFull(),
		hNotificationsCronLeak(),
		hOrdersRetryStorm(),
	}
	for i := range list {
		list[i].w = heldout
		if list[i].accessRate == 0 {
			list[i].accessRate = heldAccess
		}
	}
	return list
}

func hInventoryIndexRegression() scenario {
	return scenario{
		name: "inventory-index-regression",
		description: "inventory 3.2.0 rewrites the stock lookup as WHERE LOWER(sku) = ?, which cannot use the sku index; every reservation does a full table scan, mysql CPU climbs and checkout latency rises through orders to the gateway. " +
			"Red herring: a gateway deploy 3m before the alert and a payments 3-D Secure flag 20m before.",
		title:   "Checkout p99 above SLO on gateway",
		symptom: "gateway",
		alert:   nov(3, 11, 25),
		truth:   truth{"inventory", "deploy-inventory-3.2.0", "bad-deploy", []string{"index", "full scan", "full table scan", "LOWER(sku)", "rows_examined"}},
		effects: []effect{
			eff("inventory", mDBQ, opMul, 12, -11*mn, 4*mn),
			eff("inventory", mCPU, opMul, 1.2, -11*mn, 4*mn),
			eff("mysql", mCPU, opMul, 2.3, -11*mn, 4*mn),
			eff("fulfillment", mLag, opAdd, 900, -9*mn, 10*mn),
		},
		emitters: []emitter{
			emit("mysql", "warn", 0.25, -11*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("[Warning] slow query: Query_time: %d.%03d Rows_examined: %d user: inventory; SELECT qty, reserved FROM stock WHERE LOWER(sku) = '%s' FOR UPDATE", ms(r, 0, 2), r.IntN(1000), 1840000+r.IntN(20000), pick(r, skus))
			}),
			emit("inventory", "warn", 0.3, -10*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("slow query reserve_stock on mysql-primary:3306 took %dms", ms(r, 300, 1800))
			}),
			emit("orders", "warn", 0.2, -9*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream inventory slow: POST http://inventory:8080/reservations took %dms", ms(r, 600, 2400))
			}),
		},
		changes: []changeDef{
			{id: "deploy-inventory-3.2.0", kind: "deploy", svc: "inventory", rel: -12 * mn, author: "r.haddad",
				summary: "inventory 3.2.0: case-insensitive SKU lookup",
				diff:    "--- a/store/stock.go\n+++ b/store/stock.go\n@@ -40,7 +40,7 @@ func (s *Store) Reserve(ctx context.Context, tx *sql.Tx, sku string, qty int) error {\n-\trow := tx.QueryRowContext(ctx, `SELECT qty, reserved FROM stock WHERE sku = ? FOR UPDATE`, sku)\n+\trow := tx.QueryRowContext(ctx, `SELECT qty, reserved FROM stock WHERE LOWER(sku) = ? FOR UPDATE`, strings.ToLower(sku))\n"},
			{id: "deploy-gateway-2.15.0", kind: "deploy", svc: "gateway", rel: -3 * mn, author: "t.ivanova",
				summary: "gateway 2.15.0: upgrade proxy to HTTP/2 upstream connections",
				diff:    "--- a/proxy/transport.go\n+++ b/proxy/transport.go\n@@ -8,3 +8,3 @@\n-\tForceAttemptHTTP2: false,\n+\tForceAttemptHTTP2: true,\n"},
			{id: "flag-payments-3ds-v2", kind: "flag", svc: "payments", rel: -20 * mn, author: "k.mensah",
				summary: "payments: enable 3-D Secure v2 for 30% of EU cards",
				diff:    "threeds_v2:\n-  rollout: 0\n+  rollout: 30\n",
				logMsg:  "feature flag \"threeds_v2\" updated: rollout=30%"},
		},
		benign: 4,
	}
}

func hFulfillmentConcurrency() scenario {
	lag := func(rel time.Duration) float64 {
		if rel < -26*mn {
			return 0
		}
		return 19 * (rel + 26*mn).Seconds() // ~19 msg/s backlog growth
	}
	return scenario{
		name: "fulfillment-concurrency-config",
		description: "A config change sets the fulfillment worker's concurrency to 2 instead of 20; the worker falls behind on order-events, consumer lag grows for 26 minutes and confirmation emails stop flowing through notifications. " +
			"Red herring: a notifications deploy 12m before the alert that switches email templates and logs new template warnings.",
		title:   "Order confirmation emails delayed",
		symptom: "notifications",
		alert:   nov(4, 9, 10),
		truth:   truth{"fulfillment", "cfg-fulfillment-workers", "config-change", []string{"concurrency", "worker_concurrency", "workers"}},
		effects: []effect{
			eff("fulfillment", mRate, opMul, 0.3, -26*mn, 1*mn),
			eff("fulfillment", mCPU, opMul, 0.4, -26*mn, 1*mn),
			shaped("fulfillment", mLag, opAdd, 1, lag),
		},
		emitters: []emitter{
			emit("fulfillment", "warn", 0.12, -20*mn, 10*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("consumer lag on order-events partition %d: %d messages behind (kafka-1:9092)", r.IntN(6), ms(r, 1000, 6000))
			}),
			emit("notifications", "warn", 0.1, -12*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return "template order_confirmed: font Inter not embedded, using fallback"
			}),
		},
		changes: []changeDef{
			{id: "cfg-fulfillment-workers", kind: "config", svc: "fulfillment", rel: -26*mn - 10*sec, author: "p.silva",
				summary: "fulfillment: tune consumer settings for new label printer",
				diff:    "--- a/deploy/fulfillment/config.yaml\n+++ b/deploy/fulfillment/config.yaml\n@@ -3,6 +3,6 @@ consumer:\n   topic: order-events\n   group: fulfillment\n-  max_poll_records: 500\n-  worker_concurrency: 20\n+  max_poll_records: 200\n+  worker_concurrency: 2\n",
				logMsg:  "config reloaded: consumer group=fulfillment topic=order-events worker_concurrency=2 max_poll_records=200"},
			{id: "deploy-notifications-2.7.0", kind: "deploy", svc: "notifications", rel: -12 * mn, author: "e.lund",
				summary: "notifications 2.7.0: move email templates to MJML",
				diff:    "--- a/templates/order_confirmed.html\n+++ b/templates/order_confirmed.mjml\n@@ -1,5 +1,9 @@\n-<html><body>\n+<mjml><mj-body>\n"},
		},
		benign: 4,
	}
}

func hKafkaLeaderChurn() scenario {
	lag := func(rel time.Duration) float64 {
		if rel < -9*mn {
			return 0
		}
		return 14 * (rel + 9*mn).Seconds()
	}
	return scenario{
		name: "kafka-leader-churn",
		description: "A kafka broker keeps losing its controller session, so order-events partitions shrink their ISR and re-elect leaders repeatedly; fulfillment fetches fail with NOT_LEADER_OR_FOLLOWER, lag builds and orders sees produce errors. No change is involved. " +
			"Red herring: a fulfillment deploy 8m before the alert, right at the onset.",
		title:   "Consumer lag high on order-events",
		symptom: "fulfillment",
		alert:   nov(5, 22, 40),
		truth:   truth{"kafka", "", "dependency-failure", []string{"leader", "ISR", "broker", "partition"}},
		effects: []effect{
			eff("kafka", mCPU, opMul, 1.7, -9*mn, 2*mn),
			eff("fulfillment", mRate, opMul, 0.45, -9*mn, 2*mn),
			eff("fulfillment", mErr, opAdd, 0.07, -9*mn, 2*mn),
			shaped("fulfillment", mLag, opAdd, 1, lag),
			eff("orders", mErr, opAdd, 0.025, -8*mn, 2*mn),
			eff("orders", mLat, opAdd, 0.4, -8*mn, 2*mn),
		},
		emitters: []emitter{
			emit("kafka", "warn", 0.15, -9*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("[Partition order-events-%d broker=2] Shrinking ISR from 1,2,3 to 1,3", r.IntN(6))
			}),
			emit("kafka", "info", 0.12, -9*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("[Controller id=1] Partition order-events-%d leader changed to broker %d (epoch %d)", r.IntN(6), 1+r.IntN(3), 40+r.IntN(30))
			}),
			emit("kafka", "error", 0.03, -9*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return "[Broker id=2] Controller session expired; re-registering broker kafka-2:9092"
			}),
			emit("fulfillment", "error", 0.5, -9*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("kafka: fetch order-events-%d from kafka-2:9092: NOT_LEADER_OR_FOLLOWER, refreshing metadata", r.IntN(6))
			}),
			emit("orders", "error", 0.2, -8*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("outbox relay: produce to order-events-%d (kafka-2:9092): NOT_ENOUGH_REPLICAS, will retry", r.IntN(6))
			}),
		},
		changes: []changeDef{
			{id: "deploy-fulfillment-0.20.0", kind: "deploy", svc: "fulfillment", rel: -8 * mn, author: "p.silva",
				summary: "fulfillment 0.20.0: batch label printing requests",
				diff:    "--- a/labels.go\n+++ b/labels.go\n@@ -50,6 +50,9 @@\n+\tif len(batch) < 10 {\n+\t\treturn nil\n+\t}\n"},
		},
		mute:   []mute{{svc: "kafka", contains: "group fulfillment stable", start: -9 * mn, end: after}},
		benign: 5,
	}
}

func hAuthKeyRotation() scenario {
	return scenario{
		name: "auth-key-rotation",
		description: "auth rotates its JWT signing key and retires the old one immediately, but gateway caches JWKS for 6h; tokens signed with the new kid fail verification, the gateway answers 401 and clients hammer auth with refreshes. " +
			"Red herring: a gateway deploy 22m before the alert and a profile avatars flag 9m before.",
		title:   "Gateway 401 rate spiking",
		symptom: "gateway",
		alert:   nov(6, 13, 5),
		truth:   truth{"auth", "rotate-auth-signing-key", "config-change", []string{"signing key", "kid", "JWKS", "rotat"}},
		effects: []effect{
			eff("gateway", mErr, opAdd, 0.22, -4*mn, 3*mn),
			eff("gateway", mLat, opMul, 0.85, -4*mn, 3*mn),
			eff("auth", mRate, opMul, 2.4, -4*mn, 3*mn),
			eff("valkey", mCPU, opMul, 1.6, -4*mn, 3*mn),
			eff("orders", mRate, opMul, 0.8, -4*mn, 3*mn),
		},
		emitters: []emitter{
			emit("gateway", "warn", 0.6, -4*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("jwt: verify token: key id \"2026-11\" not found in cached JWKS (fetched from http://auth:8080/.well-known/jwks.json %dh%02dm ago); responding 401 path=/api/v2/orders/%d", 4, 10+r.IntN(5), 100000+r.IntN(900000))
			}),
			emit("auth", "info", 0.4, -4*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("token issued kid=2026-11 client=%s grant=refresh_token", pick(r, []string{"web", "ios", "android"}))
			}),
		},
		oneshots: []oneshot{
			{rel: -4*mn - 10*sec + 2*sec, svc: "auth", level: "info", msg: "signing keys: active kid=2026-11, retired kid=2026-10 (published JWKS has 1 key)"},
		},
		changes: []changeDef{
			{id: "rotate-auth-signing-key", kind: "config", svc: "auth", rel: -4*mn - 10*sec, author: "secops-bot",
				summary: "auth: monthly JWT signing key rotation",
				diff:    "--- a/secrets/auth/jwks.yaml\n+++ b/secrets/auth/jwks.yaml\n@@ -1,6 +1,4 @@\n keys:\n-  - kid: 2026-10\n-    status: active\n+  - kid: 2026-11\n+    status: active\n",
				logMsg:  "config reloaded: signing key set updated (1 key)"},
			{id: "deploy-gateway-2.15.1", kind: "deploy", svc: "gateway", rel: -22 * mn, author: "t.ivanova",
				summary: "gateway 2.15.1: tighten CORS allowed origins",
				diff:    "--- a/config/cors.go\n+++ b/config/cors.go\n@@ -3 +3 @@\n-var allowed = []string{\"*\"}\n+var allowed = []string{\"https://shop.example\"}\n"},
			{id: "flag-profile-avatars", kind: "flag", svc: "profile", rel: -9 * mn, author: "e.lund",
				summary: "profile: serve avatars from new CDN for 50%",
				diff:    "avatar_cdn:\n-  rollout: 0\n+  rollout: 50\n",
				logMsg:  "feature flag \"avatar_cdn\" updated: rollout=50%"},
		},
		benign: 4,
	}
}

func hFxDNSFailure() scenario {
	return scenario{
		name: "fx-dns-failure",
		description: "The external FX rates provider's DNS stops resolving; payments cannot price non-EUR charges and fails after DNS timeouts, so orders and gateway checkout fail. No internal change is involved. " +
			"Red herring: a payments deploy 16m before the alert.",
		title:   "Checkout failures on gateway",
		symptom: "gateway",
		alert:   nov(7, 7, 50),
		truth:   truth{"fx-api", "", "dependency-failure", []string{"DNS", "no such host", "lookup"}},
		effects: []effect{
			eff("payments", mErr, opAdd, 0.4, -3*mn, 30*sec),
			eff("payments", mLat, opAdd, 4.5, -3*mn, 30*sec),
			eff("payments", mCPU, opMul, 0.8, -3*mn, 30*sec),
		},
		emitters: []emitter{
			emit("payments", "error", 0.35, -3*mn, 30*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("fx quote for %s: Get \"https://api.fx-rates.example/v1/quote?pair=%s\": dial tcp: lookup api.fx-rates.example on 10.96.0.10:53: no such host", pick(r, []string{"GBP", "USD", "SEK"}), pick(r, []string{"EURGBP", "EURUSD", "EURSEK"}))
			}),
			emit("orders", "error", 0.25, -3*mn, 30*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("create order %d: payments POST http://payments:8080/charges: 502 Bad Gateway after %dms", 100000+r.IntN(900000), ms(r, 4600, 5200))
			}),
		},
		mute: []mute{{svc: "payments", contains: "fx: refreshed", start: -3 * mn, end: after}},
		oneshots: []oneshot{
			{rel: -2 * mn, svc: "payments", level: "warn", msg: "fx: using cached EUR/USD quote, age 3m10s exceeds max_age 2m; non-EUR charges disabled"},
		},
		changes: []changeDef{
			{id: "deploy-payments-5.1.0", kind: "deploy", svc: "payments", rel: -16 * mn, author: "k.mensah",
				summary: "payments 5.1.0: retry idempotent captures once on 409",
				diff:    "--- a/capture.go\n+++ b/capture.go\n@@ -22,3 +22,7 @@\n+\tif resp.StatusCode == http.StatusConflict {\n+\t\treturn c.capture(ctx, id)\n+\t}\n"},
		},
		benign: 5,
	}
}

func hMySQLDiskFull() scenario {
	fill := func(rel time.Duration) float64 {
		start, full := -100*mn, -2*mn
		if rel < start {
			return 0
		}
		return math.Min(1, float64(rel-start)/float64(full-start))
	}
	return scenario{
		name: "mysql-disk-full",
		description: "Binary logs on the mysql primary are not purged and the data volume fills over 100 minutes; once it is full, writes block or fail with errno 28, inventory reservations and profile updates error and orders fail at the gateway. No change is involved. " +
			"Red herring: an inventory deploy 9m before the alert and an orders config change 30m before.",
		title:   "Gateway 5xx on order placement",
		symptom: "gateway",
		alert:   nov(8, 18, 15),
		truth:   truth{"mysql", "", "resource-exhaustion", []string{"disk", "space", "errno 28", "full"}},
		effects: []effect{
			shaped("mysql", mDisk, opAdd, 0.39, fill),
			eff("inventory", mErr, opAdd, 0.32, -2*mn, 1*mn),
			eff("inventory", mLat, opAdd, 1.5, -2*mn, 1*mn),
			eff("inventory", mPool, opAdd, 0.6, -2*mn, 1*mn),
			eff("profile", mErr, opAdd, 0.06, -2*mn, 1*mn),
			eff("mysql", mCPU, opMul, 0.7, -2*mn, 1*mn),
		},
		emitters: []emitter{
			emit("mysql", "error", 0.1, -2*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("[ERROR] [MY-012639] [InnoDB] Write to file ./shop/stock_movements.ibd failed at offset %d, 16384 bytes should have been written, only 0 were written. OS errno 28 - No space left on device", 1<<30+r.IntN(1<<20)*16384)
			}),
			emit("mysql", "warn", 0.03, -2*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return "[Warning] Disk is full writing './binlog.000412' (OS errno 28 - No space left on device). Waiting for someone to free space... Retry in 60 secs."
			}),
			emit("inventory", "error", 0.6, -2*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("reserve %s: Error 1114 (HY000): The table 'stock_movements' is full (mysql-primary:3306)", pick(r, skus))
			}),
			emit("orders", "error", 0.3, -2*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("create order %d: inventory POST http://inventory:8080/reservations: 500 Internal Server Error", 100000+r.IntN(900000))
			}),
			emit("profile", "error", 0.05, -2*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("update profile %d: Error 1114 (HY000): The table 'profile_audit' is full", 100000+r.IntN(900000))
			}),
		},
		changes: []changeDef{
			{id: "deploy-inventory-3.1.7", kind: "deploy", svc: "inventory", rel: -9 * mn, author: "r.haddad",
				summary: "inventory 3.1.7: write stock movement reason codes",
				diff:    "--- a/store/movements.go\n+++ b/store/movements.go\n@@ -12,3 +12,4 @@\n \tSKU string\n+\tReason string\n"},
			{id: "cfg-orders-page-size", kind: "config", svc: "orders", rel: -30 * mn, author: "m.brandt",
				summary: "orders: default page size 20 -> 50",
				diff:    "--- a/config.yaml\n+++ b/config.yaml\n@@ -2 +2 @@\n-page_size: 20\n+page_size: 50\n"},
		},
		benign: 4,
	}
}

func hNotificationsCronLeak() scenario {
	start := -135 * mn
	restarts := []time.Duration{-1 * mn, 70 * mn}
	frac := leakFrac(start, restarts)
	sq := func(rel time.Duration) float64 { f := frac(rel); return f * f }
	inRestart := func(rel time.Duration) bool { return rel >= -1*mn && rel < -1*mn+45*sec }
	return scenario{
		name: "notifications-cron-leak",
		description: "A weekly digest cron job inside notifications loads every subscriber into memory; memory grows for over two hours with no deploy until the pod is OOM-killed, then the job resumes and memory climbs again. Fulfillment, which calls notifications, sees slow calls and then connection errors. " +
			"Red herring: a notifications template config change 30m before the alert and a fulfillment deploy 15m before.",
		title:      "Fulfillment handler errors rising",
		symptom:    "fulfillment",
		alert:      nov(9, 14, 20),
		lookback:   3 * time.Hour,
		accessRate: 0.08,
		truth:      truth{"notifications", "", "resource-exhaustion", []string{"memory", "digest", "cron", "OOM"}},
		effects: []effect{
			shaped("notifications", mMem, opAdd, 790e6, frac),
			shaped("notifications", mLat, opAdd, 0.6, sq),
			shaped("notifications", mCPU, opAdd, 0.4, sq),
			eff("notifications", mErr, opAdd, 0.5, -1*mn, 10*sec).until(-1*mn + 40*sec),
			eff("notifications", mRate, opMul, 0.3, -1*mn, 10*sec).until(-1*mn + 40*sec),
		},
		gaps: []gap{{svc: "notifications", start: -1 * mn, end: -1*mn + 30*sec}},
		emitters: []emitter{
			emit("notifications", "info", 0.02, start, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("weekly_digest: built digests for %d subscribers (batch %d)", 5000, r.IntN(400))
			}),
			{window: window{shape: sq}, svc: "notifications", level: "warn", rate: 0.15, msg: func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("gc: pause %dms, heap %dMB", ms(r, 60, 240), ms(r, 600, 990))
			}},
			{window: window{shape: func(rel time.Duration) float64 {
				if inRestart(rel) {
					return 1
				}
				return 0
			}}, svc: "fulfillment", level: "error", rate: 1.2, msg: func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("send order_shipped notification: POST http://notifications:8080/send: dial tcp 10.0.5.9:8080: connect: connection refused (order %d)", 100000+r.IntN(900000))
			}},
			{window: window{shape: sq}, svc: "fulfillment", level: "warn", rate: 0.12, msg: func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream notifications slow: POST http://notifications:8080/send took %dms", ms(r, 700, 2200))
			}},
		},
		oneshots: []oneshot{
			{rel: start, svc: "notifications", level: "info", msg: "cron: starting job weekly_digest (schedule \"5 12 * * MON\")"},
			{rel: -1*mn - 2*sec, svc: "notifications", level: "error", msg: "fatal error: runtime: out of memory"},
			{rel: -1*mn + 1*sec, svc: "notifications", level: "warn", msg: "container notifications restarted: OOMKilled (exit code 137), restart count 1", labels: map[string]string{"source": "kubelet"}},
			{rel: -1*mn + 8*sec, svc: "notifications", level: "info", msg: "starting notifications version 2.6.4 (commit 51ac0e2)"},
			{rel: -1*mn + 9*sec, svc: "notifications", level: "info", msg: "listening on :8080"},
			{rel: -1*mn + 12*sec, svc: "notifications", level: "info", msg: "cron: resuming job weekly_digest from checkpoint 0 (previous run did not finish)"},
		},
		changes: []changeDef{
			{id: "cfg-notifications-templates", kind: "config", svc: "notifications", rel: -30 * mn, author: "e.lund",
				summary: "notifications: shipping email subject line A/B test",
				diff:    "--- a/config/templates.yaml\n+++ b/config/templates.yaml\n@@ -4 +4,2 @@\n-order_shipped_subject: \"Your order is on its way\"\n+order_shipped_subject_a: \"Your order is on its way\"\n+order_shipped_subject_b: \"Good news: it shipped!\"\n"},
			{id: "deploy-fulfillment-0.19.4", kind: "deploy", svc: "fulfillment", rel: -15 * mn, author: "p.silva",
				summary: "fulfillment 0.19.4: retry label printer once on timeout",
				diff:    "--- a/labels.go\n+++ b/labels.go\n@@ -60,3 +60,6 @@\n+\tif errors.Is(err, context.DeadlineExceeded) {\n+\t\treturn p.print(ctx, l)\n+\t}\n"},
		},
		benign: 3,
	}
}

func hOrdersRetryStorm() scenario {
	return scenario{
		name: "orders-retry-storm",
		description: "Orders' payments client was reconfigured 50m earlier to retry 6 times with no backoff. A brief payments blip during a rolling deploy triggers a retry storm: payments traffic multiplies, payments saturates and keeps failing, and gateway latency and 503s rise. The symptomatic service is the gateway and the visible failure is payments, but the cause is the orders retry policy. " +
			"Red herring: the payments deploy 7m before the alert that triggered the blip.",
		title:   "Gateway latency and 503s",
		symptom: "gateway",
		alert:   nov(10, 20, 30),
		truth:   truth{"orders", "cfg-orders-payments-retries", "config-change", []string{"retry", "retries", "backoff", "amplif"}},
		effects: []effect{
			eff("payments", mErr, opAdd, 0.3, -7*mn, 10*sec).until(-6 * mn),
			eff("payments", mRate, opMul, 4.5, -6*mn, 2*mn),
			eff("payments", mErr, opAdd, 0.08, -6*mn, 2*mn),
			eff("orders", mCPU, opMul, 1.4, -6*mn, 2*mn),
			eff("payments", mCPU, opMul, 1.35, -6*mn, 2*mn),
		},
		emitters: []emitter{
			emit("orders", "warn", 0.9, -6*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("payments POST http://payments:8080/charges failed (attempt %d/6): 503 Service Unavailable, retrying immediately", 2+r.IntN(5))
			}),
			emit("payments", "warn", 0.3, -5*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("load shedding: %d in-flight requests > limit 200, rejecting with 503", ms(r, 210, 900))
			}),
			emit("payments", "info", 0.2, -6*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("duplicate charge request for idempotency key %s (seen %d times)", hexID(r, 16), ms(r, 2, 6))
			}),
		},
		changes: []changeDef{
			{id: "cfg-orders-payments-retries", kind: "config", svc: "orders", rel: -50 * mn, author: "m.brandt",
				summary: "orders: make payments client more resilient to transient errors",
				diff:    "--- a/config/orders.yaml\n+++ b/config/orders.yaml\n@@ -18,6 +18,6 @@ clients:\n   payments:\n     url: http://payments:8080\n     timeout: 3s\n-    retries: 1\n-    backoff: 200ms\n+    retries: 6\n+    backoff: 0s\n"},
			{id: "deploy-payments-5.1.3", kind: "deploy", svc: "payments", rel: -7 * mn, author: "k.mensah",
				summary: "payments 5.1.3: rolling restart to pick up new TLS bundle",
				diff:    "--- a/deploy/payments.yaml\n+++ b/deploy/payments.yaml\n@@ -40 +40 @@\n-        caBundleVersion: \"41\"\n+        caBundleVersion: \"42\"\n"},
		},
		benign: 4,
	}
}
