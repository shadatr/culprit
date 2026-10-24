// Bundled sample data for NEXT_PUBLIC_CULPRIT_MOCK=1: lets the UI be demoed
// and screenshotted without the backend. The headline scenario is the one in
// PLAN.md §5: frontend p99 latency rises because catalog 1.8.2 shipped an N+1
// query. Times are relative to page load so "relative time" reads naturally.

import type { CulpritEvent, Evidence, Hypothesis, Incident, Investigation, InvestigationSummary, NewInvestigation, Report, TimelineEntry } from "./api";

const MIN = 60_000;
const iso = (ms: number) => new Date(ms).toISOString();

// Deterministic noise so screenshots are stable between reloads.
function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface Shape {
  base: number;
  noise: number; // relative
  onset?: number; // ms
  ramp?: number; // ms to reach peak
  peak?: number;
  peakNoise?: number;
}

function series(name: string, service: string, from: number, to: number, shape: Shape, seed: number) {
  const r = rng(seed);
  const pts: { t: string; v: number }[] = [];
  for (let t = from; t <= to; t += 30_000) {
    let v = shape.base * (1 + (r() - 0.5) * 2 * shape.noise);
    if (shape.onset !== undefined && shape.peak !== undefined && t >= shape.onset) {
      const k = Math.min(1, (t - shape.onset) / (shape.ramp ?? 3 * MIN));
      const target = shape.peak * (1 + (r() - 0.5) * 2 * (shape.peakNoise ?? 0.08));
      v = v + (target - v) * k;
    }
    pts.push({ t: iso(t), v: Number(v.toPrecision(4)) });
  }
  return { name, labels: { service }, points: pts };
}

function stats(metric: string, service: string, s: Shape, onsetIso?: string, score?: number) {
  const after = s.peak ?? s.base;
  return {
    metric,
    labels: { service },
    before_mean: s.base,
    before_p95: Number((s.base * (1 + s.noise * 0.9)).toPrecision(3)),
    after_mean: Number((after * 0.96).toPrecision(3)),
    after_max: Number((after * 1.08).toPrecision(3)),
    change_percent: Number((((after * 0.96 - s.base) / s.base) * 100).toFixed(1)),
    anomaly:
      onsetIso && s.peak !== undefined
        ? { metric, labels: { service }, service, direction: s.peak > s.base ? "up" : "down", onset: onsetIso, baseline: s.base, peak: Number((s.peak * 1.08).toPrecision(3)), score: score ?? 8 }
        : undefined,
  };
}

interface StreamItem {
  delay: number;
  event: CulpritEvent;
}

interface Built {
  incident: Incident;
  report: Report;
  stream: StreamItem[];
}

/** Turns an ordered script into events; `delay` is the pause before each. */
class Script {
  items: StreamItem[] = [];
  private seq = 0;
  constructor(private clock: number) {}
  push(type: string, data: unknown, delay: number) {
    this.clock += delay;
    this.items.push({ delay, event: { seq: ++this.seq, type, at: iso(this.clock), data } });
  }
  get now() {
    return this.clock;
  }
}

// ---- headline scenario -----------------------------------------------------

