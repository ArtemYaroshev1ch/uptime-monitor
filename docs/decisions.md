# Decisions

Lightweight decision records: context → decision → alternatives.
Newest entries go at the bottom.

## 001. LF line endings for all text files

*2026-10-24, Phase 0*

**Context.** Development happens on native Windows, where Git for Windows sets
`core.autocrlf=true` by default and checks text files out with CRLF. Shell
scripts and make recipes break on CRLF (`$'\r': command not found`), gofmt
reports CRLF files as unformatted, and Docker/compose/`.env` parsers expect LF.

**Decision.** `.gitattributes` sets `* text=auto eol=lf`: the repository and
every working tree use LF, whatever `core.autocrlf` says. `Makefile`, `*.mk`
and `*.sh` are additionally forced to `text eol=lf`; `*.bat`/`*.cmd` use CRLF.

**Alternatives.** Ask every contributor to set `core.autocrlf=input`: easy to
forget and not enforced. LF only for Makefile and `*.sh`: Go files would still
get CRLF on Windows and fail formatting checks.

## 002. Makefile runs on native Windows through sh from Git for Windows

*2026-10-24, Phase 0*

**Context.** The project must build on Linux (CI), macOS and native Windows
without WSL. GNU Make for Windows uses `sh.exe` when it finds one in PATH and
silently falls back to `cmd.exe` otherwise. Even with sh, make starts simple
recipe lines (without shell metacharacters) directly, so tools such as `echo`,
`date`, `rm` or `awk` must be in PATH, and they live in Git's `usr\bin`, which
is not in PATH outside Git Bash.

**Decision.**
- Recipes are POSIX sh. The Makefile stops with a clear error if make uses
  `cmd.exe`, and on Windows prepends Git's `usr\bin` (found with `cygpath`) to
  PATH, so it works from PowerShell, cmd and Git Bash alike.
- Executable names use `go env GOEXE` (`.exe` on Windows).
- `run-api` builds `bin/api` and starts it instead of `go run`: the executable
  path stays the same, so Windows Firewall asks once, and Ctrl+C reaches the
  server process directly.
- `-race` needs cgo and a C compiler. It is enabled when `go env CGO_ENABLED`
  is `1`; on a Windows machine without gcc tests run without it, and CI
  (Linux) always runs them with `-race`.
- Scripts are invoked as `sh script.sh` or `bash script.sh` rather than relying
  on the executable bit, which Windows checkouts do not keep.

**Alternatives.** Require WSL: excluded by the environment. PowerShell scripts
or a Go-based task runner (mage, task): a second toolchain to learn, while
GNU Make is the de-facto standard in Go projects and CI.

## 003. golangci-lint is installed by `go install` into `bin/tools`

*2026-10-24, Phase 0*

**Context.** `make lint` must work on a fresh machine, and the linter has to
support the Go version from `go.mod` (here Go 1.27).

**Decision.** The Makefile pins `GOLANGCI_LINT_VERSION` and builds the linter
with the local Go toolchain into `bin/tools/golangci-lint-<version>`. The
version in the file name makes a version bump reinstall it. CI uses the
official `golangci-lint-action` with the same version for speed and caching.

**Alternatives.** Prebuilt release binaries via the install script: faster,
but it means piping a downloaded script into sh and handling OS/arch on
Windows. `go tool` (a `tool` directive in `go.mod`): adds the linter's large
dependency tree to the project's module graph and Dependabot updates.

## 004. Repository structure and configuration grow phase by phase

*2026-10-24, Phase 0*

**Context.** The target layout and the full list of environment variables are
known up front, but most packages and variables belong to later phases.

**Decision.** A directory, a Makefile target or a variable in `.env.example`
appears in the phase that implements it. Git does not track empty directories,
and placeholder packages or unused variables would document things that do
not exist yet.

**Alternatives.** Create the whole tree with `doc.go` stubs and every variable
on day one: the structure is visible earlier, but the repository describes
features that are not there.

## 005. Build metadata is linked into package main

*2026-10-24, Phase 0*

**Context.** `/version`, `-version` and `uptime_build_info` report version,
commit and build time.

**Decision.** Each binary declares `version`, `commit` and `builtAt` in package
`main`, set with `-ldflags "-X main.version=..."`; the same flags work for all
three binaries. The values are passed on as `observability.BuildInfo`, so no
other package reads globals. The version is SemVer without the `v` of the git
tag (`v1.2.0` → `1.2.0`, matching the image tag), `0.0.0-dev` before the first
tag. `-version` prints with `fmt` to stdout because it is command output, not
a log record.

**Alternatives.** Variables in an internal package: a long `-X` path for no
benefit. Only `debug.ReadBuildInfo()` VCS stamps: not available for `go run`
and Docker builds without `.git`.

## 006. Configuration parsing with caarlos0/env

*2026-10-24, Phase 0*

**Context.** Configuration comes only from environment variables and must be
validated at startup.

