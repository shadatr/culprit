package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

const (
	sec = time.Second
	mn  = time.Minute
)

func at(day, hour, minute int) time.Time {
	return time.Date(2026, time.October, day, hour, minute, 0, 0, time.UTC)
}

func ms(r *rand.Rand, lo, hi int) int { return lo + r.IntN(hi-lo+1) }

func ip(r *rand.Rand, subnet int) string {
	return fmt.Sprintf("10.0.%d.%d:%d", subnet, 2+r.IntN(60), 30000+r.IntN(30000))
}

// leakFrac models a memory leak that starts at start and resets at each
// restart: it grows linearly from 0 to 1 between consecutive restarts.
func leakFrac(start time.Duration, restarts []time.Duration) func(time.Duration) float64 {
	return func(rel time.Duration) float64 {
		if rel < start {
			return 0
		}
		prev := start
		for _, r := range restarts {
			if rel < r {
				return float64(rel-prev) / float64(r-prev)
			}
			prev = r
		}
		last := restarts[len(restarts)-1]
		span := last - restarts[len(restarts)-2]
		return math.Min(1, float64(rel-last)/float64(span))
	}
}

func scenarios() []scenario {
	return []scenario{
		catalogBadDeploy(),
		catalogPoolExhaustion(),
		redisOutage(),
		frontendBadConfig(),
		cartMemoryLeak(),
		trafficSpike(),
		searchCertExpiry(),
		searchFeatureFlag(),
		cartCacheTTLRedHerring(),
		slowPostgres(),
	}
}

func catalogBadDeploy() scenario {
	return scenario{
		name: "catalog-bad-deploy",
		description: "catalog 1.8.2 adds an image gallery that loads images once per product (N+1 queries); catalog and frontend p99 rise. " +
			"Red herring: a cart deploy 22m earlier and a frontend banner flag 2m before the alert.",
		title:   "High p99 latency on frontend",
		symptom: "frontend",
		alert:   at(20, 14, 5),
		truth:   truth{"catalog", "deploy-catalog-1.8.2", "bad-deploy", []string{"N+1", "product_images", "per product"}},
		effects: []effect{
			eff("catalog", mLat, opMul, 4.5, -6*mn, 3*mn),
			eff("catalog", mCPU, opMul, 1.5, -6*mn, 3*mn),
			eff("catalog", mDBQ, opMul, 1.3, -6*mn, 3*mn),
			eff("catalog", mPool, opAdd, 0.35, -6*mn, 3*mn),
			eff("postgres", mCPU, opMul, 2.2, -6*mn, 3*mn),
		},
		emitters: []emitter{
			emit("catalog", "warn", 0.5, -6*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("slow request GET /api/products?category=%s took %dms (db_queries=%d)", pick(r, categories), ms(r, 900, 2400), ms(r, 36, 60))
			}),
			emit("postgres", "info", 0.4, -6*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("duration: %d.%03d ms  statement: SELECT url, alt_text, position FROM product_images WHERE product_id = $1 ORDER BY position", ms(r, 6, 19), r.IntN(1000))
			}),
			emit("frontend", "warn", 0.25, -5*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream catalog slow: GET http://catalog:8080/api/products?category=%s took %dms", pick(r, categories), ms(r, 1100, 2900))
			}),
			emit("search", "warn", 0.15, -5*mn, 3*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("hydrate results: catalog GET http://catalog:8080/api/products/%d took %dms", products(r), ms(r, 700, 2100))
			}),
		},
		changes: []changeDef{
			{id: "deploy-catalog-1.8.2", kind: "deploy", svc: "catalog", rel: -7 * mn, author: "maria.k",
				summary: "catalog 1.8.2: show image gallery on product list pages",
				diff:    "--- a/internal/store/products.go\n+++ b/internal/store/products.go\n@@ -61,6 +61,13 @@ func (s *Store) ListByCategory(ctx context.Context, cat string) ([]Product, error) {\n \tproducts, err := s.queryProducts(ctx, cat)\n \tif err != nil {\n \t\treturn nil, err\n \t}\n+\tfor i := range products {\n+\t\timgs, err := s.images.ForProduct(ctx, products[i].ID) // SELECT ... FROM product_images WHERE product_id = $1\n+\t\tif err != nil {\n+\t\t\treturn nil, err\n+\t\t}\n+\t\tproducts[i].Gallery = imgs\n+\t}\n \treturn products, nil\n"},
			{id: "deploy-cart-2.3.1", kind: "deploy", svc: "cart", rel: -22 * mn, author: "j.okafor",
				summary: "cart 2.3.1: upgrade redis client to go-redis v9.6",
				diff:    "--- a/go.mod\n+++ b/go.mod\n@@ -7,7 +7,7 @@\n-\tgithub.com/redis/go-redis/v9 v9.5.1\n+\tgithub.com/redis/go-redis/v9 v9.6.0\n"},
			{id: "flag-frontend-autumn-banner", kind: "flag", svc: "frontend", rel: -2 * mn, author: "lin.chen",
				summary: "frontend: enable autumn-sale homepage banner for 50% of sessions",
				diff:    "autumn_banner:\n-  rollout: 0\n+  rollout: 50\n",
				logMsg:  "feature flag \"autumn_banner\" updated: rollout=50%"},
		},
		benign: 4,
	}
}

