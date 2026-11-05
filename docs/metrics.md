# Metrics catalog

Uptime Monitor exposes Prometheus metrics on its ops ports, which are never
published outside the host network:

| Process  | Endpoint                     | What it exposes                                         |
|----------|------------------------------|---------------------------------------------------------|
| `api`    | `OPS_ADDR` (`:9090`) `/metrics` | HTTP metrics, connection pool, build info, `go_*`, `process_*` |
| `worker` | `OPS_ADDR` (`:9091`) `/metrics` | checks, scheduler, notifications, system gauges, connection pool, build info, `go_*`, `process_*` |

The public port (`:8080`) never serves `/metrics`.

**The names, types and labels below are a public contract**: dashboards,
recording rules and alerts in `uptime-infra` depend on them. Renaming a
metric or a label, or changing its unit, is a breaking change: add the new
one, migrate `uptime-infra`, then remove the old one in a later release.
Adding metrics or label values is safe.

## Cardinality rules

- Labels take values from **closed lists only**. Never `monitor_id`,
  `user_id`, `url`, `request_id`, an email or any other user input. A
  per-monitor metric would create a series for every monitor of every user;
  per-monitor data belongs in the API (`/monitors/{id}/stats`), not in
  Prometheus.
- `route` is the chi route template (`/api/v1/monitors/{id}`), or
  `unmatched` when no route matched (including unknown paths under
  `/api/v1/`). `method` is a standard HTTP method or `other`.
- Label values are created up front where the list is known
  (`result`/`error_kind`, `channel_type`/`status`, `state`), so a value that
  has not happened yet reads `0` instead of being absent. HTTP series appear
  with the first request of a route.
- Enforced by tests: `internal/httpapi/metrics_test.go` (route templates and
  methods only, no IDs in the exposition) and
  `internal/observability/observability_test.go` (every metric of this
  catalog is present, `promlint` finds no problems, and
  `TestCatalogMatchesDocs` compares the tables of this file with the
  exposed metrics: names, types and label names).

## Catalog

### HTTP (api)

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_http_requests_total` | counter | `method`, `route`, `code` | Requests handled, by final status code. `/healthz` and `/readyz` are not counted. A panic counts as `500`. | **Availability SLI** of the API: share of non-5xx; 5xx rate alert. |
| `uptime_http_request_duration_seconds` | histogram | `method`, `route` | Time from the first middleware to the end of the handler. Buckets: 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5. | **Latency SLI** (e.g. share of requests under 250 ms); p95/p99 panels. |
| `uptime_http_requests_in_flight` | gauge | — | Requests being handled right now (probes included). | Load and saturation of an instance. |

### Checks and scheduler (worker)

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_checks_total` | counter | `result` (`success`/`failure`), `error_kind` (`none` for success; `dns`, `timeout`, `connection`, `tls`, `unexpected_status`, `keyword_mismatch`, `blocked_target`, `unknown`) | Checks performed. A check counts even if its result could not be saved. | Throughput; failure mix. A jump in `dns`/`connection` across all targets points at the worker's own network, not at the targets. |
| `uptime_check_duration_seconds` | histogram | — | Time to check one target (DNS, connect, TLS, response, body). Buckets: 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30. | Capacity: long checks occupy pool slots. |
| `uptime_scheduler_lag_seconds` | histogram | — | Delay between when a check was due (`next_check_at`) and when it started. Buckets: 0.5, 1, 2, 5, 10, 30, 60. | **Timeliness SLI**: share of checks started within N s of schedule. |
| `uptime_scheduler_due_monitors` | gauge | — | Active monitors whose check is due or overdue (refreshed every 15 s). | Backlog: grows when the workers cannot keep up. |
| `uptime_worker_pool_busy` | gauge | — | Checks running right now in this worker. | Saturation: `busy / size` close to 1 for long → add workers or concurrency. |
| `uptime_worker_pool_size` | gauge | — | `WORKER_CONCURRENCY` of this worker. | Denominator of saturation. |
| `uptime_incidents_opened_total` | counter | — | Incidents opened. | Incident flow; a spike across many monitors suggests a problem on our side. |
| `uptime_incidents_open` | gauge | — | Incidents not yet resolved (refreshed every 15 s). | Dashboard. |
| `uptime_monitors` | gauge | `state` (`active`/`paused`) | Monitors (refreshed every 15 s). | Size of the system; capacity planning. |
| `uptime_monitors_cert_expiring` | gauge | — | Active monitors whose last seen certificate expires within their `cert_expiry_warn_days` (refreshed every 15 s). | Dashboard. |