**Decision.** `caarlos0/env` parses typed fields (durations, `slog.Level`) and
defaults from struct tags; semantic checks live in `validate()` methods that
report all problems at once via `errors.Join`. Tests pass a map through
`env.Options.Environment` instead of changing the process environment. Parse
errors from the library name the struct field (`ShutdownTimeout`) rather than
the variable (`SHUTDOWN_TIMEOUT`); the rejected value is in the message, which
is enough to find the mistake.

**Alternatives.** Hand-written `os.Getenv` parsing: more code for every type.
Translating field names to variable names via reflection: extra code for a
small gain.

## 007. The API starts without a database; readiness reports it

*2026-10-30, Phase 1*

**Context.** The API needs PostgreSQL, but the database may be unavailable at
startup (it starts in parallel, a failover is in progress) or later.

**Decision.** The pgx pool is created without connecting (`MinConns = 0`), so
the process starts and serves `/healthz` regardless of the database.
`GET /readyz` pings the database with a 2 s timeout and answers 503 while it is
unreachable: the instance leaves the load balancer but is not restarted.
The response names the failed check without the error text (the port is
public); the error goes to the log at `warn`. `/healthz` never touches the
database (see `handler.Healthz`).

**Alternatives.** Ping at startup and exit on failure: during a database
outage every replica crash-loops, and restarts cannot fix the database.
Retry at startup until the database appears: the process is up but cannot
explain why it is not serving, which is what `/readyz` does.

## 008. Migrations: goose Provider, embedded SQL, advisory lock

*2026-10-30, Phase 1*

**Context.** Migrations run as a separate step (`/app/migrate up`) before a
new version starts. Two deploys may overlap, and the binary must not depend on
files next to it.

**Decision.**
- SQL files `migrations/NNNN_name.sql` are embedded with `embed`, so each
  binary carries exactly the schema of its commit.
- goose's `Provider` API instead of package-level functions: no global state
  (dialect, table name, logger), and every call takes a `context.Context`.
- A PostgreSQL session advisory lock (`lock.NewPostgresSessionLocker`)
  serializes concurrent runs; an integration test runs two `up`s at once.
- Commands: `up` (all pending), `down` (exactly one step, safe to repeat),
  `status`. Rolling back several steps is a deliberate repetition of `down`.
- Numbering is sequential (`0001`, `0002`, …), created by
  `make migrate-create name=...`; with one developer, timestamp prefixes would
  only make ordering harder to read.

**Alternatives.** goose CLI in the image: a second binary and its own config.
golang-migrate: similar features, but goose is the pinned choice (PROMPT.md §4).
Timestamp-based versions: avoid merge conflicts in big teams, not needed here.

## 009. Connection pool metrics come with the metrics registry (Phase 7)

*2026-10-30, Phase 1*

**Context.** Rule 8 says an external call is not finished without its metrics,
and Phase 1 adds the database. The plan (PROMPT.md §14) places the metrics
registry, the ops server (`:9090`) and the `pgxpool` collector in Phase 7.

**Decision.** Phase 1 adds no metrics. `uptime_db_pool_*` is implemented in
Phase 7 together with the registry and `/metrics`, where it can be scraped and
tested with `promlint`. Until then `/readyz` and the logs show the database
state.

**Alternatives.** A collector now without a `/metrics` endpoint: code that
cannot be observed and would be reworked when the registry appears. Pulling
the ops server forward into Phase 1: mixes two phases in one review.

## 010. Integration tests: testcontainers, build tag, one container per test

*2026-10-30, Phase 1*

**Context.** Repository code and migrations must be tested against a real
PostgreSQL, both locally (Windows with Docker Desktop) and in CI (Linux).

**Decision.** Tests that need Docker carry the build tag `integration` and run
with `make test-integration`; `make test` stays fast and Docker-free.
`pgtest.Start` starts an empty `postgres` container per test with the same
image tag as `docker-compose.yml`. golangci-lint runs with the tag, so this
code is linted too. CI has a separate `integration` job.

**Alternatives.** A shared container per package with a database per test:
faster, worth doing once the number of tests makes the start-up time (~3 s)
noticeable. An external database from compose: tests would depend on manual
setup and leftover state.

## 011. Access tokens: HS256 JWT via golang-jwt, no refresh tokens

*2026-10-31, Phase 2*

**Context.** The API needs stateless authentication that works with N API
replicas. The stack (PROMPT.md section 4) fixes `golang-jwt/jwt/v5`.

**Decision.** One HS256-signed access token per login, lifetime
`ACCESS_TOKEN_TTL` (12 h), claims `sub` (user ID), `iss`, `iat`, `exp`. The
parser accepts only HS256 and requires `exp`, so tokens with `alg: none`, an
other algorithm or no expiry are rejected. `JWT_SECRET` must be at least 32
bytes. A valid token of a deleted user gets 401, like any invalid token.