func catalogPoolExhaustion() scenario {
	return scenario{
		name: "catalog-pool-exhaustion",
		description: "catalog 1.9.0 calls the pricing API inside a DB transaction, holding pool connections for seconds; the pool saturates and requests fail with 'timeout acquiring connection'. " +
			"Red herring: a postgres minor upgrade 45m before and a search flag 10m before.",
		title:   "Frontend 5xx error rate above 5%",
		symptom: "frontend",
		alert:   at(21, 10, 20),
		truth:   truth{"catalog", "deploy-catalog-1.9.0", "resource-exhaustion", []string{"pool", "connection", "transaction"}},
		effects: []effect{
			eff("catalog", mPool, opAdd, 0.85, -21*mn, 17*mn),
			eff("catalog", mLat, opMul, 7, -7*mn, 4*mn),
			eff("catalog", mErr, opAdd, 0.2, -4*mn, 3*mn),
			eff("catalog", mCPU, opMul, 0.85, -5*mn, 3*mn),
			eff("postgres", mCPU, opMul, 0.8, -5*mn, 3*mn),
		},
		emitters: []emitter{
			emit("catalog", "warn", 0.2, -14*mn, 8*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("db pool: %d requests waiting for a connection (in_use=50 max=50)", ms(r, 3, 40))
			}),
			emit("catalog", "error", 1.1, -4*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("GET /api/products/%d: timeout acquiring connection from pool after 5s (in_use=50 max=50 waiting=%d)", products(r), ms(r, 20, 140))
			}),
			emit("catalog", "warn", 0.15, -20*mn, 5*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("reserve stock: transaction open for %dms (pricing call %dms)", ms(r, 1800, 4800), ms(r, 1500, 4500))
			}),
			emit("frontend", "error", 0.5, -4*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("GET /products/%d: upstream catalog: GET http://catalog:8080/api/products/%d: 503 Service Unavailable", products(r), products(r))
			}),
			emit("search", "error", 0.3, -4*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("hydrate results for q=%s: catalog GET http://catalog:8080/api/products/%d: 503 Service Unavailable", pick(r, terms), products(r))
			}),
		},
		changes: []changeDef{
			{id: "deploy-catalog-1.9.0", kind: "deploy", svc: "catalog", rel: -24 * mn, author: "dev.patel",
				summary: "catalog 1.9.0: reserve stock atomically with price check",
				diff:    "--- a/internal/stock/reserve.go\n+++ b/internal/stock/reserve.go\n@@ -22,9 +22,18 @@ func (s *Service) Reserve(ctx context.Context, sku string, qty int) error {\n-\treturn s.store.Decrement(ctx, sku, qty)\n+\ttx, err := s.db.BeginTx(ctx, nil)\n+\tif err != nil {\n+\t\treturn err\n+\t}\n+\tdefer tx.Rollback()\n+\tprice, err := s.pricing.Quote(ctx, sku) // HTTP call to pricing API while tx holds a connection\n+\tif err != nil {\n+\t\treturn err\n+\t}\n+\tif err := s.store.DecrementTx(ctx, tx, sku, qty, price); err != nil {\n+\t\treturn err\n+\t}\n+\treturn tx.Commit()\n"},
			{id: "infra-postgres-16.4", kind: "infra", svc: "postgres", rel: -45 * mn, author: "a.novak",
				summary: "postgres: minor upgrade 16.3 -> 16.4 (rolling restart)",
				diff:    "--- a/infra/postgres.tf\n+++ b/infra/postgres.tf\n@@ -3 +3 @@\n-  engine_version = \"16.3\"\n+  engine_version = \"16.4\"\n",
				logMsg:  "database system is ready to accept connections (PostgreSQL 16.4)"},
			{id: "flag-search-typo-tolerance", kind: "flag", svc: "search", rel: -10 * mn, author: "sam.r",
				summary: "search: enable typo_tolerance for 50% of queries",
				diff:    "typo_tolerance:\n-  rollout: 10\n+  rollout: 50\n",
				logMsg:  "feature flag \"typo_tolerance\" updated: rollout=50%"},
		},
		oneshots: []oneshot{
			{rel: -45*mn + 2*sec, svc: "postgres", level: "info", msg: "received fast shutdown request"},
			{rel: -45*mn + 3*sec, svc: "postgres", level: "info", msg: "database system is shut down"},
		},
		gaps:   []gap{{svc: "postgres", start: -45 * mn, end: -45*mn + 30*sec}},
		benign: 4,
	}
}

