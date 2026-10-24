# Gateway latency and 503s

**Status:** completed

## Summary

Payments began returning 5xx errors at about 20:23, right after the payments 5.1.3 rolling restart (TLS CA bundle 41→42). Orders' payments client had been changed at 19:40 to 6 retries with zero backoff, so every failed charge was retried immediately, multiplying load on payments (request rate ~13→62/min) and driving payments into load shedding and CPU saturation. Errors propagated to orders (503/504s) and then to the gateway, which surfaced latency and 503s from 20:30 onward.

## Hypotheses

### 1. Payments 5.1.3 rolling restart (new TLS CA bundle) introduced failures in payments, triggering the outage (60%)

**Service:** payments · **Category:** bad-deploy · **Change:** `deploy-payments-5.1.3` · **Evidence:** E1, E11, E18, E24, E21, E17

Payments http_errors:ratio1m jumped from ~0.003 to ~0.23 starting 20:23:15, matching the deploy-payments-5.1.3 time (20:23:00) and the first payments error log at 20:22:43. The only diff is caBundleVersion 41→42, so the TLS bundle change during the rolling restart is the most likely trigger. Payments is the earliest onset in the topology, and orders and gateway errors follow it. Note: the exact failure mode (handshake/fx-api TLS vs. other) is not directly confirmed by logs; no 'tls' log lines were found.

Next steps:

- Verify payments TLS handshake/cert errors against fx-api and the mTLS/CA bundle 42 in the pod logs (read-only).
- Check whether pods on 5.1.3 are failing while old pods on 5.1.2 succeed; consider rollback of caBundleVersion to 41 (human decision).

### 2. Orders retry config (6 retries, 0 backoff) amplified payments failures into a retry storm (35%)

**Service:** orders · **Category:** config-change · **Change:** `cfg-orders-payments-retries` · **Evidence:** E9, E10, E20, E22, E5, E23

cfg-orders-payments-retries (19:40) changed orders' payments client from retries 1 / backoff 200ms to retries 6 / backoff 0s. Once payments began failing at 20:23, orders logged ~216 'retrying immediately' warnings (attempt up to 2/6 and beyond), and payments request volume rose from ~13/min to ~62/min with in-flight limit load shedding (x37 warn increase) and CPU pinned at 1.0. This amplification made the payments outage severe and kept it going, and orders' own p99 and errors rose after. The orders request rate itself did not increase (E23), so the extra load is retry fan-out rather than user traffic. The config change predates the incident by 40 minutes, so it is not the initial trigger.

Next steps:

- Revert orders payments client to retries 1 / backoff 200ms (or add jittered exponential backoff and a retry budget) to stop the retry amplification.
- Confirm payments request-rate decline after the retry change is reverted.

### 3. Unrelated changes ruled out: auth dependency bump, notifications SMTP pool, orders 3.8.7 deploy (5%)

**Service:** gateway · **Category:** other · **Evidence:** E14, E15, E16, E12

auth 5.3.8 (19:05) and notifications SMTP config changes (19:40, 19:53) have no matching anomaly onset or log shift in the affected path; notifications is not connected to gateway/orders/payments. orders 3.8.7 (19:49) is a response-format change with a low correlation score and its errors start after payments failures, so it is a downstream effect at most.

Next steps:

- No action needed for these changes unless new evidence appears.

## Timeline

- `2026-11-10T19:40:00Z` orders config cfg-orders-payments-retries changes payments client to retries 6, backoff 0s. (E20)
- `2026-11-10T20:22:43Z` First payments error log line (5xx on charges endpoints). (E21)
- `2026-11-10T20:23:00Z` deploy-payments-5.1.3 rolling restart begins (TLS CA bundle 41→42). (E11, E18)
- `2026-11-10T20:23:15Z` payments http_errors:ratio1m rises (0.003→0.23); orders and gateway error ratios also rise at the same onset. (E1, E4, E8, E24)
- `2026-11-10T20:24:15Z` payments request rate climbs (13→62/min) and CPU saturates, as orders retries fan out. (E5, E3)
- `2026-11-10T20:24:16Z` orders begins logging payments 503 'retrying immediately' warnings. (E22)
- `2026-11-10T20:25:30Z` orders and gateway p99 latency rises (to ~1.5–1.7s). (E2, E6, E7)
- `2026-11-10T20:30:00Z` Incident declared; orders retry warnings jump 24x, payments load shedding starts. (E9, E10)

## Evidence