export function buildCatalogScenario(opts: { id: string; start: number; title?: string; service?: string; source?: string; now?: number }): Built {
  const S = opts.start;
  const now = opts.now ?? Date.now();
  const from = S - 120 * MIN;
  const to = Math.min(S + 30 * MIN, Math.max(now, S + MIN));
  const svc = opts.service || "frontend";
  const incident: Incident = {
    id: opts.id,
    title: opts.title || "High p99 latency on frontend",
    service: svc,
    started_at: iso(S),
    window: { start: iso(from), end: iso(S + 30 * MIN) },
    labels: { severity: "page", team: "storefront" },
    source: opts.source || "manual",
  };

  const deployAt = S - 8 * MIN;
  const dbOnset = S - 7 * MIN;
  const poolOnset = S - 6 * MIN;
  const feOnset = S - 3 * MIN;

  const shapes = {
    feP99: { base: 0.21, noise: 0.12, onset: feOnset, ramp: 4 * MIN, peak: 1.34 },
    dbP99: { base: 0.012, noise: 0.15, onset: dbOnset, ramp: 2 * MIN, peak: 0.118 },
    pool: { base: 0.31, noise: 0.1, onset: poolOnset, ramp: 3 * MIN, peak: 0.96, peakNoise: 0.03 },
    cartP99: { base: 0.041, noise: 0.14 },
    feRate: { base: 418, noise: 0.05 },
  } satisfies Record<string, Shape>;

  const changes = [
    {
      change: {
        id: "deploy-catalog-1.8.2",
        kind: "deploy",
        service: "catalog",
        t: iso(deployAt),
        author: "mira.k",
        summary: "catalog 1.8.2: product cards show all gallery images",
        url: "https://git.example.internal/shop/catalog/compare/v1.8.1...v1.8.2",
        diff: [
          "--- a/internal/products/list.go",
          "+++ b/internal/products/list.go",
          "@@ -41,9 +41,15 @@ func (r *Repo) List(ctx context.Context, q Query) ([]Product, error) {",
          " \trows, err := r.db.QueryContext(ctx, listSQL, q.Category, q.Limit)",
          " \tif err != nil {",
          " \t\treturn nil, fmt.Errorf(\"list products: %w\", err)",
          " \t}",
          " \tfor rows.Next() {",
          " \t\tvar p Product",
          " \t\tif err := rows.Scan(&p.ID, &p.Name, &p.Price); err != nil {",
          " \t\t\treturn nil, err",
          " \t\t}",
          "+\t\t// Load the full gallery for each card.",
          "+\t\timgs, err := r.db.QueryContext(ctx,",
          "+\t\t\t\"SELECT * FROM product_images WHERE product_id = $1\", p.ID)",
          "+\t\tif err != nil {",
          "+\t\t\treturn nil, err",
          "+\t\t}",
          "+\t\tp.Images = scanImages(imgs)",
          " \t\tout = append(out, p)",
          " \t}",
        ].join("\n"),
      },
      score: 0.91,
      reasons: [
        "deployed 1m before catalog db_query p99 onset",
        "service catalog has the earliest anomaly",
        "catalog is downstream of symptomatic service frontend",
        "new log template in catalog after deploy",
      ],
    },
    {
      change: { id: "flag-checkout-new-pricing", kind: "flag", service: "checkout", t: iso(S - 21 * MIN), author: "pricing-bot", summary: "checkout_new_pricing: 5% to 25%" },
      score: 0.14,
      reasons: ["checkout shows no anomaly", "checkout is not downstream of frontend's slow path"],
    },
    {
      change: { id: "config-cart-redis-ttl", kind: "config", service: "cart", t: iso(S - 47 * MIN), author: "devon", summary: "cart: redis session TTL 30m to 2h" },
      score: 0.11,
      reasons: ["47m before onset", "cart latency unchanged"],
    },
    {
      change: {
        id: "deploy-frontend-2.14.0",
        kind: "deploy",
        service: "frontend",
        t: iso(S - 95 * MIN),
        author: "sam.o",
        summary: "frontend 2.14.0: new product grid layout",
      },
      score: 0.22,
      reasons: ["same service as the symptom", "deployed 92m before onset; baseline was healthy afterwards"],
    },
  ];

  const ev = (id: string, kind: string, tool: string, input: unknown, summary: string, data: unknown, at: number): Evidence => ({
    id,
    kind,
    tool,
    input,
    summary,
    data,
    at: iso(at),
  });

  const t0 = S + 1 * MIN; // investigation starts a minute after the alert
  const evidence: Evidence[] = [
    ev(
      "E1",
      "anomaly",
      "triage",
      { metrics: 8 },
      "3 anomalous series: catalog db_query_duration_seconds:p99 x9.8 from -7m, catalog db_pool_in_use:ratio x3.1 from -6m, frontend http_request_duration_seconds:p99 x6.4 from -3m.",
      {
        anomalies: [
          { metric: "db_query_duration_seconds:p99", labels: { service: "catalog" }, service: "catalog", direction: "up", onset: iso(dbOnset), baseline: 0.012, peak: 0.127, score: 14.2 },
          { metric: "db_pool_in_use:ratio", labels: { service: "catalog" }, service: "catalog", direction: "up", onset: iso(poolOnset), baseline: 0.31, peak: 0.98, score: 11.7 },
          { metric: "http_request_duration_seconds:p99", labels: { service: "frontend" }, service: "frontend", direction: "up", onset: iso(feOnset), baseline: 0.21, peak: 1.45, score: 9.6 },
        ],
      },
      t0 + 2_000,
    ),
    ev(
      "E2",
      "log_patterns",
      "triage",
      { service: "catalog" },
      "catalog: 2 new templates since -7m, led by 'slow query: SELECT * FROM product_images ...' at 1.8k/min.",
      [
        {
          template: {
            id: "t-9c1e",
            pattern: "slow query: SELECT * FROM product_images WHERE product_id = <*> took <*> ms",
            count: 13250,
            level: "warn",
            service: "catalog",
            sample: "slow query: SELECT * FROM product_images WHERE product_id = 88412 took 41 ms",
            first: iso(deployAt + 40_000),
            last: iso(S + 4 * MIN),
          },
          baseline_rate: 0,
          incident_rate: 1840,
          new: true,
          score: 18.4,
          first_in_incident: iso(deployAt + 40_000),
        },
        {
          template: {
            id: "t-41aa",
            pattern: "db pool wait <*> ms (in use <*>/<*>)",
            count: 1530,
            level: "warn",
            service: "catalog",
            sample: "db pool wait 212 ms (in use 48/50)",
            first: iso(poolOnset),
            last: iso(S + 4 * MIN),
          },
          baseline_rate: 0,
          incident_rate: 212,
          new: true,
          score: 9.1,
          first_in_incident: iso(poolOnset),
        },
        {
          template: {
            id: "t-0b77",
            pattern: "GET /products?category=<*> 200 <*> ms",
            count: 61200,
            level: "info",
            service: "catalog",
            sample: "GET /products?category=shoes 200 734 ms",
            first: iso(from),
            last: iso(S + 4 * MIN),
          },
          baseline_rate: 402,
          incident_rate: 409,
          new: false,
          score: 0.4,
          first_in_incident: iso(S),
        },
      ],
      t0 + 2_400,
    ),
    ev(
      "E3",
      "log_patterns",
      "triage",
      { service: "frontend" },
      "frontend: new 'upstream catalog timeout after <*> ms' at 46/min; 'render product grid' unchanged.",
      [
        {
          template: {
            id: "t-77d0",
            pattern: "upstream catalog timeout after <*> ms path=<*>",
            count: 290,
            level: "error",
            service: "frontend",
            sample: "upstream catalog timeout after 1500 ms path=/category/shoes",
            first: iso(feOnset + 50_000),
            last: iso(S + 4 * MIN),
          },
          baseline_rate: 0,
          incident_rate: 46,
          new: true,
          score: 7.8,
          first_in_incident: iso(feOnset + 50_000),
        },
        {
          template: {
            id: "t-12fe",
            pattern: "render product grid items=<*> in <*> ms",
            count: 50110,
            level: "info",
            service: "frontend",
            sample: "render product grid items=24 in 18 ms",
            first: iso(from),
            last: iso(S + 4 * MIN),
          },
          baseline_rate: 331,
          incident_rate: 318,
          new: false,
          score: 0.1,
          first_in_incident: iso(S),
        },
      ],
      t0 + 2_700,
    ),
    ev("E4", "changes", "triage", {}, "4 changes in window; top: deploy-catalog-1.8.2 (0.91), deployed 1m before the first anomaly.", changes, t0 + 3_000),
    ev(
      "E5",
      "suspects",
      "triage",
      { symptom: svc },
      "Suspects: catalog 0.86, postgres 0.41, frontend 0.22, cart 0.06.",
      [
        { service: "catalog", score: 0.86, earliest_onset: iso(dbOnset), reasons: ["earliest onset (-7m)", "2 new log templates", "downstream of frontend", "recent deploy"] },
        { service: "postgres", score: 0.41, earliest_onset: iso(dbOnset), reasons: ["called by catalog", "no metrics of its own in window"] },
        { service: "frontend", score: 0.22, earliest_onset: iso(feOnset), reasons: ["symptomatic service", "onset 4m after catalog"] },
        { service: "cart", score: 0.06, reasons: ["no anomaly", "no new log templates"] },
      ],
      t0 + 3_200,
    ),
    ev(
      "E6",
      "metric",
      "get_metric",
      { selector: 'http_request_duration_seconds:p99{service="frontend"}' },
      "frontend p99 0.21s before, 1.29s mean after (+514%), onset -3m.",
      {
        selector: 'http_request_duration_seconds:p99{service="frontend"}',
        series: [series("http_request_duration_seconds:p99", "frontend", from, to, shapes.feP99, 11)],
        stats: [stats("http_request_duration_seconds:p99", "frontend", shapes.feP99, iso(feOnset), 9.6)],
      },
      t0 + 9_000,
    ),
    ev(
      "E7",
      "metric",
      "get_metric",
      { selector: 'db_query_duration_seconds:p99{service="catalog"}' },
      "catalog db query p99 12ms before, 113ms after (+842%), onset -7m, 1m after deploy-catalog-1.8.2.",
      {
        selector: 'db_query_duration_seconds:p99{service="catalog"}',
        series: [series("db_query_duration_seconds:p99", "catalog", from, to, shapes.dbP99, 23)],
        stats: [stats("db_query_duration_seconds:p99", "catalog", shapes.dbP99, iso(dbOnset), 14.2)],
      },
      t0 + 15_000,
    ),
    ev(
      "E8",
      "change",
      "get_change",
      { id: "deploy-catalog-1.8.2" },
      "deploy-catalog-1.8.2 adds a per-product SELECT on product_images inside the product list loop.",
      changes[0]?.change,
      t0 + 21_000,
    ),
    ev(
      "E9",
      "logs",
      "search_logs",
      { service: "catalog", contains: "product_images", limit: 200 },
      "200 lines, 1 template: one product_images query per product; 24 per request on category pages.",
      {
        templates: [
          {
            id: "t-9c1e",
            pattern: "slow query: SELECT * FROM product_images WHERE product_id = <*> took <*> ms",
            count: 200,
            level: "warn",
            service: "catalog",
            sample: "slow query: SELECT * FROM product_images WHERE product_id = 88412 took 41 ms",
            first: iso(S + 2 * MIN),
            last: iso(S + 4 * MIN),
          },
        ],
        lines: [88412, 88413, 88415, 88419, 88420].map((pid, i) => ({
          t: iso(S + 3 * MIN + i * 90),
          service: "catalog",
          level: "warn",
          message: `slow query: SELECT * FROM product_images WHERE product_id = ${pid} took ${38 + ((i * 7) % 11)} ms request_id=c7f2a1`,
        })),
      },
      t0 + 26_000,
    ),
    ev(
      "E10",
      "metric",
      "get_metric",
      { selector: 'db_pool_in_use:ratio{service="catalog"}' },
      "catalog DB pool 31% before, 94% after; saturated from -6m.",
      {
        selector: 'db_pool_in_use:ratio{service="catalog"}',
        series: [series("db_pool_in_use:ratio", "catalog", from, to, shapes.pool, 37)],
        stats: [stats("db_pool_in_use:ratio", "catalog", shapes.pool, iso(poolOnset), 11.7)],
      },
      t0 + 30_000,
    ),
    ev(
      "E11",
      "metric",
      "get_metric",
      { selector: 'http_request_duration_seconds:p99{service=~"cart|frontend"}' },
      "cart p99 flat at 41ms; frontend request rate flat at ~418/s, so not a traffic spike.",
      {
        series: [
          series("http_request_duration_seconds:p99", "cart", from, to, shapes.cartP99, 41),
          series("http_requests:rate1m", "frontend", from, to, shapes.feRate, 43),
        ],
        stats: [stats("http_request_duration_seconds:p99", "cart", shapes.cartP99), stats("http_requests:rate1m", "frontend", shapes.feRate)],
      },
      t0 + 34_000,
    ),
    ev("E12", "topology", "get_topology", {}, "frontend calls catalog and cart; catalog calls postgres; cart calls redis.", { frontend: ["catalog", "cart"], catalog: ["postgres"], cart: ["redis"] }, t0 + 36_000),
  ];

  const hypotheses: Hypothesis[] = [
    {
      title: "N+1 query introduced by catalog 1.8.2 saturates the DB pool",
      explanation:
        "catalog 1.8.2 loads product_images with one query per product inside the list loop (E8). One minute after the deploy, catalog's DB query p99 rose from 12ms to 113ms (E7) and a new 'SELECT * FROM product_images WHERE product_id = <*>' template appeared at 1.8k/min (E2, E9). The extra queries exhausted the connection pool (E10, 31% to 94%), so catalog list requests queued and frontend p99 followed four minutes later (E6), with upstream catalog timeouts in frontend logs (E3). Traffic and cart latency were flat (E11), ruling out load and the cart path.",
      service: "catalog",
      category: "bad-deploy",
      change_id: "deploy-catalog-1.8.2",
      confidence: 0.86,
      evidence_ids: ["E8", "E7", "E2", "E9", "E10", "E6", "E3", "E11"],
      next_steps: [
        "Roll back catalog to 1.8.1 and confirm frontend p99 returns under 300ms",
        "Replace the per-product query with one batched query (WHERE product_id = ANY($1))",
        "Add a query-count assertion to the product list handler test",
      ],
    },
    {
      title: "Postgres degradation slowing all catalog queries",
      explanation:
        "The pool saturation (E10) and slow queries could also come from Postgres itself. Against it: the slow template is a single new query shape (E2) that only exists after the deploy, and onset is 1m after deploy-catalog-1.8.2 (E4). No Postgres-side metrics were available to confirm or reject this fully (E5).",
      service: "postgres",
      category: "dependency-failure",
      confidence: 0.09,
      evidence_ids: ["E10", "E2", "E4", "E5"],
      next_steps: ["Check pg_stat_activity and Postgres CPU for the same window"],
    },
    {
      title: "Frontend 2.14.0 product grid regression",
      explanation:
        "frontend shipped 2.14.0 earlier in the window (E4). Its own render template is unchanged (E3), latency stayed at baseline for 90 minutes after the deploy (E6), and the slowness is on the catalog call path (E3, E12).",
      service: "frontend",
      category: "bad-deploy",
      change_id: "deploy-frontend-2.14.0",
      confidence: 0.03,
      evidence_ids: ["E4", "E3", "E6", "E12"],
      next_steps: [],
    },
  ];

  const timeline: TimelineEntry[] = [
    { t: iso(S - 95 * MIN), text: "frontend 2.14.0 deployed; latency stays at baseline", evidence_ids: ["E4", "E6"] },
    { t: iso(deployAt), text: "catalog 1.8.2 deployed with per-product image query", evidence_ids: ["E4", "E8"] },
    { t: iso(deployAt + 40_000), text: "New 'SELECT * FROM product_images' log template appears in catalog", evidence_ids: ["E2", "E9"] },
    { t: iso(dbOnset), text: "catalog DB query p99 rises from 12ms toward 113ms", evidence_ids: ["E7"] },
    { t: iso(poolOnset), text: "catalog DB pool climbs past 90% in use; pool waits logged", evidence_ids: ["E10", "E2"] },
    { t: iso(feOnset), text: "frontend p99 latency starts rising", evidence_ids: ["E6"] },
    { t: iso(feOnset + 50_000), text: "frontend logs upstream catalog timeouts", evidence_ids: ["E3"] },
    { t: iso(S), text: "Alert fires: High p99 latency on frontend", evidence_ids: ["E6"] },
  ];

  const sc = new Script(t0 - 1_000);
  sc.push("started", incident, 300);
  for (const e of evidence.slice(0, 5)) sc.push("evidence", e, 220);
  sc.push(
    "triage",
    {
      summary:
        "3 anomalies, earliest catalog db_query_duration_seconds:p99 at -7m (E1). New catalog log templates incl. slow product_images query (E2); frontend upstream catalog timeouts (E3). Top change deploy-catalog-1.8.2 at -8m, score 0.91 (E4). Suspects: catalog 0.86, postgres 0.41 (E5).",
      evidence_ids: ["E1", "E2", "E3", "E4", "E5"],
    },
    600,
  );

  let call = 0;
  const tool = (name: string, args: unknown, e: Evidence | undefined, thinking: string | null, gap = 1600) => {
    if (thinking) sc.push("thinking", { text: thinking }, gap);
    const id = `call_${++call}`;
    sc.push("tool_call", { id, name, arguments: args }, thinking ? 700 : gap);
    if (e) sc.push("evidence", e, 1100);
    sc.push("tool_result", { id, name, evidence_id: e?.id ?? "", summary: e?.summary ?? "", error: "" }, e ? 120 : 900);
  };

  tool(
    "get_metric",
    { selector: 'http_request_duration_seconds:p99{service="frontend"}' },
    evidence[5],
    "Triage points away from frontend: the earliest anomaly is in catalog, four minutes before frontend's latency moved. First I want the symptom itself confirmed with a clean before/after.",
  );
  tool(
    "get_metric",
    { selector: 'db_query_duration_seconds:p99{service="catalog"}' },
    evidence[6],
    "Frontend p99 confirmed: 0.21s to ~1.3s from -3m. Now the earliest signal: catalog's DB query latency. If it jumps right after the 1.8.2 deploy, the deploy is the prime suspect.",
  );
  tool(
    "get_change",
    { id: "deploy-catalog-1.8.2" },
    evidence[7],
    "DB query p99 went from 12ms to 113ms one minute after deploy-catalog-1.8.2. Reading the diff to see whether it changed query behaviour.",
  );
  tool(
    "search_logs",
    { service: "catalog", contains: "product_images", limit: 200 },
    evidence[8],
    "The diff runs a SELECT on product_images inside the rows loop: a classic N+1. If that's live, catalog logs should show one product_images query per product, many per request.",
  );
  tool(
    "get_metric",
    { selector: 'db_pool_in_use:ratio{service="catalog"}' },
    evidence[9],
    "Logs match: 24 product_images queries per category request. That should exhaust the DB connection pool, which would explain queueing rather than just slower queries.",
  );
  tool(
    "get_metric",
    { selector: 'http_request_duration_seconds:p99{service=~"cart|frontend"}' },
    evidence[10],
    "Pool saturated at 94%. Before concluding, rule out the alternatives: a traffic spike, or the cart path being slow too.",
  );
  tool("get_topology", {}, evidence[11], null, 900);

  const report: Report = {
    incident_id: opts.id,
    status: "completed",
    summary:
      "Frontend p99 latency rose from 0.21s to ~1.3s because catalog 1.8.2, deployed 8 minutes before the alert, introduced an N+1 query: it fetches product_images once per product in the list loop. The extra queries saturated catalog's DB connection pool, so catalog responses queued and frontend's calls to catalog slowed and timed out. Traffic, cart and the earlier frontend deploy are ruled out. Rolling back catalog to 1.8.1 is the fastest mitigation.",
    timeline,
    hypotheses,
    evidence,
    usage: { steps: 8, tool_calls: 8, prompt_tokens: 38420, completion_tokens: 2915 },
    model: "gpt-4.1",
    started_at: iso(t0),
    finished_at: "",
  };

  sc.push(
    "thinking",
    {
      text: "Cart p99 and request rate are flat, and topology confirms frontend's slow path goes through catalog to postgres. The evidence chain is complete: deploy, N+1 query shape, DB latency, pool saturation, then frontend latency. Submitting the report.",
    },
    1500,
  );
  const submitId = `call_${++call}`;
  sc.push("tool_call", { id: submitId, name: "submit_report", arguments: { summary: report.summary, hypotheses: hypotheses.map((h) => ({ title: h.title, confidence: h.confidence, evidence_ids: h.evidence_ids })) } }, 1200);
  sc.push("tool_result", { id: submitId, name: "submit_report", evidence_id: "", summary: "report valid: 3 hypotheses, 12 evidence items cited", error: "" }, 600);
  report.finished_at = iso(sc.now + 200);
  sc.push("report", report, 200);
  sc.push("finished", { status: "completed" }, 150);

  return { incident, report, stream: sc.items };
}