func redisOutage() scenario {
	return scenario{
		name: "redis-outage",
		description: "The redis primary host fails with no preceding change; cart cannot reach the cache (connection refused), cache hit ratio collapses and checkout errors rise on frontend. Redis comes back about 17m after the alert. " +
			"Red herring: a frontend deploy 9m before the alert on the symptomatic service.",
		title:   "Checkout errors on frontend",
		symptom: "frontend",
		alert:   at(21, 19, 42),
		truth:   truth{"redis", "", "dependency-failure", []string{"redis", "connection refused", "cache"}},
		effects: []effect{
			eff("cart", mCache, opSet, 0, -2*mn, 20*sec).until(16 * mn),
			eff("cart", mErr, opAdd, 0.32, -2*mn, 30*sec).until(16 * mn),
			eff("cart", mLat, opAdd, 1.2, -2*mn, 30*sec).until(16 * mn),
			// After the restart redis reloads its RDB and memory climbs back.
			shaped("redis", mMem, opSet, 60e6, func(rel time.Duration) float64 {
				if rel < 16*mn {
					return 0
				}
				return math.Max(0, 1-float64(rel-16*mn)/float64(14*mn))
			}),
		},
		gaps: []gap{{svc: "redis", start: -2 * mn, end: 16 * mn}},
		emitters: []emitter{
			emit("cart", "error", 1.2, -2*mn, 20*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("redis GET cart:%s: dial tcp 10.0.3.7:6379: connect: connection refused", hexID(r, 8))
			}).until(16 * mn),
			emit("cart", "error", 0.6, -2*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("redis SET cart:%s: redis: connection pool timeout", hexID(r, 8))
			}).until(16 * mn),
			emit("cart", "warn", 0.3, -2*mn, 20*sec, func(r *rand.Rand, _ time.Time) string {
				return "cache unavailable, rebuilding cart from session store"
			}).until(16 * mn),
			emit("frontend", "error", 0.5, -2*mn, 30*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("POST /checkout: upstream cart: POST http://cart:8080/api/cart/%s/checkout: 500 Internal Server Error", hexID(r, 8))
			}).until(16 * mn),
		},
		oneshots: []oneshot{
			{rel: -2*mn - 40*sec, svc: "redis", level: "info", msg: "Background saving started by pid 2211"},
			{rel: 16*mn + 4*sec, svc: "redis", level: "info", msg: "oO0OoO0OoO0Oo Redis is starting oO0OoO0OoO0Oo"},
			{rel: 16*mn + 5*sec, svc: "redis", level: "info", msg: "Server initialized"},
			{rel: 16*mn + 9*sec, svc: "redis", level: "info", msg: "Done loading RDB, keys loaded: 182311, keys expired: 1204"},
			{rel: 16*mn + 9*sec, svc: "redis", level: "info", msg: "Ready to accept connections tcp"},
		},
		changes: []changeDef{
			{id: "deploy-frontend-4.12.0", kind: "deploy", svc: "frontend", rel: -9 * mn, author: "lin.chen",
				summary: "frontend 4.12.0: restyle checkout button and order summary",
				diff:    "--- a/web/styles/checkout.css\n+++ b/web/styles/checkout.css\n@@ -10,4 +10,6 @@\n-.checkout-btn { background: #2a6; }\n+.checkout-btn { background: #1f8a4c; border-radius: 6px; }\n"},
			{id: "commit-4b2e9a1", kind: "commit", svc: "cart", rel: -52 * mn, author: "j.okafor",
				summary: "Log cart size on checkout",
				diff:    "--- a/internal/checkout/handler.go\n+++ b/internal/checkout/handler.go\n@@ -30,6 +30,7 @@\n+\tslog.Debug(\"checkout\", \"items\", len(c.Items))\n"},
		},
		benign: 4,
	}
}