### Notifications (worker)

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_notifications_total` | counter | `channel_type` (`telegram`/`webhook`), `status` (`sent`/`retry`/`failed`) | Outcome of each delivery attempt. `retry`: failed, will be tried again; `failed`: given up (5 attempts or a permanent error). | Delivery success ratio; alert on `failed` rate. |
| `uptime_notification_delivery_delay_seconds` | histogram | `channel_type` | `sent_at − created_at` of delivered notifications, retries included. Buckets: 1, 5, 15, 30, 60, 120, 300, 900. | **Delivery SLI**: share delivered within N s. |
| `uptime_notifications_pending` | gauge | — | Outbox rows not yet sent or failed, including those waiting for a retry (refreshed every 15 s). | Outbox size. |
| `uptime_notification_oldest_pending_age_seconds` | gauge | — | Age of the oldest pending row; 0 when there is none (refreshed every 15 s). | Alert: notifications stuck (the notifier loop is down or every delivery fails). |

### Retention (worker)

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_retention_deleted_rows_total` | counter | — | Check results deleted (also by a run that failed halfway). | Work done by the cleanup. |
| `uptime_retention_last_success_timestamp_seconds` | gauge | — | Unix time of the last successful run; 0 before the first. Runs at startup and hourly. | Alert: `time() - … > 3h` → the table keeps growing. |

### Database connection pool (api and worker)

Read from `pgxpool.Stat()` at scrape time.

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_db_pool_acquired_connections` | gauge | — | Connections in use. | Saturation: close to max for long → raise `DB_MAX_CONNS`. |
| `uptime_db_pool_idle_connections` | gauge | — | Open connections not in use. | |
| `uptime_db_pool_total_connections` | gauge | — | Open connections (acquired, idle, being opened). | Connections this process holds on PostgreSQL. |
| `uptime_db_pool_max_connections` | gauge | — | Pool limit. | Denominator of saturation. |
| `uptime_db_pool_acquires_total` | counter | — | Connections taken from the pool. | |
| `uptime_db_pool_acquire_waits_total` | counter | — | Acquires that had to wait because no connection was free. | Pool saturation: should stay near 0. |
| `uptime_db_pool_acquire_wait_seconds_total` | counter | — | Time spent waiting for a connection, summed. | `rate(…)` = seconds of waiting per second. |

### Build (api and worker)

| Metric | Type | Labels | Meaning | Used for |
|---|---|---|---|---|
| `uptime_build_info` | gauge (always 1) | `version`, `commit`, `go_version` | Which build runs. | Rollout dashboards; join to other series (`* on(instance) group_left(version) uptime_build_info`). |

Plus the standard `go_*` (goroutines, GC, memory) and `process_*` (CPU, RSS,
open file descriptors) metrics.

## Several worker replicas

Counters, histograms and the pool gauges are per process: sum them over
instances. The gauges refreshed from the database (`scheduler_due_monitors`,
`notifications_pending`, `notification_oldest_pending_age_seconds`,
`incidents_open`, `monitors`, `monitors_cert_expiring`) describe the whole
system, so every replica reports the same value: aggregate them with `max`,
never `sum`. A failed refresh keeps the last value and is logged
(`refresh metrics snapshot`).

## Example queries

```promql
# API availability SLI over 30 days (share of requests without a 5xx)
1 - sum(increase(uptime_http_requests_total{code=~"5.."}[30d]))
  / sum(increase(uptime_http_requests_total[30d]))

# API latency: share of requests faster than 250 ms
sum(rate(uptime_http_request_duration_seconds_bucket{le="0.25"}[5m]))
  / sum(rate(uptime_http_request_duration_seconds_count[5m]))

# Timeliness SLI: share of checks started within 5 s of schedule
sum(rate(uptime_scheduler_lag_seconds_bucket{le="5"}[5m]))
  / sum(rate(uptime_scheduler_lag_seconds_count[5m]))

# Delivery SLI: share of notifications delivered within 60 s
sum(rate(uptime_notification_delivery_delay_seconds_bucket{le="60"}[1h]))
  / sum(rate(uptime_notification_delivery_delay_seconds_count[1h]))

# Worker pool saturation
sum(uptime_worker_pool_busy) / sum(uptime_worker_pool_size)

# Notifications stuck
max(uptime_notification_oldest_pending_age_seconds) > 900
```
