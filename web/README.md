# culprit-web

The Culprit UI: a Next.js 15 (App Router) app that lists investigations, starts new ones, and shows an investigation live as it runs, then its ranked, evidence-cited report.

## Run

```sh
cd web
npm install
cp .env.example .env.local   # optional
npm run dev                  # http://localhost:3200
```

It talks to the API from `culprit serve` (PLAN.md §4.8). Set the API base with:

| Variable | Default | Meaning |
|---|---|---|
| `NEXT_PUBLIC_CULPRIT_API` | `http://localhost:8090` | Base URL of the Culprit API. The API's CORS config must allow this UI's origin. |
| `NEXT_PUBLIC_CULPRIT_MOCK` | unset | `1` uses bundled sample data and no network at all. |

`NEXT_PUBLIC_*` values are inlined at build time, so set them before `npm run build`.

## Mock mode

```sh
NEXT_PUBLIC_CULPRIT_MOCK=1 npm run dev
```

The UI then runs fully offline on sample data (`lib/mock.ts`):

- **High p99 latency on frontend** (completed): the root cause is catalog 1.8.2 adding an N+1 query. It has 12 evidence items: metric series with onsets, log pattern shifts, ranked changes with a diff, suspect services and topology.
- **FrontendLatencySLOBurn** (running): the same story, replayed live with realistic delays. It takes about 30 seconds and shows triage, reasoning, tool calls and results, then the report. Once it finishes it stays completed until you reload the page.
- One completed config-change investigation, one that ran out of budget, and one that failed.
- Starting an investigation from `/new` creates a new live replay that uses your title and service.

Nothing persists across reloads. This mode is meant for demos and screenshots.

## Pages

- `/`: investigation list (status, service, top hypothesis with confidence, relative time). It refreshes every 2.5s while something is running, otherwise every 10s. When the list is empty it shows how to start an investigation from the CLI, an Alertmanager webhook or the API.
- `/new`: title, service, start time, lookback and after. It POSTs `/api/investigations` and then opens the investigation.
- `/investigations/[id]`: live step feed from `GET /api/investigations/{id}/events` (SSE). The browser resumes with `Last-Event-ID`, duplicate seqs are dropped, the page reconnects with backoff if the stream closes early, and it stops after `finished`. The report view shows:
  - the summary, ranked hypothesis cards and the timeline
  - the evidence index, and an evidence panel with renderers for metric series (incident start, onset, baseline band), log pattern shifts, changes with diffs, suspects, anomalies, log templates and lines, topology, and raw JSON as a fallback
  - Copy as Markdown, and a usage footer
  - `?e=E7` deep-links to a piece of evidence.

## Code

- `lib/api.ts`: types that mirror `internal/core/core.go`, defensive parsers (Go `null` slices, unknown fields) and the fetch client.
- `lib/events.ts`: a pure reducer (`applyEvent`, `foldEvents`) that turns events into view state, plus the `useInvestigationEvents` SSE hook.
- `lib/evidence.ts`: recognises the analysis payload shapes (`anomaly.Stats`/`Anomaly`, `logpattern.Shift`/`Template`, `correlate.ScoredChange`/`ScoredService`, `core.Series`/`Change`/`LogLine`) anywhere inside `Evidence.data`.
- `lib/markdown.ts`: client-side Markdown rendering of a report.

## Checks

```sh
npm run typecheck && npm run lint && npm test && npm run build
```