func frontendBadConfig() scenario {
	return scenario{
		name: "frontend-bad-config",
		description: "A frontend config change lowers the catalog client timeout from 2s to 200ms; product page requests to catalog time out and frontend errors rise while catalog itself is healthy. " +
			"Red herring: a catalog deploy 11m before that adds new slow-query log lines.",
		title:   "Frontend error rate elevated on product pages",
		symptom: "frontend",
		alert:   at(22, 8, 55),
		truth:   truth{"frontend", "cfg-frontend-catalog-timeout", "config-change", []string{"timeout", "200ms", "config"}},
		effects: []effect{
			eff("frontend", mErr, opAdd, 0.11, -5*mn, 40*sec),
			eff("frontend", mLat, opMul, 0.85, -5*mn, 40*sec),
			eff("catalog", mRate, opMul, 1.22, -5*mn, 40*sec),
			eff("catalog", mCPU, opMul, 1.1, -5*mn, 40*sec),
		},
		emitters: []emitter{
			emit("frontend", "error", 0.9, -5*mn, 40*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("GET /products/%d: upstream catalog: Get \"http://catalog:8080/api/products/%d\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", products(r), products(r))
			}),
			emit("frontend", "warn", 0.45, -5*mn, 40*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("retrying catalog request GET /api/products/%d (attempt 2/2)", products(r))
			}),
			emit("catalog", "info", 0.4, -5*mn, 40*sec, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("client closed connection before response: GET /api/products/%d (elapsed %dms)", products(r), ms(r, 200, 260))
			}),
			emit("catalog", "info", 0.12, -11*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("query plan recorded: products_by_category plan_hash=%s rows=%d", hexID(r, 8), ms(r, 20, 400))
			}),
		},
		changes: []changeDef{
			{id: "cfg-frontend-catalog-timeout", kind: "config", svc: "frontend", rel: -5*mn - 20*sec, author: "sam.r",
				summary: "frontend: tune upstream client timeouts",
				diff:    "--- a/config/frontend.yaml\n+++ b/config/frontend.yaml\n@@ -14,9 +14,9 @@ upstreams:\n   catalog:\n     url: http://catalog:8080\n-    timeout: 2s\n+    timeout: 200ms\n     retries: 1\n   cart:\n     url: http://cart:8080\n-    timeout: 1s\n+    timeout: 800ms\n",
				logMsg:  "config reloaded from /etc/frontend/config.yaml (version 87)"},
			{id: "deploy-catalog-1.8.4", kind: "deploy", svc: "catalog", rel: -11 * mn, author: "maria.k",
				summary: "catalog 1.8.4: record query plan hashes for slow-query analysis",
				diff:    "--- a/internal/store/trace.go\n+++ b/internal/store/trace.go\n@@ -12,3 +12,9 @@\n+func (s *Store) recordPlan(ctx context.Context, name string) {\n+\th := s.planHash(ctx, name)\n+\tslog.Info(\"query plan recorded\", \"query\", name, \"plan_hash\", h)\n+}\n"},
		},
		benign: 5,
	}
}

