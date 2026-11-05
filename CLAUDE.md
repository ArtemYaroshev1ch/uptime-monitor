# CLAUDE.md

Working context for AI assistants in this repository. The full specification
is `PROMPT.md`; this file is a digest of its sections 2, 3 and 13 plus the
local environment. If they disagree, `PROMPT.md` wins.

Uptime Monitor is a self-hosted uptime monitoring service in Go (API + worker +
migrate). It is the "workload" application for an SRE learning project; servers,
observability stack, deploy and backups live in the sibling repo `uptime-infra`.

## Working rules (PROMPT.md §2)

1. Work in phases (PROMPT.md §14). Do not start the next phase until the
   current one meets its Definition of Done.
2. At the start of a phase print a short plan (5–10 lines). At the end run
   `make lint test`, show the result, list the commits and **stop and wait for
   the user's confirmation**. Also give a 5–10 line explanation of key
   decisions and name 2–3 places in the code worth studying.
3. Git: one phase = branch `feat/phase-N-<slug>` from `main`. Small commits,
   Conventional Commits in English. Commit locally only. **No `git push`, no
   PRs, no `--force` without an explicit command.**
4. No secrets in the repo. Only `.env.example` with safe placeholders; `.env`
   is git-ignored.
5. Dependencies: standard library first. Before adding a library, explain why
   in one line. Check current versions; do not rely on memory.
6. Tests are written together with the code. Code without tests is not done.
7. Language: code, comments, commits, README, OpenAPI — English. Talk to the
   user in Russian.
8. Metrics are part of the feature: a new loop, queue or external call is not
   finished without its metrics from PROMPT.md §10.
9. Migrations must be backward compatible (expand/contract): the new schema
   must work with the **previous** app version. Destructive changes (drop
   column) go in a later release, after the old code is gone.
10. Never use the `latest` tag for base images in Dockerfile/compose; pin
    exact versions.

Ambiguous requirement → pick the simplest reasonable option and record it in
`docs/decisions.md` (context → decision → alternatives). Ask the user only if
the choice changes the architecture. No "magic": add a short comment explaining
why for non-obvious constructs (`errgroup`, `SKIP LOCKED`, `Dialer.Control`,
`httptrace`).

## Contract with `uptime-infra` (PROMPT.md §3)

This is the operations API. Do not change it without updating `uptime-infra`.

| Area | Contract |
|---|---|
| Image | `ghcr.io/<username>/uptime-monitor:<semver>` (no `v`, e.g. `1.2.0`) + `sha-<commit>`. One image, three binaries: `/app/api`, `/app/worker`, `/app/migrate`. Platforms `linux/amd64`, `linux/arm64`. |
| Ports | `api`: `8080` (public REST API), `9090` (ops: `/metrics`, optional `/debug/pprof`). `worker`: `9091` (ops: `/healthz`, `/readyz`, `/metrics`). Ops ports are never published externally. |
| Liveness | `GET /healthz` — process is alive, does **not** check the DB. |
| Readiness | `GET /readyz` — DB reachable. Worker additionally: scheduler heartbeat fresher than 30 s. |
| Container healthcheck | `-healthcheck` flag on `api` and `worker`: requests localhost, exits 0/1 (distroless has no curl). |
| Logs | JSON to stdout with `time`, `level`, `msg`, `service` (`api`/`worker`), `version`, `request_id`, plus `monitor_id`/`incident_id` where relevant. No secrets. |
| Metrics | Prometheus; catalog in PROMPT.md §10 and `docs/metrics.md`. Names and labels are a public contract. |
| Migrations | Separate binary `/app/migrate up`, run **before** the new version starts. Backward compatible (rule 9). |
| Shutdown | `SIGTERM` → graceful shutdown. `SHUTDOWN_TIMEOUT` default `25s` (below compose `stop_grace_period: 30s`). |
| Configuration | Environment variables only (PROMPT.md §12). |
| Scaling | `api` is stateless, N replicas OK (in-memory rate limiter is a known limitation, documented in `docs/decisions.md`). `worker`: N replicas safe thanks to `SKIP LOCKED` (needed for blue/green and zero-downtime deploys). |
| Version | `GET /version` → `{"version","commit","built_at"}`; metric `uptime_build_info`. SemVer, tags `vX.Y.Z`. |

## Code standards (PROMPT.md §13)

- `gofmt`/`goimports`. Linters (`.golangci.yml`): errcheck, govet,
  staticcheck, revive, gosec, errorlint, bodyclose, sqlclosecheck, noctx,
  gocritic, unused, ineffassign.