| ID | Kind | Summary |
|---|---|---|
| E1 | anomaly | http_errors:ratio1m{env="prod",service="payments"} up: baseline 0.00286 -> peak 0.3802, score 95.16, onset 2026-11-10T20:23:15Z (-6m45s) |
| E2 | anomaly | http_request_duration_seconds:p99{env="prod",service="orders"} up: baseline 0.2253 -> peak 1.684, score 83.98, onset 2026-11-10T20:25:30Z (-4m30s) |
| E3 | anomaly | process_cpu:ratio{env="prod",service="payments"} up: baseline 0.2278 -> peak 1, score 69.86, onset 2026-11-10T20:24:15Z (-5m45s) |
| E4 | anomaly | http_errors:ratio1m{env="prod",service="orders"} up: baseline 0.00152 -> peak 0.234, score 65.99, onset 2026-11-10T20:23:15Z (-6m45s) |
| E5 | anomaly | http_requests:rate1m{env="prod",service="payments"} up: baseline 13.62 -> peak 62.23, score 65.77, onset 2026-11-10T20:24:15Z (-5m45s) |
| E6 | anomaly | http_request_duration_seconds:p99{env="prod",service="gateway"} up: baseline 0.3065 -> peak 1.541, score 46.06, onset 2026-11-10T20:25:30Z (-4m30s) |
| E7 | anomaly | http_request_duration_seconds:p99{env="prod",service="payments"} up: baseline 0.4597 -> peak 2.147, score 45.16, onset 2026-11-10T20:25:30Z (-4m30s) |
| E8 | anomaly | http_errors:ratio1m{env="prod",service="gateway"} up: baseline 0.0019 -> peak 0.1031, score 27.03, onset 2026-11-10T20:23:15Z (-6m45s) |
| E9 | log_patterns | orders: x24.63 [warn] 2.195/min -> 54.04/min first 2026-11-10T20:30:00Z (+0s): payments POST http://payments:<*>/charges failed (attempt <*>): <*> Service Unavailable, retrying immediately |
| E10 | log_patterns | payments: x37.07 [warn] 0.501/min -> 18.57/min first 2026-11-10T20:30:01Z (+1s): load shedding: <*> in-flight requests > limit <*>, rejecting with <*> \| x18.9 [info] 0.6263/min -> 11.84/min first 202… |
| E11 | change | deploy-payments-5.1.3 deploy payments at 2026-11-10T20:23:00Z (-7m0s) score 0.99: payments 5.1.3: rolling restart to pick up new TLS bundle (by k.mensah) [deployed just before payments http_errors:ra… |
| E12 | change | deploy-orders-3.8.7 deploy orders at 2026-11-10T19:49:37Z (-40m23s) score 0.25: orders 3.8.7: include currency in order summary response (by deploy-bot) [deployed 34m before orders http_errors:ratio1… |
| E13 | change | cfg-orders-payments-retries config orders at 2026-11-10T19:40:00Z (-50m0s) score 0.18: orders: make payments client more resilient to transient errors (by m.brandt) [config changed 43m before orders … |
| E14 | change | deploy-auth-5.3.8 deploy auth at 2026-11-10T19:05:49Z (-1h24m11s) score 0.05: auth 5.3.8: bump golang.org/x/crypto to v0.41.0 (by j.okafor) [deployed 1h17m before gateway http_errors:ratio1m rose; au… |
| E15 | change | cfg-notifications-c972 config notifications at 2026-11-10T19:53:48Z (-36m12s) score 0.04: notifications: raise smtp pool size 8 -> 12 (by a.novak) [config changed 29m before gateway http_errors:ratio… |
| E16 | change | cfg-notifications-eaf1 config notifications at 2026-11-10T19:40:19Z (-49m41s) score 0.03: notifications: raise smtp pool size 8 -> 12 (by maria.k) [config changed 43m before gateway http_errors:ratio… |
| E17 | suspects | Suspect services: 1. payments score 1.00 onset 2026-11-10T20:23:15Z (-6m45s) (earliest onset among 3 affected services (2026-11-10T20:23:15Z: payments http_errors:ratio1m rose); http_errors:ratio1m r… |
| E18 | change | Change deploy-payments-5.1.3 (deploy) service=payments at 2026-11-10T20:23:00Z (-7m0s) Author: k.mensah URL: https://deploy.example.internal/payments/releases/5.1.3 Summary: payments 5.1.3: rolling r… |
| E19 | log_patterns | payments: 3056 lines scanned; baseline = before 2026-11-10T20:30:00Z. - x37.07 [warn] 0.501/min -> 18.57/min first 2026-11-10T20:30:01Z (+1s): load shedding: <*> in-flight requests > limit <*>, rejec… |
| E20 | change | Change cfg-orders-payments-retries (config) service=orders at 2026-11-10T19:40:00Z (-50m0s) Author: m.brandt URL: https://git.example.internal/shop/infra/changes/cfg-orders-payments-retries Summary: … |
| E21 | logs | 210 lines from payments, 1 templates: - x210 [error] method=POST <*> status=<*> duration_ms=<*> (first 2026-11-10T20:22:43Z (-7m16s), last 2026-11-10T20:59:56Z (+29m56s)) Samples: 2026-11-10T20:57:55… |
| E22 | logs | 300 lines from orders (limit reached; results may be partial), 4 templates: - x216 [warn] payments POST http://payments:<*>/charges failed (attempt <*>): <*> Service Unavailable, retrying immediately… |
| E23 | metric | 1 series for http_requests:rate1m{service="orders"} (incident start 2026-11-10T20:30:00Z): - http_requests:rate1m{env="prod",service="orders"}: before mean 34.02 p95 37.64 \| after mean 30.82 max 35.1… |
| E24 | metric | 1 series for http_errors:ratio1m{service="payments"} (incident start 2026-11-10T20:30:00Z): - http_errors:ratio1m{env="prod",service="payments"}: before mean 0.09025 p95 0.3174 \| after mean 0.2273 ma… |

_model anthropic/claude-haiku-5.5 · 4 steps · 9 tool calls · 27,512 prompt + 3,850 completion tokens · 20s_
