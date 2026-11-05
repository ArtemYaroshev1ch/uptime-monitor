# Uptime Monitor

[![CI](https://github.com/artemyaroshev1ch/uptime-monitor/actions/workflows/ci.yml/badge.svg)](https://github.com/artemyaroshev1ch/uptime-monitor/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/artemyaroshev1ch/uptime-monitor)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Self-hosted uptime monitoring for websites with HTTP/HTTPS diagnostics,
incidents and notifications, built to be operated: metrics, health probes,
structured logs and graceful shutdown.

> **Status: early development (Phase 6, notifications).** Users, monitors,
> scheduled HTTP/HTTPS checks with phase timings, history, statistics,
> incidents, Telegram and webhook notifications and certificate expiry
> warnings work. Metrics come next. Items marked *planned* are
> not implemented yet; see [Roadmap](#roadmap).

## Features

- Registration and login with email and password (bcrypt); login returns a
  JWT access token. Auth endpoints are rate-limited per client IP.
- Monitors: create, list (paginated), read, partially update (including
  pause/resume) and delete. URLs are validated against SSRF targets; each user
  sees only their own monitors and has a configurable limit.
- HTTP/HTTPS checks by a separate `worker`: DNS, connect, TLS and
  time-to-first-byte timings, status code, HTTP version, TLS version and
  certificate expiry, keyword and expected-status rules, up to 5 redirects.
  Failures are classified (`dns`, `timeout`, `connection`, `tls`,
  `unexpected_status`, `keyword_mismatch`, `blocked_target`, `unknown`).
- Check history and statistics (uptime %, average and p95 latency, p95 TTFB
  over 24 h / 7 d / 30 d), computed in SQL. Old checks are deleted after
  `CHECKS_RETENTION_DAYS`.
- Several workers can run side by side: monitors are claimed with
  `FOR UPDATE SKIP LOCKED`, so no monitor is checked twice.
- Incidents: `failure_threshold` failed checks in a row open one, the first
  successful check resolves it. Never more than one open incident per
  monitor, even with concurrent results. The start time is the first failure
  of the streak, so the duration is the real downtime.
- Notifications to Telegram and signed webhooks (HMAC-SHA256) when an
  incident opens or resolves and when a TLS certificate is about to expire
  (`cert_expiry_warn_days`, once per certificate). Delivered through a
  transactional outbox with retries and exponential backoff; a test endpoint
  checks a channel on demand.
- One error format for every endpoint, with per-field details for invalid
  input. Every response carries an `X-Request-ID` that also appears in logs.
- Liveness `GET /healthz`, readiness `GET /readyz` (checks PostgreSQL) and
  build information `GET /version`.
- PostgreSQL schema migrations embedded in a separate `migrate` binary.
- JSON logs in production, text logs in development.
- Graceful shutdown on `SIGTERM`/Ctrl+C.
- `-healthcheck` flag for container health checks without curl.
- Prometheus metrics on internal ops ports (api `:9090`, worker `:9091`):
  RED metrics of the API, check, scheduler and delivery SLIs, outbox and
  connection pool state; catalog in [docs/metrics.md](docs/metrics.md).
  Optional `pprof` on the API ops port.

## Quick start

Requirements: Go 1.27+, GNU Make, a POSIX shell (on Windows: Git for Windows,
see [Windows](#windows)) and Docker with Compose v2.

```sh
cp .env.example .env
# set JWT_SECRET in .env, e.g. to the output of: openssl rand -base64 48
make up           # PostgreSQL in Docker, waits until healthy
make migrate-up   # apply migrations
make run-api      # terminal 1
make run-worker   # terminal 2: checks monitors
```

In another terminal:

```sh
curl http://localhost:8080/healthz
# {"status":"ok"}
curl http://localhost:8080/readyz
# {"status":"ok","checks":{"database":"ok"}}
curl http://localhost:8080/version
# {"version":"0.0.0-dev","commit":"...","built_at":"..."}

curl -X POST http://localhost:8080/api/v1/auth/register \
  -d '{"email":"alice@example.com","password":"correct-horse"}'
# {"id":"...","email":"alice@example.com","created_at":"..."}
curl -X POST http://localhost:8080/api/v1/auth/login \
  -d '{"email":"alice@example.com","password":"correct-horse"}'
# {"access_token":"eyJ...","token_type":"Bearer","expires_at":"..."}
curl http://localhost:8080/api/v1/me -H "Authorization: Bearer eyJ..."
# {"id":"...","email":"alice@example.com","created_at":"..."}

TOKEN=eyJ...
curl -X POST http://localhost:8080/api/v1/monitors -H "Authorization: Bearer $TOKEN" \
  -d '{"name":"Example","url":"https://example.com","interval_seconds":60}'
# {"id":"...","name":"Example","url":"https://example.com","method":"GET",
#  "interval_seconds":60,"timeout_seconds":10,"is_active":true,"status":"unknown",...}
curl "http://localhost:8080/api/v1/monitors?limit=50&offset=0" -H "Authorization: Bearer $TOKEN"
# {"items":[...],"total":1}
curl -X PATCH http://localhost:8080/api/v1/monitors/<id> -H "Authorization: Bearer $TOKEN" \
  -d '{"is_active":false,"keyword":null}'   # pause; null clears an optional field
curl "http://localhost:8080/api/v1/monitors/<id>/checks?limit=1" -H "Authorization: Bearer $TOKEN"
# {"items":[{"checked_at":"...","success":true,"status_code":200,"response_time_ms":420,
#   "dns_ms":12,"connect_ms":95,"tls_ms":110,"ttfb_ms":88,"http_proto":"HTTP/2.0",
#   "tls_version":"TLS1.3","tls_not_after":"...","error_kind":null,...}],"total":1,...}
curl "http://localhost:8080/api/v1/monitors/<id>/stats?period=7d" -H "Authorization: Bearer $TOKEN"
# {"period":"7d","total_checks":20160,"failed_checks":3,"uptime_percent":99.985,
#  "avg_response_ms":431.2,"p95_response_ms":610,"p95_ttfb_ms":140,...}
curl "http://localhost:8080/api/v1/incidents?open=true" -H "Authorization: Bearer $TOKEN"
# {"items":[{"id":"...","monitor_id":"...","monitor_name":"Example",
#   "started_at":"...","resolved_at":null,"cause_kind":"timeout","duration_seconds":null}],"total":1}
# Notifications: a webhook channel (the secret is shown only now), then a test.
curl -X POST http://localhost:8080/api/v1/channels -H "Authorization: Bearer $TOKEN" \
  -d '{"type":"webhook","name":"ops","config":{"url":"https://hooks.example.com/uptime"}}'
# {"id":"...","type":"webhook","name":"ops","config":{"url":"...","secret":"K7QJ..."},...}
curl -X POST http://localhost:8080/api/v1/channels/<id>/test -H "Authorization: Bearer $TOKEN"
# {"delivered":true}
curl -X DELETE http://localhost:8080/api/v1/monitors/<id> -H "Authorization: Bearer $TOKEN"

# Metrics, on the ops ports only (make run-* binds them to 127.0.0.1)
curl -s http://127.0.0.1:9090/metrics | grep '^uptime_http_requests_total'
# uptime_http_requests_total{code="201",method="POST",route="/api/v1/monitors"} 1 ...
curl -s http://127.0.0.1:9091/metrics | grep '^uptime_checks_total'
# uptime_checks_total{error_kind="none",result="success"} 12 ...
```

Errors look the same everywhere:

```json
{"error": {"code": "validation_failed", "message": "Invalid request",
           "details": [{"field": "password", "issue": "must be at least 8 characters"}]}}
```

Codes: `validation_failed` (400), `unauthorized` (401), `not_found` (404),
`conflict` (409), `rate_limited` (429, with `Retry-After`), `internal` (500).
The API is described in [api/openapi.yaml](api/openapi.yaml).

*Planned:* the whole system with `docker compose up --build`.

## Configuration

All configuration comes from environment variables. `make run-api` and
`make run-worker` also read `.env` if it exists.

| Variable                | Description                                             | Default |
|-------------------------|---------------------------------------------------------|---------|
| `APP_ENV`               | `dev` (text logs) or `prod` (JSON logs)                 | `dev`   |
| `LOG_LEVEL`             | `debug`, `info`, `warn` or `error`                      | `info`  |
| `HTTP_ADDR`             | public REST API listen address                          | `:8080` |
| `DATABASE_URL`          | PostgreSQL URL (**required**, contains password)        | —       |
| `DB_MAX_CONNS`          | connection pool size of one `api` process               | `10`    |
| `SHUTDOWN_TIMEOUT`      | time for in-flight requests to finish on SIGTERM        | `25s`   |
| `JWT_SECRET`            | token signing key, ≥ 32 bytes (**required**, secret)    | —       |
| `ACCESS_TOKEN_TTL`      | access token lifetime                                   | `12h`   |
| `TRUST_PROXY_HEADERS`   | rate-limit by `X-Forwarded-For` (behind a proxy only)   | `false` |
| `MAX_MONITORS_PER_USER` | monitors one user may have                              | `20`    |
| `ALLOW_PRIVATE_TARGETS` | allow private/loopback monitor URLs (**dev only**)      | `false` |
| `OPS_ADDR`              | ops port, internal only (see [Operations](#operations)) | api `:9090`, worker `:9091` |
| `PPROF_ENABLED`         | serve `/debug/pprof/` on the api ops port               | `false` |
| `WORKER_CONCURRENCY`    | checks running at once in one worker                    | `20`    |
| `SCHEDULER_BATCH_SIZE`  | monitors claimed per scheduler tick (1 s)               | `50`    |
| `CHECKS_RETENTION_DAYS` | days check results are kept                             | `30`    |
| `TELEGRAM_BOT_TOKEN`    | Telegram bot token (secret); empty disables Telegram    | empty   |

`api` uses the variables up to `PPROF_ENABLED` and `TELEGRAM_BOT_TOKEN`
(for test notifications); `worker` uses the common ones (`APP_ENV`,
`LOG_LEVEL`, `DATABASE_URL`, `DB_MAX_CONNS`, `SHUTDOWN_TIMEOUT`),
`ALLOW_PRIVATE_TARGETS`, `OPS_ADDR`, the worker settings and
`TELEGRAM_BOT_TOKEN`. Both processes read `OPS_ADDR` with different
defaults; `make run-api` and `make run-worker` set it to `127.0.0.1:9090`
and `127.0.0.1:9091` (override with `API_OPS_ADDR=...` / `WORKER_OPS_ADDR=...`
on the make command line).

Invalid values stop the process at startup with an error that lists every
problem.

## Operations

| Endpoint / flag          | Purpose                                                              |
|--------------------------|----------------------------------------------------------------------|
| api `GET /healthz`       | liveness: the process serves HTTP; does not check dependencies       |
| api `GET /readyz`        | readiness: 200 if PostgreSQL answers within 2 s, otherwise 503       |
| api `GET /version`       | `{"version","commit","built_at"}` of the running build               |
| api `:9090/metrics`      | Prometheus metrics of the API ([catalog](docs/metrics.md))           |
| api `:9090/debug/pprof/` | Go profiler, only with `PPROF_ENABLED=true`                          |
| worker `:9091/healthz`   | liveness of the worker                                               |
| worker `:9091/readyz`    | readiness: PostgreSQL answers and the scheduler claimed work < 30 s ago |
| worker `:9091/metrics`   | Prometheus metrics of the worker ([catalog](docs/metrics.md))        |
| `api -healthcheck`       | probes `/healthz` on `HTTP_ADDR`, exit code 0 = healthy, 1 = not     |
| `worker -healthcheck`    | probes `/healthz` on `OPS_ADDR`, exit code 0 = healthy, 1 = not      |
| `api -version`, `worker -version` | print version, commit, build time and Go version            |
| `migrate up`       | applies all pending migrations; run before starting a new version |
| `migrate down`     | rolls back exactly one migration                                  |
| `migrate status`   | lists migrations and whether they are applied                     |

- **Logs:** one record per line on stdout with `time`, `level`, `msg`,
  `service` and `version`; records of a request also carry `request_id`
  (returned to the client as `X-Request-ID`) and, once authenticated,
  `user_id`. Each request gets one `http request` access-log record; probe
  requests are logged at debug level. The worker logs `incident opened`
  (warn) and `incident resolved` (info) with `monitor_id`, `incident_id`,
  `cause_kind` and `duration_seconds`; every check at debug level.
  Each delivery is logged as `notification sent` (info), `notification will
  be retried` or `notification failed` (warn) with `notification_id`,
  `event`, `channel_type`, `attempt`, `monitor_id` and `incident_id`;
  `certificate warnings queued` (info) when certwatch finds expiring
  certificates.
- **Notifications:** an incident change and its notifications (one per
  active channel of the owner) are written in one transaction to the
  `notifications` outbox. Every worker delivers due rows (`SKIP LOCKED`, a
  1-minute lease), at least once: a worker that dies mid-delivery leaves the
  row to be retried after the lease. Failures are retried after 30 s, 1 m,
  2 m and 4 m; after 5 attempts, or at once for a permanent error (most
  4xx, blocked address, disabled channel, Telegram not configured), the row
  becomes `failed` with `last_error`. To see stuck or failed deliveries:
  `SELECT status, count(*), min(created_at) FROM notifications GROUP BY 1;`
- **Shutdown:** on `SIGTERM` the API stops accepting connections and waits up
  to `SHUTDOWN_TIMEOUT` for in-flight requests. The worker stops claiming
  monitors and waits up to `SHUTDOWN_TIMEOUT` for running checks to finish
  and save their results, then aborts the rest.
- **Scaling the worker:** run as many as you like; each claims due monitors
  with `FOR UPDATE SKIP LOCKED` and only as many as it has free slots, so
  a monitor is never checked twice and a stopped worker leaves no claimed
  work behind (at most one interval of delay for the checks it was running).
- **Database outage:** `/readyz` returns 503 and the API keeps running; it
  serves again as soon as the database is back. `/healthz` stays 200.
- **Metrics:** the ops ports (`:9090` api, `:9091` worker) are internal:
  never publish them. The public port does not serve `/metrics`. Labels
  never carry IDs or URLs; per-monitor numbers are in the API
  (`/monitors/{id}/stats`). Database-backed gauges (backlog, outbox, open
  incidents, monitors) are refreshed by every worker every 15 s and are
  the same on all replicas: aggregate them with `max`. With
  `PPROF_ENABLED=true`: `go tool pprof http://127.0.0.1:9090/debug/pprof/profile?seconds=30`
  (CPU) or `.../debug/pprof/heap` (memory).
- *Planned:* the rollback procedure.

### Migrations

Migrations live in `migrations/NNNN_name.sql` (goose format) and are embedded
into the `migrate` binary. A PostgreSQL advisory lock makes concurrent
`migrate up` runs wait for each other.

```sh
make migrate-create name=add_monitors   # new empty migration
make migrate-up                         # apply all pending
make migrate-down                       # roll back one
make migrate-status                     # what is applied
```

Every migration must work with the **previous** release of the application,
because the old version keeps running while the new one is rolled out and
after a rollback (expand/contract):

- Allowed: new tables, new nullable columns or columns with a default, new
  indexes (`CREATE INDEX CONCURRENTLY` on large tables).
- In two releases: renaming or dropping a column, tightening a constraint.
  First release: add the new shape and write to both; next release, once the
  old code is gone: remove the old shape.
- `-- +goose Down` must undo `Up` exactly; the integration test applies all
  migrations, rolls them back and applies them again.

## Security

- Passwords: bcrypt with cost 12, 8 characters minimum, 72 bytes maximum
  (the bcrypt limit; longer input is rejected, not truncated).
- Login answers the same for an unknown email and a wrong password, and takes
  the same time: an unknown email still costs one bcrypt comparison.
- Access tokens: JWT signed with HS256 only (`alg` in the token is not
  trusted), with required expiry and issuer. `JWT_SECRET` shorter than 32
  bytes stops the API at startup.
- `/api/v1/auth/*`: 10 requests per minute per client IP, in memory per API
  instance. Behind a reverse proxy set `TRUST_PROXY_HEADERS=true`, otherwise
  all clients share the proxy's address.
- Request bodies are limited to 1 MiB; unknown JSON fields are rejected.
- Data isolation: every monitor and channel query filters by the owner.
  Another user's monitor or channel answers 404, exactly like a missing one.
- Notification secrets: the webhook secret is generated by the server
  (128 random bits) and returned only in the response that creates the
  channel; the Telegram bot token is removed from every error before it is
  logged or stored. Messages contain the monitor name, URL and incident
  data, never channel configuration.

### SSRF

The service fetches URLs chosen by its users, so without protection anyone
could make it request `http://169.254.169.254/` (cloud credentials),
`http://localhost:9090/metrics` or hosts in the internal network.

- **When a monitor is saved** the URL must be `http` or `https`, must not
  contain `user:password@`, and must not point to loopback, private
  (RFC 1918, IPv6 ULA), link-local (including `169.254.169.254`), CGNAT,
  multicast, unspecified or reserved addresses, `localhost`, or numeric host
  forms such as `2130706433` or `0x7f.1`.
- This check is syntactic: a hostname can resolve to an internal address,
  now or later (DNS rebinding). So the worker checks again **at connect
  time**: `net.Dialer.Control` sees the final IP of every connection,
  including each redirect hop, and refuses blocked addresses
  (`error_kind = blocked_target`). Redirect targets also pass the URL rules.
  The checker never uses `HTTP_PROXY`.
- **Webhook URLs** get the same treatment: the URL rules when the channel is
  saved and the connect-time address check on every delivery. Redirects are
  not followed, so a receiver cannot bounce a signed delivery elsewhere.
- `ALLOW_PRIVATE_TARGETS=true` disables the address rules (not the scheme and
  credential rules) so you can monitor local services during development.
  **Never enable it in production.** The API logs a warning at startup when it
  is on.

## Development

```sh
make help   # list all targets
make test   # unit tests (-race when cgo is available)
make test-integration  # plus tests against PostgreSQL in Docker (testcontainers)
make lint   # golangci-lint (installed into bin/tools on first run)
make fmt    # gofmt + goimports
make cover  # coverage report in coverage.html
```

```
cmd/api/                  API binary
cmd/migrate/              migration binary
internal/config/          environment configuration and validation
internal/httpapi/         router, handlers, JSON responses
internal/observability/   build information, Prometheus registry and metrics
internal/platform/        logger, HTTP server, self health check, PostgreSQL
migrations/               SQL migrations (embedded)
docker-compose.yml        local PostgreSQL
docs/decisions.md         design decisions
```

### Windows

The project builds natively on Windows without WSL:

- Install Git for Windows and GNU Make. `make` must find `sh.exe` in PATH
  (`C:\Program Files\Git\bin`); the Makefile stops with a clear error if it
  does not and adds Git's `usr\bin` tools to PATH itself.
- `.gitattributes` keeps every text file in LF regardless of `core.autocrlf`.
- The race detector needs a C compiler; without one, `make test` runs without
  `-race`. CI always runs with `-race`.
- Listening on `:8080` triggers a Windows Firewall prompt on first run. Set
  `HTTP_ADDR=127.0.0.1:8080` in `.env` to stay on loopback. `make run-*`
  already binds the ops ports to `127.0.0.1`.
- Docker Desktop must be running for `make up` and `make test-integration`.
- `DATABASE_URL` uses `127.0.0.1`, not `localhost`: `localhost` resolves to
  `::1` first, where the container does not listen, and each connection waits
  about 2 s before falling back to IPv4.

Background for these choices is in [docs/decisions.md](docs/decisions.md).

## Related

Deployment, monitoring stack, SLOs, alerts and runbooks live in the
`uptime-infra` repository (*planned*).

## Roadmap

- [x] Phase 0: skeleton, config, logging, `/healthz`, `/version`, CI
- [x] Phase 1: PostgreSQL, migrations, `/readyz`, integration tests
- [x] Phase 2: users and authentication
- [x] Phase 3: monitors CRUD
- [x] Phase 4: checks, scheduler, statistics
- [x] Phase 5: incidents
- [x] Phase 6: notifications and certificate warnings
- [x] Phase 7: observability
- [ ] Phase 8: Docker
- [ ] Phase 9: CI/CD
- [ ] Phase 10: documentation and v1.0.0

## License

[MIT](LICENSE)
