# Culprit report: inc-42

**Status:** completed

## Summary

Catalog 1.8.2 added an N+1 query that saturated the DB pool; frontend p99 rose.

## Hypotheses

### 1. N+1 query in catalog 1.8.2 (82%)

**Service:** catalog · **Category:** bad-deploy · **Change:** `deploy-catalog-1.8.2` · **Evidence:** E1, E2, E3

Pool saturation began two minutes after the deploy.

Next steps:

- Roll back catalog to 1.8.1
- Check slow query log

### 2. Cart TTL change (5%)

**Service:** cart · **Evidence:** E4

Unlikely: cart metrics are flat.

## Timeline

- `2026-10-20T14:02:00Z` catalog 1.8.2 deployed (E3)
- `2026-10-20T14:05:00Z` frontend p99 rises (E1)

## Evidence

| ID | Kind | Summary |
|---|---|---|
| E1 | anomaly | http_request_duration_seconds:p99{service="frontend"} up 0.2 -> 1.9 |
| E2 | log_patterns | catalog: NEW [error] pool \| exhausted waiting <*>ms |
| E3 | change | deploy-catalog-1.8.2 deploy catalog xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx… |
| E4 | metric | cache_hit:ratio flat |

_model test-model · 5 steps · 6 tool calls · 23,456 prompt + 1,234 completion tokens · 42s_
