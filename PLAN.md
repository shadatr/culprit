# Culprit: Implementation Plan

> An open-source AI incident investigator. When production breaks, Culprit reads your metrics, logs and recent changes, runs deterministic analysis first, then lets an LLM agent dig deeper. It returns a ranked root-cause report in which **every claim cites evidence**. It is read-only: it never changes your systems.

This document is the single source of truth. Interfaces in §4 are a contract between packages: do not change a signature without updating this file.

---

## 0. Ground rules

1. **Original work only.** No code, names or schemas from any employer.
2. **Never commit secrets.** API keys come from env vars named in config (`api_key_env`). `.env` is git-ignored.
3. **Read-only by design.** No tool may mutate a monitored system. Only GET/query APIs.
4. **Evidence or it didn't happen.** Every hypothesis in a report must cite evidence IDs that exist in the ledger; the agent validates this before finishing.
5. **Deterministic first, LLM second.** Anomaly detection, log-pattern diffing and change correlation run without an LLM. The LLM reasons over their compact output, not over raw data dumps.
6. **Style.** `gofmt`, `go vet` clean. Standard library first (deps allowed: `gopkg.in/yaml.v3`, `modernc.org/sqlite`; anything else needs a plan update). Errors wrapped with `%w`. `context.Context` first on I/O. `log/slog`. Tests run with `-race`. Table-driven tests.
7. **Package ownership.** Each package has one owner while building (§6). Do not edit packages you don't own; if you need a change, note it in your report.

---

## 1. Stack

| Concern | Choice |
|---|---|
| Language | Go 1.26, module `github.com/shadatr/culprit` |
| HTTP | `net/http` with 1.22+ pattern routing |
| LLM | Any OpenAI-compatible Chat Completions API with tool calling (OpenAI, Switchyard, OpenRouter, Ollama, vLLM) |
| Metrics source | Prometheus HTTP API (selectors only, see §3) |
| Logs source | Loki HTTP API |
| Changes source | local git repos (`git log`), JSONL deploy/change file |
| Offline source | fixture scenario directories (§5), used by tests, eval and the demo |
| Storage | SQLite via `modernc.org/sqlite` (pure Go) |
| UI | Next.js 15 App Router, TypeScript, Tailwind 4, Recharts, in `web/` (port 3200) |
| Demo | Docker Compose: demo shop services + Prometheus + Loki + fault injection, in `demo/` (own Go module) |

---

## 2. Layout

```
cmd/culprit/            CLI: investigate | serve | eval   (main.go dispatch; one file per subcommand)
internal/core/          shared types + source interfaces (contract)
internal/llm/           OpenAI-compatible client + Scripted test double
internal/analysis/
  anomaly/              metric anomaly + onset detection
  logpattern/           log template mining (Drain-style) + baseline/incident diff
  correlate/            change ranking + suspect-service localisation over topology
internal/sources/
  prometheus/ loki/ gitlog/ changesfile/ fixture/
internal/tools/         LLM tools over sources + analysis; evidence ledger
internal/agent/         investigation loop, budgets, report validation
internal/report/        Markdown rendering of a Report
internal/config/        YAML config
internal/app/           wiring: config -> sources -> investigator
internal/store/         SQLite: investigations + events
internal/server/        HTTP API, SSE, Alertmanager webhook
internal/eval/          scenario runner + scoring
eval/scenarios/<name>/  fixture scenarios with ground truth
web/                    Next.js UI
demo/                   docker-compose demo environment
```

---

## 3. How an investigation runs

1. **Incident in.** From CLI, UI, or an Alertmanager webhook: title, symptomatic service, start time, window (lookback before start, `after` past start).
2. **Triage (deterministic, no LLM).** `tools.Triage`:
   - Metrics: query triage metrics (config list, or all metrics if the source lists ≤ 60) across the window; `anomaly.Detect` with baseline = before incident start.
   - Logs: for each service, `logpattern.Diff` baseline vs incident → new/spiking templates.
   - Changes: list changes in window; `correlate.RankChanges`; `correlate.SuspectServices` using topology (earliest onset + dependency direction).
   - Every finding is added to the **evidence ledger** as `E1, E2, ...` and summarised in ≤ ~1,500 tokens.