// ---- smaller scenarios for the list ---------------------------------------

function buildCheckoutConfig(id: string, S: number): Built {
  const from = S - 60 * MIN;
  const to = S + 30 * MIN;
  const incident: Incident = {
    id,
    title: "Checkout 5xx ratio above 2%",
    service: "checkout",
    started_at: iso(S),
    window: { start: iso(from), end: iso(to) },
    labels: { alertname: "CheckoutErrorRatio", severity: "page" },
    source: "alertmanager",
  };
  const t0 = S + MIN;
  const errShape: Shape = { base: 0.002, noise: 0.4, onset: S - 2 * MIN, ramp: MIN, peak: 0.064, peakNoise: 0.15 };
  const evidence: Evidence[] = [
    {
      id: "E1",
      kind: "metric",
      tool: "get_metric",
      summary: "checkout error ratio 0.2% before, 6.1% after; onset -2m.",
      data: { series: [series("http_errors:ratio1m", "checkout", from, to, errShape, 5)], stats: [stats("http_errors:ratio1m", "checkout", errShape, iso(S - 2 * MIN), 12.5)] },
      at: iso(t0 + 4_000),
    },
    {
      id: "E2",
      kind: "changes",
      tool: "list_changes",
      summary: "1 config change 3m before onset: payments client timeout 2s to 200ms.",
      data: [
        {
          change: { id: "config-payments-timeout", kind: "config", service: "checkout", t: iso(S - 5 * MIN), author: "ops-bot", summary: "payments.client.timeout: 2s -> 200ms", diff: "-  timeout: 2s\n+  timeout: 200ms" },
          score: 0.88,
          reasons: ["3m before onset", "same service as symptom"],
        },
      ],
      at: iso(t0 + 6_000),
    },
    {
      id: "E3",
      kind: "log_patterns",
      tool: "log_patterns",
      summary: "New 'payments call failed: context deadline exceeded after <*>' at 120/min.",
      data: [
        {
          template: { id: "t-aa01", pattern: "payments call failed: context deadline exceeded after <*> order=<*>", count: 1450, level: "error", service: "checkout", sample: "payments call failed: context deadline exceeded after 200ms order=ord_88123", first: iso(S - 2 * MIN), last: iso(S + 9 * MIN) },
          baseline_rate: 0,
          incident_rate: 120,
          new: true,
          score: 11,
          first_in_incident: iso(S - 2 * MIN),
        },
      ],
      at: iso(t0 + 9_000),
    },
  ];
  const report: Report = {
    incident_id: id,
    status: "completed",
    summary: "A config change cut the payments client timeout from 2s to 200ms; payments p95 is ~350ms, so a large share of checkout calls now time out and return 5xx.",
    timeline: [
      { t: iso(S - 5 * MIN), text: "payments client timeout lowered to 200ms", evidence_ids: ["E2"] },
      { t: iso(S - 2 * MIN), text: "deadline-exceeded errors begin; error ratio climbs", evidence_ids: ["E1", "E3"] },
    ],
    hypotheses: [
      {
        title: "Payments client timeout set below payments latency",
        explanation: "The timeout change (E2) precedes the error onset by 3 minutes (E1) and every new error is a deadline exceeded at exactly 200ms (E3).",
        service: "checkout",
        category: "config-change",
        change_id: "config-payments-timeout",
        confidence: 0.92,
        evidence_ids: ["E1", "E2", "E3"],
        next_steps: ["Revert payments.client.timeout to 2s", "Add a config lint that the timeout exceeds the payments p99"],
      },
      { title: "Payments provider partial outage", explanation: "Possible but errors are all client-side deadlines at 200ms (E3).", service: "payments", category: "dependency-failure", confidence: 0.06, evidence_ids: ["E3"], next_steps: [] },
    ],
    evidence,
    usage: { steps: 4, tool_calls: 4, prompt_tokens: 14200, completion_tokens: 1180 },
    model: "gpt-4.1",
    started_at: iso(t0),
    finished_at: iso(t0 + 19_000),
  };
  return { incident, report, stream: simpleStream(incident, report, t0) };
}