**Alternatives.** Hand-written HMAC JWT on the standard library: little code,
but parsing untrusted tokens is exactly where subtle bugs hide. Refresh tokens
with rotation and logout: deferred to the stretch list (PROMPT.md section 17);
until then a token cannot be revoked before it expires, short of rotating
`JWT_SECRET` (which logs everyone out).

## 012. Emails are stored lowercase

*2026-10-31, Phase 2*

**Context.** `users.email` is `UNIQUE`, but case-sensitive: `Alice@x.com` and
`alice@x.com` would be two accounts, and a user who typed a different case at
login would get "invalid credentials".

**Decision.** The service trims and lowercases emails before registration and
login. The unique index then enforces one account per address; a duplicate is
reported as 409 from the unique violation (no racy "SELECT first").

**Alternatives.** A `citext` column or a unique index on `lower(email)`: same
effect in the database, but needs a migration and still requires lowercasing
for lookups by index. Per RFC 5321 the local part is case-sensitive, but no
real provider treats it that way.

## 013. Auth rate limit: in memory, per client IP, optional proxy header

*2026-10-31, Phase 2*

**Context.** `/api/v1/auth/*` is limited to 10 requests per minute per IP.
The API has no shared store (no Redis), and in production it runs behind a
reverse proxy, where the TCP peer is the proxy.

**Decision.** A token bucket per IP (`golang.org/x/time/rate`, burst 10,
refill one per 6 s), shared by register and login. Buckets idle for a minute
are dropped inline, at most once a minute, so there is no cleanup goroutine.
The client IP is the TCP peer unless `TRUST_PROXY_HEADERS=true` (a variable
added to the PROMPT.md section 12 list); then it is the **last**
`X-Forwarded-For` entry, the one appended by our own proxy. Earlier entries
come from the client and are ignored.

**Known limitations.** Each API replica counts separately, so with N replicas
the effective limit is N x 10. Every IPv6 address is a separate client,
though one user may own a whole /64. Both are acceptable for brute-force
protection at this scale; a shared limiter (PostgreSQL or Redis) would fix
the first.

**Alternatives.** chi `middleware.RealIP`: trusts headers from anyone, so a
client could pick a new "IP" per request and bypass the limit.

## 014. Request decoding: unknown fields rejected, oversized body is a 400

*2026-10-31, Phase 2*

**Context.** The error codes are a closed list (PROMPT.md section 9); 413 is
not on it. The body limit is 1 MiB.

**Decision.** JSON bodies are decoded strictly: unknown fields, trailing data
and wrong types are `validation_failed` with the field in `details`. A body
over 1 MiB is also `validation_failed` (`{"field":"body","issue":"must be at
most 1048576 bytes"}`), status 400.

**Alternatives.** 413 with a new code `payload_too_large`: more precise HTTP,
but extends the public error contract for a case no legitimate client hits.
Ignoring unknown fields: a typo such as `"pasword"` would yield a confusing
"password is required".

## 015. Request ID from the client is reused if it is safe

*2026-10-31, Phase 2*

**Context.** Every log line of a request needs `request_id`, and services deep
in the call stack log with only a `context.Context`.

**Decision.** The request ID middleware reuses an incoming `X-Request-ID` (a
proxy may have set it, which links proxy and API logs) if it is at most 64
characters of `[A-Za-z0-9._-]`; otherwise it generates 128 random bits. The ID
is returned in `X-Request-ID` and stored in the context; a wrapping
`slog.Handler` (`logger.WithAttrs`) adds it, and `user_id` after
authentication, to every record logged with that context.

**Alternatives.** chi `middleware.RequestID`: does not reuse the incoming
header and does not reach slog. A per-request `*slog.Logger` in the context:
every function would have to fetch the logger from the context instead of
using its injected one.

## 016. Monitor limit: enforced in one transaction under a user row lock

*2026-10-31, Phase 3*

**Context.** `MAX_MONITORS_PER_USER` (20) must hold even when a client sends
several create requests at once. "Count, then insert" in the service races:
two requests of a user with 19 monitors both see 19 and both insert.

**Decision.** The repository runs `SELECT ... FROM users WHERE id = $1 FOR
UPDATE`, counts and inserts in one transaction. Concurrent creates of the same
user queue on the row lock; other users are not affected. An integration test
fires 10 parallel creates at a limit of 3 and expects exactly 3. A reached
limit is `409 conflict` with the message `Limit reached`: the request clashes
with the current state, and deleting a monitor resolves it.

**Alternatives.** `SERIALIZABLE` isolation: correct, but the losing
transaction fails with a serialization error that has to be retried. A
trigger: hides business rules in the database. 403 or 422: neither is in the
closed list of error codes.

## 017. PATCH semantics: absent keeps, null clears, create is a patch of defaults

*2026-10-31, Phase 3*

**Context.** `expected_status` and `keyword` are nullable: a client must be
able to clear them. With pointer fields `{}` and `{"keyword": null}` decode to
the same nil.