func cartMemoryLeak() scenario {
	start := -138 * mn
	restarts := []time.Duration{-95 * mn, -58 * mn, -27 * mn, -4 * mn, 18 * mn}
	frac := leakFrac(start, restarts)
	sq := func(rel time.Duration) float64 { f := frac(rel); return f * f }
	var effects []effect
	effects = append(effects,
		shaped("cart", mMem, opAdd, 760e6, frac),
		shaped("cart", mLat, opAdd, 0.35, sq),
		shaped("cart", mCPU, opAdd, 0.35, sq),
	)
	var oneshots []oneshot
	var gaps []gap
	for i, rs := range restarts {
		if rs > after {
			break
		}
		effects = append(effects,
			eff("cart", mErr, opAdd, 0.45, rs, 10*sec).until(rs+50*sec),
			eff("cart", mRate, opMul, 0.6, rs, 10*sec).until(rs+50*sec),
		)
		gaps = append(gaps, gap{svc: "cart", start: rs, end: rs + 30*sec})
		oneshots = append(oneshots,
			oneshot{rel: rs - 3*sec, svc: "cart", level: "error", msg: fmt.Sprintf("runtime: out of memory: cannot allocate 8388608-byte block (%d in use)", 1009000000+i*1371)},
			oneshot{rel: rs - 3*sec, svc: "cart", level: "error", msg: "fatal error: out of memory"},
			oneshot{rel: rs + 1*sec, svc: "cart", level: "warn", msg: fmt.Sprintf("container cart restarted: OOMKilled (exit code 137), restart count %d", i+1), labels: map[string]string{"source": "kubelet"}},
			oneshot{rel: rs + 9*sec, svc: "cart", level: "info", msg: "starting cart version 2.4.0 (commit 9e1d2f7)"},
			oneshot{rel: rs + 10*sec, svc: "cart", level: "info", msg: "listening on :8080"},
		)
	}
	return scenario{
		name: "cart-memory-leak",
		description: "cart 2.4.0 memoizes promo eligibility per session in a map that is never evicted; from the deploy 2h19m before the alert memory ramps and cart is OOM-killed repeatedly, causing bursts of checkout failures. " +
			"Red herring: a frontend deploy 6m before the alert and a catalog recs flag 20m before.",
		title:      "Intermittent checkout failures on frontend",
		symptom:    "frontend",
		alert:      at(18, 16, 30),
		truth:      truth{"cart", "deploy-cart-2.4.0", "resource-exhaustion", []string{"memory", "leak", "OOM"}},
		effects:    effects,
		gaps:       gaps,
		lookback:   3 * time.Hour,
		accessRate: 0.22,
		emitters: []emitter{
			{window: window{shape: sq}, svc: "cart", level: "warn", rate: 0.25, msg: func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("gc: pause %dms, heap %dMB", ms(r, 80, 260), ms(r, 700, 980))
			}},
			{window: window{shape: func(rel time.Duration) float64 {
				for _, rs := range restarts {
					if rel >= rs && rel < rs+50*sec {
						return 1
					}
				}
				return 0
			}}, svc: "frontend", level: "error", rate: 1.5, msg: func(r *rand.Rand, _ time.Time) string {
				if r.IntN(3) == 0 {
					return fmt.Sprintf("POST /checkout: upstream cart: dial tcp 10.0.2.7:8080: connect: connection refused (req=%s)", hexID(r, 8))
				}
				return fmt.Sprintf("POST /checkout: upstream cart: read tcp %s->10.0.2.7:8080: read: connection reset by peer", ip(r, 1))
			}},
		},
		oneshots: oneshots,
		changes: []changeDef{
			{id: "deploy-cart-2.4.0", kind: "deploy", svc: "cart", rel: -139 * mn, author: "dev.patel",
				summary: "cart 2.4.0: memoize promo eligibility per session",
				diff:    "--- a/internal/promo/eligibility.go\n+++ b/internal/promo/eligibility.go\n@@ -9,8 +9,17 @@\n+var eligibilityCache = map[string][]Promo{} // keyed by session id\n+var cacheMu sync.Mutex\n+\n func Eligible(ctx context.Context, sess Session) []Promo {\n+\tcacheMu.Lock()\n+\tdefer cacheMu.Unlock()\n+\tif p, ok := eligibilityCache[sess.ID]; ok {\n+\t\treturn p\n+\t}\n \tp := computeEligible(ctx, sess)\n+\teligibilityCache[sess.ID] = p\n \treturn p\n",
				logMsg:  "starting cart version 2.4.0 (commit 9e1d2f7)"},
			{id: "deploy-frontend-4.11.2", kind: "deploy", svc: "frontend", rel: -6 * mn, author: "lin.chen",
				summary: "frontend 4.11.2: lazy-load recommendation carousel",
				diff:    "--- a/web/components/recs.tsx\n+++ b/web/components/recs.tsx\n@@ -3 +3 @@\n-import Carousel from './carousel'\n+const Carousel = lazy(() => import('./carousel'))\n"},
			{id: "flag-catalog-recs-v2", kind: "flag", svc: "catalog", rel: -20 * mn, author: "maria.k",
				summary: "catalog: enable recs-v2 for 20% of traffic",
				diff:    "recs_v2:\n-  mode: shadow\n+  mode: on\n+  rollout: 20\n",
				logMsg:  "feature flag \"recs_v2\" updated: mode=on rollout=20%"},
		},
		benign: 3,
	}
}

