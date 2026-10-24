# Culprit demo environment

A small, real online shop that breaks on demand, so you can watch Culprit
investigate an actual incident instead of a fixture.

```
loadgen ──> frontend ──> catalog ──> postgres
                    └──> cart ─────> redis
```

- **frontend**, **catalog** and **cart** are one Go binary (`app/`, run with
  `-role`). Each exposes Prometheus metrics on `/metrics` (request latency
  histogram, request and 5xx counters by `service`, `route`, `code`; process CPU
  and memory; DB pool gauges and query latency for catalog; cache lookups for
  cart) and writes JSON logs to stdout and to Loki (stream labels `service`,
  `level`).
- **loadgen** sends about 12 requests per second with mild randomness: home
  pages, product pages (some for products that do not exist, so 404s are
  normal), add-to-cart and view-cart.
- **Prometheus** scrapes every 5s. Recording rules (`prometheus/rules.yml`)
  produce the metric names Culprit expects, each with a `service` label:
  `http_request_duration_seconds:p99`, `http_requests:rate1m`,
  `http_errors:ratio1m`, `process_cpu:ratio`, `process_memory:bytes`,
  `db_pool_in_use:ratio`, `db_query_duration_seconds:p99`, `cache_hit:ratio`.
- **Loki** stores logs on the local filesystem with 24h retention.
- **Grafana** (optional) comes up with both datasources provisioned.

The healthy system is deliberately a little noisy: benign warnings (an expiring
edge certificate, price feed lag, slow template renders) and steady 404s. They
are red herrings, as are the decoy changes `chaos` records.

## Prerequisites

- Docker with Compose v2 (`docker compose version`)
- `make`, `bash`, `curl`
- A built `culprit` binary on your `PATH` (`go build -o bin/culprit ./cmd/culprit`
  from the repo root) and an LLM key in `CULPRIT_LLM_API_KEY`. See
  `culprit.yaml` to use Switchyard, Ollama or another OpenAI-compatible endpoint.

All ports are bound to `127.0.0.1`: frontend 8080, catalog 8081, cart 8082,
Prometheus 9090, Loki 3100, Postgres 5432, Redis 6379, Grafana 3000.
If you already run Postgres or Redis locally, move the demo's ports with
`DEMO_POSTGRES_PORT=55432 DEMO_REDIS_PORT=56379 make up`. Nothing in the demo
connects to them from the host.

## Walkthrough

1. **Start the stack** (from the repo root):
   ```sh
   make -C demo up
   ```
2. **Let it build a baseline.** Wait about 10 minutes. Check
   <http://127.0.0.1:9090/targets> (all three targets up) and try
   `http_request_duration_seconds:p99` in the Prometheus UI.
3. **Break something:**
   ```sh
   make -C demo chaos SCENARIO=bad-deploy    # or: demo/chaos bad-deploy
   ```
   The script records the change in `demo/changes/changes.jsonl`, applies the
   fault, and prints the exact `culprit investigate` command to run.
4. **Investigate.** Wait about 3 minutes, then paste the printed command, e.g.
   ```sh
   export CULPRIT_LLM_API_KEY=...   # your key; never commit it
   culprit investigate -config demo/culprit.yaml -service frontend \
     -start 2026-10-24T14:05:00Z -since 30m -title "High p99 latency on frontend"
   ```
5. **Reset and repeat.** `make -C demo reset`, wait a few minutes for a clean
   baseline, then try another scenario. `make -C demo down` stops everything and
   deletes the data volumes.

## Scenarios

| Scenario | What `chaos` does | Symptom at frontend | Expected root cause |
|---|---|---|---|
| `bad-deploy` | Records deploy `deploy-catalog-1.8.2-*`, restarts catalog as 1.8.2 with `FAULT=n_plus_one` | p99 latency climbs; some timeouts at peak | **catalog**, bad deploy 1.8.2: listing runs one inventory query per product (N+1); `db_query_duration_seconds:p99` and DB pool use rise |
| `pool-leak` | Records deploy `deploy-catalog-1.9.0-*`, restarts catalog as 1.9.0 with `FAULT=pool_leak` | 5xx ratio rises within a minute or two | **catalog**, bad deploy 1.9.0: connections not released; `db_pool_in_use:ratio` reaches 1, logs say `timeout acquiring connection from pool` (resource exhaustion) |
| `redis-down` | Stops the redis container. Records **no** change | Cart routes return 502; home and product pages are fine | **redis** (via cart), dependency failure: `cache_hit:ratio` drops to 0, cart logs `redis command failed ... connection refused / no such host`. No change explains it |
| `bad-config` | Records config change `config-frontend-catalog-timeout-*`, restarts frontend with `CATALOG_TIMEOUT=50ms` | 5xx (504) on home and product pages; catalog itself is healthy | **frontend**, config change: logs say `context deadline exceeded calling catalog`, catalog latency unchanged |
| `reset` | Restores default versions, faults and redis | | |

Every scenario except `reset` also writes one or two decoy changes (an unrelated
commit, a flag rollout, a Postgres tuning change, ...) 8 to 40 minutes earlier.

## Fault switches

Faults can also be toggled at run time without a restart (no change is recorded,
so this is the "someone flipped something" case):

```sh
curl -s -X POST http://127.0.0.1:8081/admin/fault \
  -H 'X-Admin-Token: demo-admin-token' \
  -d '{"fault":"n_plus_one","enabled":true}'
```

| Service | Fault | Effect |
|---|---|---|
| catalog | `n_plus_one` | product listing aggregates inventory once per product (sequential scans, pool contention) |
| catalog | `pool_leak` | about 6% of product lookups never return their connection; disabling the fault releases them |
| frontend | `bad_config` | catalog client timeout drops to 50ms |

The admin endpoint needs the token (`DEMO_ADMIN_TOKEN`, default
`demo-admin-token`, a local demo value, not a secret) and only accepts loopback or
private-network peers. Set `ADMIN_TOKEN` empty to disable it.

## Layout

```
app/                 demo services (own Go module) + Dockerfile
docker-compose.yml   the stack
prometheus/          scrape config + recording rules
loki/                Loki config
grafana/             optional Grafana datasource provisioning
chaos                fault injection script
culprit.yaml         Culprit config for this stack
changes/             changes.jsonl written by chaos (git-ignored)
```

Notes:

- `make up` (or `docker compose up`) after a scenario restores the default
  versions, because scenario settings are passed as environment variables for
  that one command only.
- Run `go test -race ./...` in `app/` for the unit tests (Loki batching, fault
  switches, admin endpoint).
