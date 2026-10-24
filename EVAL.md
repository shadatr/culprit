# Evaluating Culprit

Culprit claims to find the root cause of an incident. This document describes
how that claim is measured: a set of synthetic incidents with a known root
cause, a harness that runs the real investigator over them, and a scorer that
compares each report to the ground truth.

## What is measured

Every scenario is investigated `-runs` times (LLM output is not deterministic,
so more than one run per scenario gives a more honest number). For each run
the harness records:

| Metric | Definition |
|---|---|
| Completed | Report status is `completed` (not `failed`, `invalid_report`, `budget_exceeded` or an error). |
| Top-1 service | The top-ranked hypothesis names `truth.service` (case-insensitive). |
| Top-3 service | One of the first three hypotheses names `truth.service`. |
| Change | Only for scenarios whose truth has a `change_id`: the top hypothesis' `change_id` equals it. |
| Spurious change | Only for scenarios whose truth has **no** change: the top hypothesis blames a change anyway. Lower is better. Recent deploys are the most common red herring, so this is tracked separately. |
| Correct | Top-1 service is right **and** the change verdict is right (the true change, or no change when there is none). This is the headline number. |
| Category | The top hypothesis' `category` equals `truth.category`. |
| Keyword | At least one `truth.keywords` entry appears (case-insensitive substring) in the top hypothesis' title or explanation. A rough check that the mechanism, not only the service, was found. |
| Citations | Every hypothesis cites at least one evidence ID, and every cited ID (hypotheses and timeline) exists in the report's evidence ledger. |
| Confidence | Confidence of the top hypothesis. |
| Tokens, steps, time | Prompt + completion tokens, agent steps, and wall-clock duration of the run. |

The aggregate reports each rate as hits/runs, mean tokens, steps and duration,
and a **calibration hint**: mean top confidence on correct runs vs wrong runs.
A useful investigator is noticeably less confident when it is wrong.

Rates are computed over all runs, so a failed run counts as a miss, except
Change (runs whose truth has a change), Spurious change and Citations (runs
that produced at least one hypothesis).

## Dev set vs held-out set

There are two scenario sets:

- **Dev set**, `eval/scenarios/` (10 scenarios, shop topology). Culprit's
  deterministic analysis was tuned while looking at these, so a good score
  here partly measures fit to them, not only skill.
- **Held-out set**, `eval/heldout/` (8 scenarios, a different and larger
  topology). Nobody may tune analysis thresholds, prompts or tools against it,
  or inspect triage output on it. It uses different service names, a
  different PRNG seed, different dates (November 2026), a different
  log vocabulary (logfmt access logs) and different failure modes. A test
  (`TestSetsAreDisjoint`) checks that no service or scenario name is shared
  with the dev set.

Report both. The held-out numbers are the ones to quote. A large gap between
dev and held-out means the analysis is overfitted. If the held-out set is
ever used for tuning, it stops being held out: say so, and build a new one.

## Dev scenarios

All scenarios share one topology: `frontend -> catalog, cart, search`,
`catalog -> postgres`, `cart -> redis`, `search -> catalog`. Each covers 2h30m
(2h lookback, 30m after the alert; `cart-memory-leak` uses 3h) at 15s resolution, with the PLAN.md §5
metric set for every service, about 10-20k log lines (access logs, benign
warnings, periodic 404 scanners, rollout lines), and 5-7 changes, most of them
benign. The alert is always raised on `frontend`, so the investigator has to
walk the dependency graph. In several scenarios the real onset is well before
the alert.