func trafficSpike() scenario {
	return scenario{
		name: "traffic-spike",
		description: "A flash-sale newsletter sends 2.4x traffic to frontend with no change involved; frontend, search and catalog saturate CPU and latency and errors rise everywhere. " +
			"Red herring: a search deploy 13m before and a catalog cache TTL config change 4m before the alert.",
		title:   "Frontend p99 latency and errors elevated",
		symptom: "frontend",
		alert:   at(19, 12, 0),
		truth:   truth{"frontend", "", "traffic-spike", []string{"traffic", "spike", "load", "campaign"}},
		effects: []effect{
			eff("frontend", mRate, opMul, 2.4, -9*mn, 5*mn).until(22 * mn),
			eff("postgres", mCPU, opMul, 1.3, -9*mn, 5*mn).until(22 * mn),
		},
		emitters: []emitter{
			emit("frontend", "info", 0.8, -9*mn, 5*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("GET /landing/flash-sale?utm_source=newsletter&utm_campaign=autumn-flash 200 %dms", ms(r, 80, 900))
			}).until(22 * mn),
			emit("catalog", "warn", 0.3, -4*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("request queue depth %d exceeds 200, shedding load", ms(r, 210, 480))
			}).until(20 * mn),
			emit("frontend", "warn", 0.1, -6*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("rate limiter: %d%% of global budget used", ms(r, 78, 97))
			}).until(22 * mn),
			emit("search", "warn", 0.2, -5*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("worker pool saturated: %d queries queued", ms(r, 40, 160))
			}).until(20 * mn),
			emit("frontend", "error", 0.35, -4*mn, 2*mn, func(r *rand.Rand, _ time.Time) string {
				if r.IntN(2) == 0 {
					return fmt.Sprintf("GET /search?q=%s: upstream search: Get \"http://search:8080/v1/query?q=%s\": context deadline exceeded", pick(r, terms), pick(r, terms))
				}
				return "GET /: render timeout after 2s (cpu throttled)"
			}).until(20 * mn),
		},
		changes: []changeDef{
			{id: "deploy-search-3.2.1", kind: "deploy", svc: "search", rel: -13 * mn, author: "sam.r",
				summary: "search 3.2.1: boost in-stock items in ranking",
				diff:    "--- a/internal/rank/score.go\n+++ b/internal/rank/score.go\n@@ -40,3 +40,6 @@\n+\tif doc.InStock {\n+\t\tscore *= 1.1\n+\t}\n"},
			{id: "cfg-catalog-cache-ttl", kind: "config", svc: "catalog", rel: -4 * mn, author: "maria.k",
				summary: "catalog: price cache TTL 60s -> 90s",
				diff:    "--- a/config/catalog.yaml\n+++ b/config/catalog.yaml\n@@ -6 +6 @@\n-price_cache_ttl: 60s\n+price_cache_ttl: 90s\n"},
		},
		benign: 4,
	}
}

func searchCertExpiry() scenario {
	return scenario{
		name: "search-cert-expiry",
		description: "The TLS serving certificate of search expires at midnight (renewal had failed); frontend calls to search fail with x509 errors and search traffic drops to almost zero. No change is involved. " +
			"Red herring: a search deploy 34m before the alert on the culprit service itself.",
		title:   "Search requests failing on frontend",
		symptom: "frontend",
		alert:   at(20, 0, 2),
		truth:   truth{"search", "", "certificate-expiry", []string{"certificate", "x509", "TLS", "expired"}},
		effects: []effect{
			eff("search", mRate, opMul, 0.03, -2*mn, 0),
			eff("search", mCPU, opAdd, 0.05, -2*mn, 15*sec),
			eff("frontend", mErr, opAdd, 0.2, -2*mn, 0),
			eff("frontend", mLat, opMul, 0.9, -2*mn, 0),
		},
		emitters: []emitter{
			emit("search", "warn", 1.4, -2*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("http: TLS handshake error from %s: remote error: tls: bad certificate", ip(r, 1))
			}),
			emit("frontend", "error", 0.8, -2*mn, 0, func(r *rand.Rand, t time.Time) string {
				return fmt.Sprintf("search upstream error: Get \"https://search.internal:8443/v1/query?q=%s\": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time %s is after 2026-10-20T00:00:00Z", pick(r, terms), t.Format(time.RFC3339))
			}),
		},
		oneshots: []oneshot{
			{rel: -110 * mn, svc: "search", level: "warn", msg: "tls: serving certificate for search.internal expires in 1h52m0s (not after 2026-10-20T00:00:00Z)"},
			{rel: -80 * mn, svc: "search", level: "error", msg: "cert-renewer: renewal for search.internal failed: acme: urn:ietf:params:acme:error:rateLimited: too many certificates already issued, retry after 24h"},
			{rel: -50 * mn, svc: "search", level: "warn", msg: "tls: serving certificate for search.internal expires in 52m0s (not after 2026-10-20T00:00:00Z)"},
		},
		changes: []changeDef{
			{id: "deploy-search-3.3.0", kind: "deploy", svc: "search", rel: -34 * mn, author: "sam.r",
				summary: "search 3.3.0: tune BM25 weights for title field",
				diff:    "--- a/config/ranking.yaml\n+++ b/config/ranking.yaml\n@@ -2,3 +2,3 @@\n fields:\n-  title: 2.0\n+  title: 2.5\n"},
		},
		benign: 5,
	}
}