- Errors: wrap with `fmt.Errorf("...: %w", err)`, inspect with
  `errors.Is/As`. Domain errors map to HTTP codes in **one place**.
- `context.Context` is the first argument of every function doing I/O.
- No global state and no `init()`; dependencies via constructors (the metrics
  registry is injected too, never the global default registry).
- No `panic` in production code; every goroutine has a stop condition.
- Time comes from an injected `Clock` for deterministic tests.
- No `fmt.Println`; logging only via `slog`.

### Tests

- Unit: table-driven, run with `-race` (in CI; see environment notes).
- `checker`: `httptest.Server` (200, 500, timeout, redirect, keyword,
  private IP blocking, DNS rebinding via a custom resolver); timing fields
  `dns_ms/connect_ms/tls_ms/ttfb_ms` on `httptest.NewTLSServer`; error
  classification (expired/self-signed cert → `tls`).
- `service`: fake repositories (thresholds, incident open/close, limits).
- Integration: `testcontainers-go`, build tag `integration` — repositories,
  concurrent scheduler (two consumers never take the same monitor), outbox
  retries, certwatch deduplication.
- `httpapi`: handlers via `httptest`, authorization, data isolation.
- `observability`: cardinality test (`route` = template or `unmatched`),
  `promlint` via `testutil.GatherAndLint`, all catalog metrics present.
- Coverage of `internal/service` and `internal/checker` ≥ 70 %.

## Make targets

Run `make help` for the current list: `help`, `run-api`, `run-worker`,
`migrate-up`, `migrate-down`, `migrate-status`, `migrate-create name=...`,
`build`, `clean`, `up` (whole compose stack, built), `db` (PostgreSQL
only), `down`, `logs`, `docker-build`, `smoke`, `test`, `test-integration`
(needs Docker), `cover`, `cover-check`, `vuln`, `lint`, `lint-ci`,
`lint-docker`, `scan`, `changelog`, `fmt`, `tidy`, `tools`. CI calls these
same targets (`docs/ci-cd.md`).

- `make lint test` — the phase gate.
- golangci-lint is pinned in the Makefile (`GOLANGCI_LINT_VERSION`, keep in
  sync with `.github/workflows/ci.yml`) and built into `bin/tools/`.
- Build metadata (`version`, `commit`, `builtAt`) is injected into
  `package main` via `-ldflags`.
- Command code lives in `internal/app/{api,worker,migrate}`; `cmd/*` are
  thin mains. The image ships `cmd/uptime-monitor` (all-in-one) with
  `/app/api|worker|migrate` as symlinks (decision 039).

## Environment

- Windows 11, native, **no WSL**. Project at `D:\projects\uptime-monitor`.
- Go 1.27 (`go.mod`: `go 1.27.0`, `toolchain go1.27.3`, same as the
  Dockerfile; bump both together), module
  `github.com/artemyaroshev1ch/uptime-monitor`.
- Git for Windows; `C:\Program Files\Git\bin` is in PATH, `usr\bin` is not.
  `core.autocrlf=true` system-wide, so `.gitattributes` forces LF for all text.
- GNU Make (ezwinports). Make runs recipes through `sh` from Git Bash; the
  user runs `make` from both PowerShell and Git Bash.

Consequences for code and tooling:

- Makefile recipes and scripts must be POSIX sh. Invoke scripts as
  `sh script.sh`. Simple recipe lines are executed without sh, so the
  Makefile prepends Git's `/usr/bin` (via `cygpath -w`) to PATH on Windows.
- Use `go env GOEXE` for binary names (`.exe` on Windows).
- No C compiler locally → `CGO_ENABLED=0` → no `-race` locally; the Makefile
  enables `-race` only when cgo is available. CI (Linux) always runs with race.
- Binding `:8080` triggers a Windows Firewall prompt; for local verification
  bind `127.0.0.1:<port>`.
- Connection refused on loopback takes ~2 s on Windows; bound such tests with
  a context timeout.
- Git Bash rewrites Unix-looking arguments (`/app/worker` →
  `C:/Program Files/Git/app/worker`): use `MSYS_NO_PATHCONV=1` for
  `docker run ... /app/...`.
- `docker compose config` prints `.env` with secrets: use `--quiet`.
- Docker-dependent work (compose, testcontainers) must also run in CI on Linux;
  do not assume Linux-only paths or tools in Go code or tests.