function buildCartMemory(id: string, S: number): Built {
  const from = S - 360 * MIN;
  const to = S + 30 * MIN;
  const incident: Incident = { id, title: "Cart memory climbing toward limit", service: "cart", started_at: iso(S), window: { start: iso(from), end: iso(to) }, source: "cli" };
  const t0 = S + 2 * MIN;
  const memShape: Shape = { base: 410e6, noise: 0.03, onset: S - 200 * MIN, ramp: 220 * MIN, peak: 1.62e9, peakNoise: 0.02 };
  const evidence: Evidence[] = [
    {
      id: "E1",
      kind: "metric",
      tool: "get_metric",
      summary: "cart RSS grows linearly from 410MB to 1.6GB over ~3.5h; no step change.",
      data: { series: [series("process_memory:bytes", "cart", from, to, memShape, 9)], stats: [stats("process_memory:bytes", "cart", memShape, iso(S - 200 * MIN), 6.2)] },
      at: iso(t0 + 5_000),
    },
    { id: "E2", kind: "changes", tool: "list_changes", summary: "No cart changes in window.", data: [], at: iso(t0 + 7_000) },
  ];
  const report: Report = {
    incident_id: id,
    status: "budget_exceeded",
    summary: "Steady memory growth in cart without a matching change. The step budget ran out before the leak source was found.",
    timeline: [{ t: iso(S - 200 * MIN), text: "cart memory starts growing linearly", evidence_ids: ["E1"] }],
    hypotheses: [
      {
        title: "Slow leak in cart session cache",
        explanation: "Linear growth with no change in the window (E1, E2) suggests an unbounded cache; not confirmed.",
        service: "cart",
        category: "resource-exhaustion",
        confidence: 0.41,
        evidence_ids: ["E1", "E2"],
        next_steps: ["Take a heap profile of one cart pod", "Rerun with a larger step budget"],
      },
    ],
    evidence,
    usage: { steps: 12, tool_calls: 14, prompt_tokens: 117300, completion_tokens: 6100 },
    model: "gpt-4.1",
    started_at: iso(t0),
    finished_at: iso(t0 + 180_000),
  };
  return { incident, report, stream: simpleStream(incident, report, t0) };
}