func searchFeatureFlag() scenario {
	return scenario{
		name: "search-feature-flag",
		description: "Enabling the semantic_rerank flag on search makes every query score 500 candidates with an ML model fetched from catalog; search CPU and latency jump and frontend search pages slow down. " +
			"Red herring: a catalog deploy 5m before the alert while catalog request rate also rises.",
		title:   "Search latency degraded on frontend",
		symptom: "frontend",
		alert:   at(22, 15, 40),
		truth:   truth{"search", "flag-search-semantic-rerank", "config-change", []string{"rerank", "semantic", "flag"}},
		effects: []effect{
			eff("search", mCPU, opAdd, 0.33, -8*mn, 1*mn),
			eff("search", mLat, opMul, 2.4, -8*mn, 1*mn),
			eff("search", mMem, opMul, 1.3, -8*mn, 3*mn),
			eff("search", mErr, opAdd, 0.012, -8*mn, 1*mn),
			eff("catalog", mRate, opMul, 1.6, -8*mn, 1*mn),
		},
		emitters: []emitter{
			emit("search", "info", 0.9, -8*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("rerank: scored %d candidates in %dms (model=minilm-l6)", ms(r, 380, 500), ms(r, 600, 2100))
			}),
			emit("search", "warn", 0.3, -8*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("rerank exceeded latency budget: %dms > 300ms for q=%s", ms(r, 800, 2600), pick(r, terms))
			}),
			emit("frontend", "warn", 0.3, -7*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream search slow: GET http://search:8080/v1/query?q=%s took %dms", pick(r, terms), ms(r, 1200, 3200))
			}),
			emit("search", "error", 0.05, -8*mn, 1*mn, func(r *rand.Rand, _ time.Time) string {
				return "rerank: fetch candidates: Get \"http://catalog:8080/api/products:batch\": context deadline exceeded"
			}),
		},
		changes: []changeDef{
			{id: "flag-search-semantic-rerank", kind: "flag", svc: "search", rel: -8*mn - 15*sec, author: "a.novak",
				summary: "search: enable semantic_rerank for 100% of traffic",
				diff:    "semantic_rerank:\n-  enabled: false\n-  rollout: 0\n+  enabled: true\n+  rollout: 100\n+  candidates: 500\n",
				logMsg:  "feature flag \"semantic_rerank\" updated: enabled=true rollout=100%"},
			{id: "deploy-catalog-1.8.5", kind: "deploy", svc: "catalog", rel: -5 * mn, author: "maria.k",
				summary: "catalog 1.8.5: add batch endpoint GET /api/products:batch",
				diff:    "--- a/internal/http/routes.go\n+++ b/internal/http/routes.go\n@@ -18,3 +18,4 @@\n \tmux.HandleFunc(\"GET /api/products/{id}\", h.product)\n+\tmux.HandleFunc(\"GET /api/products:batch\", h.batch)\n"},
			{id: "commit-c0ffee7", kind: "commit", svc: "frontend", rel: -30 * mn, author: "lin.chen",
				summary: "Show result count on search page",
				diff:    "--- a/web/pages/search.tsx\n+++ b/web/pages/search.tsx\n@@ -20,3 +20,4 @@\n+      <p className=\"count\">{results.total} results</p>\n"},
		},
		benign: 4,
	}
}

func cartCacheTTLRedHerring() scenario {
	return scenario{
		name: "cart-cache-ttl-red-herring",
		description: "cart 2.5.0 passes the cache TTL in seconds where a time.Duration is expected (30 -> 30ns), so cart keys expire almost immediately; as old keys age out over ~25m the cache hit ratio slides and cart latency climbs. " +
			"Red herring (heavy): a large, scary catalog ORM migration deploy 1m before the alert, which logs new warnings but changes nothing.",
		title:   "Cart page latency high on frontend",
		symptom: "frontend",
		alert:   at(21, 13, 15),
		truth:   truth{"cart", "deploy-cart-2.5.0", "bad-deploy", []string{"TTL", "expire", "cache miss"}},
		effects: []effect{
			eff("cart", mCache, opAdd, -0.84, -30*mn, 26*mn),
			eff("cart", mErr, opAdd, 0.012, -15*mn, 12*mn),
			eff("cart", mCPU, opMul, 1.4, -30*mn, 26*mn),
			eff("redis", mCPU, opMul, 1.8, -30*mn, 26*mn),
			eff("catalog", mCPU, opAdd, 0.15, -1*mn, 20*sec).until(1 * mn),
		},
		emitters: []emitter{
			emit("cart", "warn", 0.9, -30*mn, 26*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("cache miss for cart:%s, rebuilding from source (%dms)", hexID(r, 8), ms(r, 40, 400))
			}),
			emit("frontend", "warn", 0.15, -20*mn, 15*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream cart slow: GET http://cart:8080/api/cart/%s took %dms", hexID(r, 8), ms(r, 500, 1300))
			}),
			emit("catalog", "warn", 0.12, -1*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return "ent: option UseLegacyJoins is deprecated and will be removed in v0.15"
			}),
			emit("catalog", "info", 0.1, -1*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("ent: schema check ok (%d tables)", ms(r, 14, 14))
			}),
		},
		changes: []changeDef{
			{id: "deploy-cart-2.5.0", kind: "deploy", svc: "cart", rel: -31 * mn, author: "j.okafor",
				summary: "cart 2.5.0: refactor cache key builder",
				diff:    "--- a/internal/cache/cache.go\n+++ b/internal/cache/cache.go\n@@ -30,8 +30,8 @@ func (c *Cache) Put(ctx context.Context, cart Cart) error {\n-\tkey := fmt.Sprintf(\"cart:%s\", cart.ID)\n-\treturn c.rdb.Set(ctx, key, cart.Bytes(), c.ttl).Err()\n+\tkey := c.keys.Cart(cart.ID)\n+\treturn c.rdb.Set(ctx, key, cart.Bytes(), time.Duration(c.cfg.TTLSeconds)).Err()\n"},
			{id: "deploy-catalog-2.0.0", kind: "deploy", svc: "catalog", rel: -1 * mn, author: "maria.k",
				summary: "catalog 2.0.0: migrate product queries from sqlc to ent ORM",
				diff:    "--- a/internal/store/products.go\n+++ b/internal/store/products.go\n@@ -1,40 +1,38 @@\n-\trows, err := s.q.ListProductsByCategory(ctx, cat)\n+\trows, err := s.ent.Product.Query().Where(product.CategoryEQ(cat)).All(ctx)\n"},
			{id: "flag-frontend-new-checkout", kind: "flag", svc: "frontend", rel: -40 * mn, author: "lin.chen",
				summary: "frontend: enable new checkout layout for 10% of sessions",
				diff:    "new_checkout:\n-  rollout: 0\n+  rollout: 10\n",
				logMsg:  "feature flag \"new_checkout\" updated: rollout=10%"},
		},
		benign: 4,
	}
}