| Scenario | Category | Truth | Onset vs alert | Red herring |
|---|---|---|---|---|
| `catalog-bad-deploy` | bad-deploy | catalog, `deploy-catalog-1.8.2` (N+1 image queries) | -6m | cart deploy at -22m; frontend banner flag at -2m |
| `catalog-pool-exhaustion` | resource-exhaustion | catalog, `deploy-catalog-1.9.0` (HTTP call inside DB transaction, "timeout acquiring connection") | pool climbs from -21m, errors at -4m | postgres minor upgrade at -45m; search flag at -10m |
| `redis-outage` | dependency-failure | redis, no change (host failure, connection refused, cache hit ratio falls to 0, redis metrics go missing) | -2m | frontend deploy at -9m on the symptomatic service |
| `frontend-bad-config` | config-change | frontend, `cfg-frontend-catalog-timeout` (catalog client timeout 2s -> 200ms) | -5m | catalog deploy at -11m that adds new log templates |
| `cart-memory-leak` | resource-exhaustion | cart, `deploy-cart-2.4.0` at -2h19m (unbounded per-session map; OOM kills). Uses a 3h lookback. | ramp from the deploy; OOM restarts (memory reset, short scrape gap, startup logs) at -95m, -58m, -27m, -4m, +18m | frontend deploy at -6m; catalog recs flag at -20m |
| `traffic-spike` | traffic-spike | frontend, no change (2.4x newsletter traffic, CPU saturation everywhere) | -9m | search deploy at -13m; catalog cache TTL config at -4m |
| `search-cert-expiry` | certificate-expiry | search, no change (TLS cert expired at midnight, x509 errors, search traffic near zero) | -2m (sudden) | search deploy at -34m on the culprit service itself |
| `search-feature-flag` | config-change | search, `flag-search-semantic-rerank` (expensive rerank path) | -8m | catalog deploy at -5m while catalog request rate also rises |
| `cart-cache-ttl-red-herring` | bad-deploy | cart, `deploy-cart-2.5.0` (TTL in seconds passed as a Duration, keys expire instantly) | gradual from -30m | large catalog ORM migration deploy at -1m that logs new warnings but is harmless |
| `slow-postgres` | resource-exhaustion | postgres, no change (anti-wraparound autovacuum saturates disk I/O) | -14m | catalog deploy at -10m |

The data is synthetic but shaped like real telemetry: a diurnal traffic curve
(scenario start times vary between 00:02 and 19:42 UTC), measurement noise on
every series, latency and errors propagating up the call graph, CPU saturation
feeding back into latency, and logs whose volume follows request rate.
Every deploy or infra change is a process restart: memory drops and climbs
back, and startup lines are logged. A service that is down has no samples
(a scrape gap) instead of values. When a service fails because of a
dependency, its logs name that dependency the way real clients do (for
example `dial tcp 10.0.3.7:6379: connect: connection refused` or
`Get "http://catalog:8080/...": context deadline exceeded`). The
generator is `eval/gen`; the scenario definitions are in
`eval/gen/scenarios.go`.

## Held-out scenarios

Topology: `gateway -> auth, orders, profile`; `auth -> valkey`;
`orders -> inventory, payments, kafka`; `profile -> mysql`;
`inventory -> mysql`; `payments -> fx-api` (external);
`fulfillment -> kafka, inventory, notifications` (a kafka consumer worker);
`notifications -> mail-relay` (external). On top of the PLAN.md §5 metric
set it adds `kafka_consumer_lag:messages` (fulfillment) and `disk_used:ratio`
(mysql). Each scenario spans 2h30m (`notifications-cron-leak`: 3h30m) at 15s
resolution, with 1.8-2.6 MB per scenario.

| Scenario | Category | Symptom | Truth | Red herring |
|---|---|---|---|---|
| `inventory-index-regression` | bad-deploy | gateway | inventory, `deploy-inventory-3.2.0` (`LOWER(sku)` defeats the index; full scans), two hops below the symptom | gateway deploy at -3m; payments 3DS flag at -20m |
| `fulfillment-concurrency-config` | config-change | notifications | fulfillment, `cfg-fulfillment-workers` (worker concurrency 20 -> 2; consumer lag grows) | notifications template deploy at -12m with new warnings |
| `kafka-leader-churn` | dependency-failure | fulfillment | kafka, no change (ISR shrink and leader re-election; NOT_LEADER_OR_FOLLOWER) | fulfillment deploy at -8m, right at the onset |
| `auth-key-rotation` | config-change | gateway | auth, `rotate-auth-signing-key` (old key retired while gateway caches JWKS; 401s) | gateway deploy at -22m; profile flag at -9m |
| `fx-dns-failure` | dependency-failure | gateway | fx-api, no change (external DNS: `no such host`) | payments deploy at -16m |
| `mysql-disk-full` | resource-exhaustion | gateway | mysql, no change (binlogs fill the volume over 100m; errno 28) | inventory deploy at -9m; orders config at -30m |
| `notifications-cron-leak` | resource-exhaustion | fulfillment | notifications, no change (weekly digest cron leaks memory for 2h15m, then OOM kill) | notifications config at -30m; fulfillment deploy at -15m |
| `orders-retry-storm` | config-change | gateway | orders, `cfg-orders-payments-retries` (6 retries, no backoff, changed 50m earlier) amplifies a payments blip | payments deploy at -7m that triggered the blip |

## Reproducing