function simpleStream(incident: Incident, report: Report, t0: number): StreamItem[] {
  const sc = new Script(t0);
  sc.push("started", incident, 0);
  for (const e of report.evidence) sc.push("evidence", e, 0);
  sc.push("report", report, 0);
  sc.push("finished", { status: report.status }, 0);
  return sc.items;
}

// ---- store -----------------------------------------------------------------

interface Entry {
  inv: Investigation;
  built?: Built;
  error?: string;
}

let store: Map<string, Entry> | null = null;

function ensure(): Map<string, Entry> {
  if (store) return store;
  const now = Date.now();
  store = new Map();
  const add = (built: Built, status: string, created: number, updated: number) => {
    const id = built.incident.id ?? "";
    store!.set(id, {
      built,
      inv: { id, incident: built.incident, status, report: status === "running" ? undefined : built.report, created_at: iso(created), updated_at: iso(updated) },
    });
  };
  const live = buildCatalogScenario({ id: "inv-live-frontend-slo", start: now - 2 * MIN, title: "FrontendLatencySLOBurn", source: "alertmanager", now });
  add(live, "running", now - 50_000, now - 5_000);
  const main = buildCatalogScenario({ id: "inv-catalog-n1", start: now - 38 * MIN, now });
  add(main, "completed", now - 37 * MIN, now - 36 * MIN);
  const co = buildCheckoutConfig("inv-checkout-timeout", now - 5 * 60 * MIN);
  add(co, "completed", now - 299 * MIN, now - 298 * MIN);
  const cart = buildCartMemory("inv-cart-memory", now - 27 * 60 * MIN);
  add(cart, "budget_exceeded", now - 27 * 60 * MIN, now - 27 * 60 * MIN + 4 * MIN);

  const failedAt = now - 50 * 60 * MIN;
  const failedInc: Incident = {
    id: "inv-search-latency",
    title: "Search latency SLO burn",
    service: "search",
    started_at: iso(failedAt),
    window: { start: iso(failedAt - 60 * MIN), end: iso(failedAt + 30 * MIN) },
    source: "alertmanager",
  };
  const err = "llm: 401 Unauthorized from the model endpoint (check llm.api_key_env)";
  store.set("inv-search-latency", {
    inv: { id: "inv-search-latency", incident: failedInc, status: "failed", error: err, created_at: iso(failedAt), updated_at: iso(failedAt + 6_000) },
    error: err,
  });
  return store;
}