func slowPostgres() scenario {
	return scenario{
		name: "slow-postgres",
		description: "An anti-wraparound autovacuum on a large table saturates postgres disk I/O with no change involved; catalog db_query p99 rises 9x, catalog and frontend latency follow. " +
			"Red herring: a catalog deploy 10m before the alert.",
		title:   "Frontend p99 latency high",
		symptom: "frontend",
		alert:   at(19, 3, 20),
		truth:   truth{"postgres", "", "resource-exhaustion", []string{"vacuum", "I/O", "disk", "checkpoint"}},
		effects: []effect{
			eff("catalog", mDBQ, opMul, 9, -14*mn, 6*mn),
			eff("postgres", mCPU, opMul, 1.8, -15*mn, 4*mn),
			eff("postgres", mMem, opMul, 1.1, -15*mn, 4*mn),
		},
		emitters: []emitter{
			emit("postgres", "warn", 0.05, -13*mn, 0, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("checkpoints are occurring too frequently (%d seconds apart)", ms(r, 9, 24))
			}),
			emit("postgres", "info", 0.45, -12*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("duration: %d.%03d ms  statement: SELECT p.id, p.name, p.price FROM products p WHERE p.category_id = $1", ms(r, 300, 1400), r.IntN(1000))
			}),
			emit("catalog", "warn", 0.4, -12*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("slow query products_by_category on postgres-primary:5432 took %dms", ms(r, 600, 1900))
			}),
			emit("frontend", "warn", 0.15, -11*mn, 4*mn, func(r *rand.Rand, _ time.Time) string {
				return fmt.Sprintf("upstream catalog slow: GET http://catalog:8080/api/products?category=%s took %dms", pick(r, categories), ms(r, 900, 2200))
			}),
		},
		oneshots: []oneshot{
			{rel: -15 * mn, svc: "postgres", level: "info", msg: "automatic aggressive vacuum to prevent wraparound of table \"shop.public.order_items\": index scans: 0"},
			{rel: -9 * mn, svc: "postgres", level: "warn", msg: "could not write block 1849201 of relation base/16384/24611: retrying (I/O wait 2400 ms)"},
		},
		changes: []changeDef{
			{id: "deploy-catalog-1.8.3", kind: "deploy", svc: "catalog", rel: -10 * mn, author: "dev.patel",
				summary: "catalog 1.8.3: add product badge field to API response",
				diff:    "--- a/internal/http/product.go\n+++ b/internal/http/product.go\n@@ -15,3 +15,4 @@ type productJSON struct {\n \tPrice float64 `json:\"price\"`\n+\tBadge string  `json:\"badge,omitempty\"`\n"},
			{id: "cfg-search-refresh", kind: "config", svc: "search", rel: -50 * mn, author: "sam.r",
				summary: "search: nightly reindex window 02:00 -> 03:00",
				diff:    "--- a/config/search.yaml\n+++ b/config/search.yaml\n@@ -9 +9 @@\n-reindex_at: \"02:00\"\n+reindex_at: \"03:00\"\n"},
		},
		benign: 3,
	}
}