3. **Agent loop.** System prompt + incident + triage summary. The model calls tools (§4.6) to confirm or reject leads. Each tool result is recorded as evidence and returned to the model with its ID. Budgets: max steps (default 12), max wall time (default 3 min), max total tokens (default 120k). When a budget is near, the agent asks for the final report.
4. **Report.** The model calls `submit_report`. The agent validates: ≥ 1 hypothesis, confidences in [0,1], ranked descending, every cited evidence ID exists, timeline sorted. On failure it returns the errors to the model (max 2 repair attempts), then fails with status `invalid_report`.
5. **Events** are emitted throughout (§4.7) so the CLI and UI can show live progress.

Selectors only: metric tools accept Prometheus selectors (`name{label="v",other=~"re"}`), not arbitrary PromQL. Rate/quantile logic lives in recording rules. This keeps tools portable to fixtures and hard to hallucinate.

---

## 4. Contracts

The Go source in `internal/core`, `internal/llm/llm.go`, and the stub files of each package are the authoritative signatures. Summary:

### 4.1 core (owner: lead, done)
`Window`, `Incident`, `Point`, `Series`, `LogQuery`, `LogLine`, `Change`, `Topology`, `Evidence`, `Hypothesis`, `TimelineEntry`, `Usage`, `Report`, `Event`, `EventSink`, and interfaces `MetricsSource`, `LogsSource`, `ChangesSource`.

### 4.2 llm
```go
type Client interface { Chat(ctx context.Context, req Request) (*Response, error) }
func NewOpenAI(baseURL, apiKey string, hc *http.Client) Client
type Scripted struct{ ... }  // replays canned responses; records requests (tests)
```

### 4.3 analysis
```go
anomaly.Detect(series []core.Series, opts anomaly.Options) []anomaly.Anomaly
anomaly.Describe(s core.Series, incidentStart time.Time) anomaly.Stats
logpattern.Mine(lines []core.LogLine) []logpattern.Template
logpattern.Diff(lines []core.LogLine, incidentStart time.Time) []logpattern.Shift
correlate.RankChanges(changes []core.Change, an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, incidentStart time.Time) []correlate.ScoredChange
correlate.SuspectServices(an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, symptom string) []correlate.ScoredService
```

### 4.4 sources
```go
prometheus.New(baseURL string, hc *http.Client) *prometheus.Client          // core.MetricsSource
loki.New(baseURL, serviceLabel string, hc *http.Client) *loki.Client        // core.LogsSource
gitlog.New(repos []gitlog.Repo) *gitlog.Source                              // core.ChangesSource
changesfile.New(path string) *changesfile.Source                            // core.ChangesSource (JSONL of core.Change)
fixture.Load(dir string) (*fixture.Scenario, error)                         // Metrics/Logs/Changes + Incident, Topology, Truth
```

### 4.5 agent
```go
agent.New(deps agent.Deps, cfg agent.Config) *agent.Investigator
(*Investigator).Run(ctx, inc core.Incident, sink core.EventSink) (*core.Report, error)
```

### 4.6 tools exposed to the LLM
| Tool | Args | Returns |
|---|---|---|
| `list_metrics` | `match` | metric names containing match |
| `get_metric` | `selector`, optional `start`,`end` | per-series stats before/after incident + anomaly, evidence ID |
| `search_logs` | `service`, optional `contains`, `level`, `start`,`end`, `limit` | clustered templates with counts + ≤ 5 samples, evidence ID |
| `log_patterns` | `service` | baseline vs incident template shifts, evidence ID |
| `list_changes` | optional `service` | changes in window with correlation score, evidence ID |
| `get_change` | `id` | full change incl. truncated diff, evidence ID |
| `get_topology` | none | dependency map |
| `submit_report` | report JSON (summary, timeline, hypotheses) | validation result |