function topOf(inv: Investigation): Hypothesis | undefined {
  return inv.report?.hypotheses[0];
}

export function mockList(): InvestigationSummary[] {
  return [...ensure().values()]
    .map(({ inv }) => ({
      id: inv.id,
      title: inv.incident.title,
      service: inv.incident.service,
      status: inv.status,
      created_at: inv.created_at,
      updated_at: inv.updated_at,
      top_hypothesis: topOf(inv)?.title,
      confidence: topOf(inv)?.confidence,
    }))
    .sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
}

export function mockGet(id: string): Investigation | undefined {
  const e = ensure().get(id);
  return e ? structuredClone(e.inv) : undefined;
}

export function mockCreate(inc: NewInvestigation): string {
  const s = ensure();
  const id = `inv-${Math.random().toString(36).slice(2, 10)}`;
  const start = Date.parse(inc.started_at) || Date.now();
  const built = buildCatalogScenario({ id, start, title: inc.title, service: inc.service, source: inc.source });
  built.incident.window = inc.window;
  const now = Date.now();
  s.set(id, { built, inv: { id, incident: built.incident, status: "running", created_at: iso(now), updated_at: iso(now) } });
  return id;
}

/** Events to replay for id; delays only apply while the investigation is live. */
export function mockStream(id: string): StreamItem[] | null {
  const e = ensure().get(id);
  if (!e) return null;
  const live = e.inv.status === "running" || e.inv.status === "queued";
  if (e.built) return e.built.stream.map((it) => ({ delay: live ? it.delay : 0, event: it.event }));
  const inc = e.inv.incident;
  const t = Date.parse(e.inv.created_at);
  return [
    { delay: 0, event: { seq: 1, type: "started", at: iso(t), data: inc } },
    { delay: 0, event: { seq: 2, type: "error", at: iso(t + 5_000), data: { message: e.error ?? "investigation failed" } } },
    { delay: 0, event: { seq: 3, type: "finished", at: iso(t + 6_000), data: { status: e.inv.status } } },
  ];
}

/** Marks a replayed live investigation as finished so reloads show the report. */
export function mockFinish(id: string): void {
  const e = ensure().get(id);
  if (!e?.built || (e.inv.status !== "running" && e.inv.status !== "queued")) return;
  e.inv = { ...e.inv, status: e.built.report.status, report: e.built.report, updated_at: iso(Date.now()) };
}