```sh
# Regenerate fixtures (byte-identical; the eval/gen tests fail if the
# committed files drift from the generator).
go run ./eval/gen -out eval/scenarios          # dev set (same as -set dev)
go run ./eval/gen -set heldout                 # held-out set -> eval/heldout

# Run the eval. The API key is read from the env var named in config
# (default CULPRIT_LLM_API_KEY).
export CULPRIT_LLM_API_KEY=...
go run ./cmd/culprit eval -runs 3 -json eval-results.json -md eval-results.md
go run ./cmd/culprit eval -scenarios eval/heldout -runs 3 -json heldout-results.json -md heldout-results.md

# A subset, another model, a config file with base_url/budgets:
go run ./cmd/culprit eval -only redis-outage,slow-postgres -model gpt-4.1 -config culprit.yaml
```

Flags: `-scenarios` (default `eval/scenarios`), `-only` (comma list), `-runs`
(default 1), `-concurrency` (default 2), `-config`, `-model`, `-json` (path),
`-md` (path, default stdout). Progress per run goes to stderr.

## Adding a scenario

1. Add a function to `eval/gen/scenarios.go` (dev) or `eval/gen/heldout.go`
   (held-out) returning a `scenario` and list it in `scenarios()` or
   `heldoutScenarios()`. New failure modes should normally go to the held-out
   set first. A scenario is:
   - an alert time, title and symptomatic service;
   - `truth` (service, optional change ID, category, keywords that a correct
     explanation would use);
   - `effects`: perturbations of one service metric (`opMul`, `opAdd`,
     `opSet`) with a start, ramp and optional end relative to the alert, or a
     custom shape; propagation to callers is automatic;
   - `emitters` (log lines per second over a window) and `oneshots`;
   - `changes`: the causal change (if any) and at least one deliberate red
     herring, plus `benign` random changes;
   - optional `gaps` where a service's metrics are missing.
2. Name the red herring in the description.
3. Regenerate the set you changed and run `go test ./eval/...`. The
   self-check validates the format (metrics parse, `service` label on every
   series, sorted timestamps, logs inside the window, truth fields and change
   present) and keeps each scenario under 3 MB.
4. Add a row to the scenario table above.

Hand-written scenarios also work: any directory in the PLAN.md §5 format
under `eval/scenarios` is picked up.

## Limitations

- The telemetry is synthetic. It is designed to be realistic enough that the
  deterministic triage and the agent face the same ambiguities as in
  production, but it is not production data.
- Keyword matching is a coarse proxy for "the explanation is right".
- Ten scenarios with few runs give wide error bars; treat differences of one
  or two runs as noise.

## Results

### Triage only (no LLM), 2026-10-24

Deterministic triage (`culprit investigate -scenario <dir> -triage-only`), scored
by the top suspect service and the rank of the true change in the Changes list.
Triage was tuned while looking at the dev set, so the dev numbers are optimistic;
the held-out set was generated afterwards and was not used for tuning.

| Set | Top-1 service | Top-3 service | True change ranked first |
|---|---|---|---|
| Dev (10) | 10/10 | 10/10 | 6/6 |
| Held-out (8) | 2/8 | 6/8 | 3/4 |

Held-out misses: the cause was ranked second behind a caller or a victim
(auth-key-rotation, inventory-index-regression, kafka-leader-churn,
notifications-cron-leak, orders-retry-storm), and an external dependency with no
metrics of its own was absent from the suspects (fx-dns-failure). These are the
cases the LLM step is meant to resolve by reading log messages and change diffs.

### Full investigation (LLM), 2026-10-24

Model `anthropic/claude-haiku-5.5` via OpenRouter, temperature 0.1, 1 run per
scenario, default budgets (12 steps, 3 min, 120k tokens).
Command: `culprit eval -config <config> -scenarios eval/<set> -concurrency 4`.

| Set | Completed | Top-1 service | Top-3 service | True change named | Spurious change blamed | Valid citations | Mean tokens | Mean time |
|---|---|---|---|---|---|---|---|---|
| Dev (10) | 10/10 | 10/10 | 10/10 | 6/6 | 0/4 | 10/10 | 19.4k | 14.0 s |
| Held-out (8) | 8/8 | 6/8 | 7/8 | 3/4 | 0/4 | 8/8 | 26.8k | 16.4 s |

On the held-out set the agent lifts top-1 accuracy from 2/8 (triage alone) to 6/8.
Misses:

- **fx-dns-failure:** the external API has no metrics of its own, and the agent
  blamed an internal service.
- **orders-retry-storm:** the agent blamed a payments deploy that coincided with
  the failure. It ranked the real cause, an orders retry config with zero backoff,
  second at 35% and described the amplification correctly.

Mean top confidence was 0.85 when right and 0.80 when wrong, so confidence is not
yet well calibrated. A single run per scenario is a small sample; repeat with
`-runs 3` before drawing strong conclusions.

An example report is in [docs/example-report.md](docs/example-report.md).