### 4.7 events (`core.Event.Type`)
`started`, `triage`, `thinking` (model text), `tool_call`, `tool_result`, `evidence`, `report`, `error`, `finished`. `Data` is JSON (see core for payload types).

### 4.8 HTTP API (server)
| Method | Path | Body / Result |
|---|---|---|
| POST | `/api/investigations` | `core.Incident` (id optional) → `{"id": "..."}` (202), runs async |
| GET | `/api/investigations` | `[]InvestigationSummary` newest first |
| GET | `/api/investigations/{id}` | `Investigation` `{id, incident, status, report?, error?, created_at, updated_at}` |
| GET | `/api/investigations/{id}/events` | SSE: replays stored events, then live; `event: <type>`, `data: <core.Event JSON>`; ends after `finished` |
| POST | `/api/webhooks/alertmanager` | Alertmanager v4 payload → one investigation per firing alert (dedupe by fingerprint for 30 min) |
| GET | `/api/health` | `{"ok":true}` |
Status values: `queued`, `running`, `completed`, `failed`, `invalid_report`, `budget_exceeded`. CORS allows `web` origin from config.

---

## 5. Fixture scenario format

```
eval/scenarios/<name>/
  scenario.yaml
  metrics.json      [{"name": "...", "labels": {...}, "points": [[unix_seconds, value], ...]}]
  logs.jsonl        one core.LogLine JSON per line
  changes.json      [core.Change]
```
`scenario.yaml`:
```yaml
name: catalog-bad-deploy
description: A catalog release adds an N+1 query; frontend p99 rises.
incident:
  title: High p99 latency on frontend
  service: frontend
  started_at: 2026-10-20T14:05:00Z
  lookback: 2h
  after: 30m
topology:            # service -> services it calls
  frontend: [catalog, cart]
  catalog: [postgres]
  cart: [redis]
truth:
  service: catalog
  change_id: deploy-catalog-1.8.2      # optional
  category: bad-deploy                 # bad-deploy | config-change | resource-exhaustion | dependency-failure | traffic-spike | certificate-expiry | other
  keywords: [query, N+1]               # top hypothesis should mention at least one
```
Metric naming convention (demo + fixtures): recording-rule style, `service` label always present:
`http_request_duration_seconds:p99`, `http_requests:rate1m`, `http_errors:ratio1m`, `process_cpu:ratio`, `process_memory:bytes`, `db_pool_in_use:ratio`, `db_query_duration_seconds:p99`, `cache_hit:ratio`.

---

## 6. Workstreams

| WS | Owner owns | Done when |
|---|---|---|
| A | `internal/llm`, `internal/tools`, `internal/agent`, `internal/report` | Scripted-LLM end-to-end test over a fixture produces a valid report; OpenAI client tested against httptest |
| B | `internal/sources/*` | each source tested against httptest/temp git repo; fixture loader + selector matcher tested |
| C | `internal/analysis/*` | unit tests incl. synthetic onset detection, Drain templating, ranking |
| D | `internal/store`, `internal/server`, `cmd/culprit/{investigate,serve}.go` | API + SSE tested with httptest and a fake investigator |
| E | `web/` | UI builds, typechecks, lints; works against live API and mock data |
| F | `demo/` | `make demo-up` starts stack; `demo/chaos` injects 3 faults; Culprit config for it |
| G | `internal/eval`, `cmd/culprit/eval.go`, `eval/scenarios/*`, `eval/gen` | ≥ 8 realistic scenarios with red herrings; harness scores top-1/top-3 service, change match, keywords, tokens, time; `EVAL.md` |
| Lead | core, config, app, main.go, README, integration | `go build ./... && go test -race ./...` green; full run on fixtures |

---

## 7. Roadmap after v0.1
Traces (Tempo/Jaeger), Kubernetes events, GitHub deployments API, Slack summary, runbook retrieval, learning from resolved incidents, per-team RBAC.