**Decision.** Input fields are `service.Optional[T]` (Set/Null/Value; its
`UnmarshalJSON` runs only for keys present in the body). `null` clears a
nullable field and is a validation error (`must not be null`) for the others.
Create applies the same patch to `domain.DefaultMonitorSettings()`, so create
and update share one code path and one set of rules (validator tags on
`domain.MonitorSettings`, mirroring the table's CHECK constraints). Update
loads the row `FOR UPDATE`, applies, validates and saves in one transaction,
so concurrent PATCHes of different fields are not lost. The URL policy runs
on create and when the URL changes, so a monitor created under a looser
policy can still be paused or renamed.

**Alternatives.** JSON Merge Patch (RFC 7396) on a generic map: the same
semantics, but untyped. PUT with the full object: clients would have to
resend every field to pause a monitor.

## 018. Rescheduling on update

*2026-10-31, Phase 3*

**Context.** `next_check_at` drives the scheduler (Phase 4). After an edit,
waiting out the old interval (up to 24 h) would be surprising.

**Decision.** A resumed monitor and one with a new URL are due at once
(`next_check_at = now`). A shortened interval moves the next check to at most
`now + interval`. Other edits keep the schedule; a paused monitor is not
rescheduled.

## 019. Syntactic SSRF rules for monitor URLs

*2026-10-31, Phase 3*

**Context.** PROMPT.md section 11 requires blocking internal targets at
connect time (Phase 4). Rejecting obviously internal URLs when a monitor is
saved gives the user an immediate, field-level error.

**Decision.** `checker.TargetPolicy.ValidateURL`: `http`/`https` only, at
most 2048 characters, no `user:password@` (it would be stored, returned and
logged in plain text), and no blocked IP literal (`checker.IsBlockedIP`,
shared with the Phase 4 dialer; IPv4-mapped IPv6 is unmapped first), no
`localhost`/`*.localhost`, no host whose last label is numeric (`2130706433`,
`0x7f.1`, `127.1`: inet_aton shorthands for IPs). `ALLOW_PRIVATE_TARGETS`
relaxes only the address rules. A malformed monitor ID in the path is `404`,
like an unknown or foreign one.

**Alternatives.** Resolving the hostname when saving: slower, and worthless
against DNS rebinding, which the connect-time check handles anyway.

## 020. Scheduler: claim only what fits in the pool, plan from now()

*2026-10-31, Phase 4*

**Context.** The query from PROMPT.md section 6 claims due monitors with
`FOR UPDATE SKIP LOCKED` and moves `next_check_at` ahead in the same
statement. Two details are open: how many to claim and from which time to
plan the next check.

**Decision.** Each tick (1 s) a worker claims `min(free pool slots,
SCHEDULER_BATCH_SIZE)` monitors. A claimed monitor starts at once, never
waits in an in-memory queue where it would be late (and lost if the process
died). With a full pool the tick skips the query and only refreshes the
heartbeat. `next_check_at = now() + interval`, not `scheduled_for +
interval`: after an outage or a backlog each monitor is checked once and
then returns to its rhythm, instead of firing once per missed interval. The
CTE returns `scheduled_for` (the old `next_check_at`) for the scheduler-lag
SLI. A check gets `SHUTDOWN_TIMEOUT` to finish after SIGTERM; its context is
detached from the shutdown signal (`context.WithoutCancel`) and cancelled
only when that time is up.

**Alternatives.** A claimed-but-unprocessed state with a lease: recovers
work from crashed workers sooner, but needs a column, a reaper and more SQL;
with a claim moving `next_check_at` by one interval, the worst case is one
missed interval for the monitors a crashed worker was checking.

## 021. Check timings: what exactly is measured

*2026-10-31, Phase 4*

**Context.** `dns_ms/connect_ms/tls_ms/ttfb_ms` must be comparable between
checks and meaningful with redirects.

**Decision.** Keep-alives are off, so every check opens a new connection.
DNS is resolved by the checker itself (`Resolver.LookupNetIP`, injectable
for tests) and timed around the lookup; connect, TLS and first byte come
from `httptrace`. `ttfb_ms` is from "request written" to "first response
byte": the server's think time, independent of the network setup before it.
With redirects, the phase timings describe the **final** request (reset on
each hop's `GetConn`); `response_time_ms` covers the whole chain including
reading up to 1 MiB of body. A phase that did not happen is `NULL`, not 0.

## 022. Monitor status and statistics rules

*2026-10-31, Phase 4*

**Decision.** After a check, in the same transaction as the insert:
success sets `status = up` and `consecutive_failures = 0`; a failure
increments `consecutive_failures` and sets `down` only once it reaches
`failure_threshold`, so one blip does not flip a monitor (incidents in
Phase 5 use the same threshold). `tls_not_after` keeps the last known value
when a check sees no certificate. Statistics count all checks for uptime,
but average and p95 latency only over successful checks: a 10 s timeout
says nothing about how fast the site answers, and would dominate the p95.
Figures are `null`, not 0, when there is nothing to aggregate.

## 023. Worker pool size and retention batches

*2026-10-31, Phase 4*

**Decision.** The worker's connection pool is at least
`WORKER_CONCURRENCY + 2` (one per running check to save its result, one for
claiming, one for retention), whatever `DB_MAX_CONNS` says; a smaller pool
would make finished checks queue for a connection and inflate the lag.
Retention deletes `checked_at < now - CHECKS_RETENTION_DAYS` in batches of
5000 rows per statement, hourly, in every worker: deletes are idempotent,
and short statements never hold long locks.

## 024. Metrics of the worker come with the registry (Phase 7)

*2026-10-31, Phase 4*

**Context.** CLAUDE.md rule 8 says a new loop or external call is not done
without its metrics. The registry, collectors and the `/metrics` endpoint
are Phase 7 (decision 009).

**Decision.** As in decision 009, the scheduler, pool, checker and retention
metrics (`uptime_checks_total`, `uptime_check_duration_seconds`,
`uptime_scheduler_lag_seconds`, `uptime_scheduler_due_monitors`,
`uptime_worker_pool_*`, `uptime_retention_*`) are added in Phase 7. The data
they need is already there: `scheduled_for` for the lag, `ErrorKind` as a
closed list for the label, `Scheduler.Heartbeat` for readiness. Until then
the worker logs each check at debug level and every claim or retention
error at error level.

## 025. Incidents: a pure policy, applied under the monitor row lock

*2026-10-31, Phase 5*

**Context.** A check result changes `consecutive_failures` and `status` and
may open or resolve an incident. Two results of the same monitor can arrive
at once (a slow check overlapping the next one, several workers), and there
must never be two open incidents.

**Decision.** The policy is one pure function, `domain.Evaluate(health,
success, threshold)`, tested table-driven at the threshold boundaries.
`Checks.SaveResult` runs it in the transaction that stores the check: it
first locks the monitor row (`SELECT ... FOR UPDATE`, including whether an
incident is open), then inserts the check, evaluates, updates the monitor
and inserts or resolves the incident. The lock serializes results per
monitor, so each sees the state the previous one left and no result is
lost. The partial unique index `uniq_open_incident_per_monitor` stays as the
last line of defense (`ON CONFLICT DO NOTHING`). An integration test fires
20 concurrent failures at threshold 1: one incident, 20 counted failures.

The repository calls a domain function inside its transaction instead of
the service owning a transaction: the transaction boundary is "one check
result", and keeping it in one place avoids a transaction abstraction
across layers.

**Alternatives.** Compute everything in one SQL statement: possible, but
the policy would be hidden in SQL and hard to test at its boundaries.
Rely on the unique index alone: two concurrent failures would both
increment from the same old value and one would be lost.

## 026. Incident times

*2026-10-31, Phase 5*

**Decision.** `started_at` is the time of the **first** failed check of the
streak (the oldest failure after the last success), not of the check that
reached `failure_threshold`: with threshold 3 and a 60 s interval the site
was already down two minutes earlier, and MTTR must include that.
`resolved_at` is the time of the first successful check. The API returns
`duration_seconds` for resolved incidents. A monitor that is paused while an
incident is open keeps it open until a check after resuming succeeds.

## 027. Notification outbox: lease claims, at-least-once, content rendered at send time

*2026-11-04, Phase 6*

**Context.** A notification must go out if and only if its incident change
was committed, the delivery is a slow call to a third party, and several
workers may run at once.

**Decision.** The rows of `notifications` are inserted in the transaction
that opens or resolves the incident (one per active channel of the owner),
and certwatch inserts its rows in the statement that records
`cert_alerted_for`. The notifier claims due rows like the scheduler claims
monitors: `FOR UPDATE SKIP LOCKED` in one `UPDATE` that moves
`next_attempt_at` one lease (1 minute) ahead, then sends **outside** any
transaction and records the outcome in a separate short `UPDATE`. No row
lock is held during an HTTP call. If a worker dies between sending and
recording, the row becomes due again when the lease ends and is sent once
more: delivery is at-least-once, and the webhook payload carries the row
`id` so receivers can drop repeats. In-flight deliveries are not cancelled
by `SIGTERM` (`context.WithoutCancel`, bounded by the 10 s send timeout),
which keeps such repeats rare.

The schema has no payload column, so the message is rendered when it is
sent, from the joined monitor, incident and channel. A renamed monitor is
therefore shown with its new name; for `cert_expiring` the expiry shown is
`cert_alerted_for`, the value that was warned about. Each tick claims at
most 10 rows and sends them in parallel; a full batch is followed by the
next one without waiting for the tick.

**Alternatives.** Hold the row lock while sending (a plain
`SELECT ... FOR UPDATE SKIP LOCKED` in a transaction around the call):
exactly-once towards the database, but one connection and one open
transaction per slow HTTP call. Store a rendered payload: more faithful to
the moment of the event, but a schema change beyond PROMPT.md section 8 for
little gain.

## 028. Retries: exponential backoff, five attempts, permanent errors fail at once

*2026-11-04, Phase 6*

**Decision.** After the n-th failed attempt the next one is due after
`30 s * 2^(n-1)`, capped at 15 minutes: 30 s, 1 m, 2 m, 4 m. The fifth
failure marks the row `failed`; `attempts` and `last_error` keep the
history. There is no jitter: the next time comes from the injected `Clock`,
which keeps the retry tests exact, and deliveries are few and spread over
many receivers. An error is *permanent* and fails the row at once when
retrying cannot help: a 4xx answer other than 408 and 429 (Telegram "chat
not found", a webhook 404), a webhook URL rejected by the address rules, a
disabled channel, or a Telegram channel on a server without
`TELEGRAM_BOT_TOKEN`. A Telegram answer of 200 with `"ok": false` is
treated as a server error and retried.

**Alternatives.** Retry every error five times: simple, but a typo in a
chat ID would cause eight minutes of useless requests. Honour Telegram's
`retry_after`: worth it at higher volume; the backoff already waits longer.

## 029. Webhooks: server-generated secret, HMAC over the raw body, no redirects

*2026-11-04, Phase 6*

**Decision.** The webhook secret is generated by the server
(`crypto/rand.Text`, 128 bits, base32) and returned only in the response to
`POST /channels` (with `Cache-Control: no-store`); it cannot be set or
read back later. Rotating it means creating a new channel. Each delivery
carries `X-Signature: sha256=<hex(HMAC-SHA256(secret, body))>` over the
exact bytes sent, plus `X-Uptime-Event`. The secret is stored in plain text
in `config` because the worker needs it to sign; it never appears in
messages, logs or `last_error`.

Webhook URLs come from users, so they get the protection of monitor URLs:
the URL rules when the channel is saved and on each delivery, and the
connect-time address check of the shared `checker.NewSafeDialer`. Redirects
are not followed (a 3xx is a permanent error): following one would send a
signed body to a place the user never configured.

**Alternatives.** Let users choose the secret: weak secrets and one more
input to validate. Encrypt secrets at rest: needs a key-management story
(where does the key live?) that this project does not have yet; noted for
later.

## 030. Test notifications are sent synchronously and report the outcome in a 200

*2026-11-04, Phase 6*

**Context.** `notifications.monitor_id` is `NOT NULL`, and a user testing a
channel wants an answer now, not a row in a queue.

**Decision.** `POST /channels/{id}/test` sends an `event: test` message
directly through the same senders, with a 10 s timeout and no retries, and
answers `200 {"delivered": bool, "error"?: string}`. The request itself
succeeded even if the delivery did not, and the error list of PROMPT.md
section 9 has no code for "the third party said no". Error texts come from
the senders and never contain the bot token or the webhook secret. Disabled
channels can be tested, so a channel can be checked before it is enabled.
The api therefore also reads `TELEGRAM_BOT_TOKEN` and
`ALLOW_PRIVATE_TARGETS`.

**Alternatives.** 502 for a failed delivery: misleading, the API is not a
gateway. Queue the test in the outbox: needs a nullable `monitor_id` and
gives no immediate answer.

## 031. Channel limit and fan-out

*2026-11-04, Phase 6*

**Decision.** A user may have at most 10 channels (a constant,
`service.MaxChannelsPerUser`, enforced like the monitor limit under the
user row lock). Every incident change is sent to all active channels of the
owner, so the limit also bounds the deliveries one incident causes. If the
owner has no active channel, an incident simply produces no notification,
and certwatch still records `cert_alerted_for`: a channel added later does
not get a warning about a certificate that was already processed.

**Alternatives.** A per-monitor channel selection: useful, but a new table
and API that PROMPT.md does not ask for (stretch).

## 032. Certwatch: one statement, deduplicated by value

*2026-11-04, Phase 6*

**Decision.** Once an hour (and at startup) every worker runs one
statement: a CTE selects active monitors with
`tls_not_after < now + cert_expiry_warn_days` and
`cert_alerted_for IS DISTINCT FROM tls_not_after` (`FOR UPDATE SKIP
LOCKED`), sets `cert_alerted_for = tls_not_after` and inserts the
`cert_expiring` rows. Being one statement, it is one transaction: either
the monitor is marked and the notifications exist, or neither. `now` comes
from the injected clock. A renewed certificate has a new `tls_not_after`,
so it is warned about again when it in turn nears expiry; an already
expired certificate is warned about once. Monitors locked by a check being
saved are left to the next run.

## 033. Notification metrics come with the registry (Phase 7); outbox cleanup later

*2026-11-04, Phase 6*

**Decision.** As in decisions 009 and 024, `uptime_notifications_total`,
`uptime_notification_delivery_delay_seconds`,
`uptime_notifications_pending`,
`uptime_notification_oldest_pending_age_seconds` and
`uptime_monitors_cert_expiring` are added in Phase 7 with the registry.
Their data is there: the outcome and `channel_type` of each delivery,
`sent_at - created_at`, and the partial index on pending rows for the
gauges. Until then every outcome is logged (decision 027) and the outbox
can be inspected with SQL (README, Operations).

Sent and failed rows are kept. PROMPT.md defines retention only for checks;
an outbox cleanup (e.g. rows older than `CHECKS_RETENTION_DAYS`) is a small
addition to the retention loop, left for when the growth of the table
matters.

## 034. Metrics: one injected registry per process, metric types in `observability`

*2026-11-05, Phase 7*

**Context.** Every component needs metrics, the catalog is a contract with
`uptime-infra`, and CLAUDE.md forbids global state (no default registry).

**Decision.** `observability.NewRegistry` creates one `prometheus.Registry`
per process with `go_*`, `process_*` and `uptime_build_info`. Each component
gets a small typed value built on that registry
(`NewSchedulerMetrics(reg)`, `NewNotifierMetrics`, `NewRetentionMetrics`,
`NewHTTPMetrics`) with methods named after events (`CheckDone`,
`Sent`, `Retry`), passed to its constructor. All names and buckets live in
one package, next to the catalog test; components never see label strings.
Vector metrics create every known label value at startup, so a value that
has not happened yet reads 0, not "no data". Registration uses
`MustRegister`: it panics only on a programming error (a duplicate name),
which the tests that build the full registry catch before any release.
Tests read values by name through `observability/obstest`.

**Alternatives.** `promauto` with the default registry: global state, and
tests would share values. Interfaces per component with no-op fakes: more
code, and the real metrics would go untested. Each package defining its own
metrics: the catalog would be scattered across the tree.

## 035. HTTP metrics: probes excluded, route templates only, Logging outside Recover

*2026-11-05, Phase 7*

**Decision.** The metrics middleware sits right after RequestID and wraps
everything else, so it times the whole request and sees the final status.
`/healthz` and `/readyz` are left out of `uptime_http_requests_total` and
the duration histogram (they count towards in-flight only): probes always
succeed and would inflate the availability SLI. The `route` label is the
chi template read after routing; chi reports an unknown path under a
sub-router as the mount pattern `/api/v1/*`, so a pattern ending in `/*`
becomes `unmatched` (the API has no wildcard routes of its own). Methods
outside the standard set become `other`. Access logs use the same route
function.

The middleware order changed to `RequestID, Metrics, Logging, Recover,
BodyLimit`: before, Recover wrapped Logging, so a panicking request had a
panic log but no access log. Now Recover turns the panic into a 500 that
Logging and Metrics record like any other response.

**Alternatives.** Counting probes and filtering them in every query: easy
to forget in one recording rule. Raw paths with IDs replaced by a regexp:
fragile, and junk paths would still create series.

## 036. Database-backed gauges: refreshed by the worker every 15 s

*2026-11-05, Phase 7*

**Decision.** `scheduler_due_monitors`, `notifications_pending`,
`notification_oldest_pending_age_seconds`, `incidents_open`, `monitors`
and `monitors_cert_expiring` come from one SQL statement
(`repository/postgres.Stats`), run by a fifth worker loop every 15 s with a
5 s timeout. A failed refresh is logged at warn level and keeps the last
values; it never stops the worker. Only the worker runs it: the API is
scaled for requests, and the values describe the whole system, so one
source is enough. With several workers every replica reports the same
values: dashboards aggregate them with `max` (docs/metrics.md).
`due_monitors` is measured in the database (active monitors with
`next_check_at <= now()`), which is the backlog of all workers rather than
of the last tick of one. "Expiring" uses the certwatch condition without
the "already warned" part.

**Alternatives.** Querying at scrape time (a custom Collector): every
Prometheus server and every manual `curl` would hit the database, and a
slow database would make scrapes time out. Counting in memory: wrong with
several replicas and after restarts.

## 037. Connection pool metrics: gauges for state, counters for totals

*2026-11-05, Phase 7*

**Decision.** PROMPT.md lists `uptime_db_pool_*` as gauges. The current
state is exported as gauges (`acquired_connections`, `idle_connections`,
`total_connections`, `max_connections`); the cumulative values of
`pgxpool.Stat` are exported as counters with `_total`
(`acquires_total`, `acquire_waits_total`,
`acquire_wait_seconds_total`), because they only grow and `rate()` needs
counters (promlint also requires it). The collector reads `Stat()` at
scrape time: it is in memory and cheap. The API and the worker both export
it.

## 038. Ops ports

*2026-11-05, Phase 7*

**Decision.** The API runs a second `http.Server` on `OPS_ADDR` (`:9090`)
with `/metrics`, `/healthz`, `/readyz` and, with `PPROF_ENABLED=true`,
`/debug/pprof/`. Both servers run in one errgroup and shut down together.
The worker's ops port (`:9091`) gains `/metrics`; pprof stays an API
option, as in the contract with `uptime-infra`. pprof handlers are
registered on the ops mux explicitly (the `net/http/pprof` import also
registers them on `http.DefaultServeMux`, which nothing serves). The ops
server has a 70 s write timeout instead of 30 s: `net/http/pprof` refuses a
CPU profile longer than the write timeout, and the default profile is 30 s.

`make run-api` and `make run-worker` set `OPS_ADDR` per process
(`127.0.0.1:9090` and `127.0.0.1:9091`), because both read the same `.env`.

**Alternatives.** `/metrics` on the public router behind a token: one
leaked token or misconfigured proxy exposes internals; a separate port is
private by network. Separate variables (`API_OPS_ADDR`, `WORKER_OPS_ADDR`):
the contract with `uptime-infra` names one `OPS_ADDR` per process.

## 039. One all-in-one binary in the image, linked as api, worker and migrate

*2026-11-05, Phase 8*

**Context.** The contract asks for one image with `/app/api`,
`/app/worker` and `/app/migrate`, and PROMPT.md for an image under 30 MB.
Three static binaries are 45 MB (stripped, linux/amd64): each carries the
same pgx, chi, Prometheus client and checker code.

**Decision.** The code of each command moves to
`internal/app/{api,worker,migrate}` (`Run(ctx, args, info, stdout)`).
`cmd/uptime-monitor` contains all three and picks one by the name it was
started as (`/app/worker` is a symbolic link to it) or by its first
argument (`uptime-monitor migrate up`); without a command it prints usage
and exits with 2. The binary is 18 MB, the image about 25 MB unpacked
(8 MB compressed). `cmd/api`, `cmd/worker` and `cmd/migrate` stay as thin
mains for local runs, so `make run-*` and the binary names do not change.
The paths of the contract are unchanged for `uptime-infra`.

**Alternatives.** Three binaries and a 47 MB image: misses the size goal.
UPX compression: an extra tool, slower start, and packed binaries are
often flagged by scanners. Hard links: the same size, but symbolic links
show what they point to (`ls -l`).

## 040. Image: distroless/static nonroot, base images pinned by digest

*2026-11-05, Phase 8*

**Decision.** The build stage is `golang:1.27.3-alpine3.24`, run on
`$BUILDPLATFORM` and cross-compiling for `TARGETOS/TARGETARCH`, so a
multi-arch build (Phase 9) needs no emulation. The runtime is
`gcr.io/distroless/static-debian13:nonroot`: CA certificates (the checker
and the senders need them), tzdata, `/etc/passwd`, nothing else. Both are
pinned by tag and digest: the tag tells a reader the version, the digest
makes the build reproducible. `USER 65532:65532` is numeric, so
"runs as non-root" can be checked without reading `/etc/passwd`. Version,
commit and build time come in as build args and go into `-ldflags` and OCI
labels. Module and build caches are BuildKit cache mounts. No
`HEALTHCHECK` in the image: it differs per command, so compose (and
`uptime-infra`) define it with `-healthcheck`. `CMD` defaults to the API.

**Alternatives.** `scratch`: no CA certificates or tzdata without copying
them by hand. Alpine runtime: a shell and a package manager to patch for
no benefit.

## 041. Compose stack: example secret, ports on loopback, hardened containers

*2026-11-05, Phase 8*

**Context.** The definition of done is `cp .env.example .env && docker
compose up --build` on a clean machine, but `JWT_SECRET` has no default
and the API refuses to start without one.

**Decision.** `.env.example` sets `JWT_SECRET` to a public value that
says what it is (`config.ExampleJWTSecret`); the API rejects exactly that
value when `APP_ENV=prod`, and a test keeps `.env.example` and the
constant in sync. Compose reads `.env` for the application settings and
sets what only makes sense inside its network itself (`HTTP_ADDR`,
`OPS_ADDR`, `DATABASE_URL` with host `postgres`), so the same `.env` also
works for `make run-*` on the host. Start order: postgres healthy, migrate
completed successfully, then api and worker; a failed migration starts
nothing new. All ports, ops ports included, are published on 127.0.0.1
only: for local `curl` and `make smoke`, never on the network; production
compose files in `uptime-infra` do not publish the ops ports at all. The
application containers run with a read-only root filesystem, no Linux
capabilities and `no-new-privileges`. `make up` now starts the whole stack;
`make db` starts PostgreSQL alone for running the binaries on the host.
`scripts/smoke.sh` is POSIX sh with curl only (no jq), so it runs in CI
and from Git for Windows.

**Alternatives.** An empty `JWT_SECRET` with an error telling the user to
fill it in: fails the "works as is" requirement. A secret generated at
first start: would change on every restart and log everybody out, or need
a volume for one string.
