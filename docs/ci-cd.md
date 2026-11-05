# CI/CD

Two GitHub Actions workflows: `ci.yml` checks every change, `release.yml`
publishes a version. Every CI step is a make target, so any failure can be
reproduced locally with the same command.

## CI (`.github/workflows/ci.yml`)

Runs on every pull request, on every push to `main`, and as the first stage
of a release. Jobs run in parallel.

| Job | What it checks | Locally |
|---|---|---|
| Build | all binaries compile | `make build` |
| Lint | golangci-lint; actionlint on the workflows; hadolint on the Dockerfile; gitleaks on the whole git history (false positives in `.gitleaksignore`) | `make lint lint-ci lint-docker secrets` |
| Vulnerabilities | govulncheck: known vulnerabilities in code the module calls, Go standard library included | `make vuln` |
| Test | unit tests with `-race` and coverage; `internal/service` and `internal/checker` at least 70 % | `make cover cover-check` |
| Integration tests | unit + integration tests against PostgreSQL in Docker (testcontainers), with `-race` and coverage | `make test-integration` |
| Image | builds `linux/amd64` and `linux/arm64`; image smaller than 30 MB; trivy finds no fixable HIGH/CRITICAL vulnerability; the compose stack from `.env.example` starts and `scripts/smoke.sh` passes | `make docker-build scan up smoke` |

Coverage reports (`coverage.html`, profiles) are attached to each run as
artifacts; the totals are in the run summary.

Caching: `actions/setup-go` caches the Go module and build caches (keyed by
`go.sum`); Docker layers are cached in the GitHub Actions cache
(`type=gha`), which the release workflow reads as well.

Go version: `actions/setup-go` reads the `toolchain` directive of `go.mod`
(`go1.27.3`), the same version as the `golang` image of the Dockerfile.
Locally, `go` downloads that toolchain automatically. Bump both together.

## Release (`.github/workflows/release.yml`)

A tag `vX.Y.Z` (or a pre-release `vX.Y.Z-rc.N`) on a commit of `main`:

1. runs the whole CI on the tagged commit (`workflow_call`);
2. checks that the commit is on `main`;
3. builds the image for `linux/amd64` and `linux/arm64` and pushes it to
   `ghcr.io/artemyaroshev1ch/uptime-monitor` with two tags:
   - `X.Y.Z` (no `v`), what deployments use;
   - `sha-<12 hex digits>`, the commit, as reported by `/version`;

   never `latest`. Provenance and an SBOM are attached to the image;
4. runs the published image and checks that it reports the tag's version;
5. creates a GitHub Release whose notes come from `scripts/changelog.sh`
   (Conventional Commits since the previous tag) plus the pull command and
   the image digest. A tag with a suffix (`-rc.1`) becomes a pre-release.

### Cutting a release

```sh
git switch main && git pull
make changelog                       # preview the notes
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Versions follow SemVer: breaking changes of the operations contract
(`CLAUDE.md`, contract with `uptime-infra`) or of the API bump the major
version (minor while in `0.x`), new features the minor, fixes the patch.

A failed release publishes nothing if it fails before step 3. If it fails
later, fix the cause and re-run the failed jobs; do not move a published
tag: a version, once pulled, must always mean the same image. Release a new
patch version instead.

### Image visibility

The first push creates the package `uptime-monitor` on GitHub as
**private**. `uptime-infra` can either pull it with a token
(`read:packages`) or the package can be made public once:
*GitHub → Packages → uptime-monitor → Package settings → Change visibility*.
The `org.opencontainers.image.source` label links the package to this
repository, so it appears on the repository page.

### Deploying a release

`uptime-infra` pins an exact version, then runs the migrations before the
new application version starts:

```sh
docker run --rm --env-file .env ghcr.io/artemyaroshev1ch/uptime-monitor:0.1.0 /app/migrate up
# then start api and worker with image ...:0.1.0
```

## Dependabot (`.github/dependabot.yml`)

Weekly grouped PRs for Go modules, GitHub Actions (pinned by SHA, the
version comment is updated too), the Dockerfile base images and the
PostgreSQL image of the compose stack. Major PostgreSQL versions and new
minor Go versions are excluded: those are deliberate upgrades (the Go
version lives in `go.mod` and the Dockerfile; PostgreSQL also in
`internal/platform/postgres/pgtest` and `uptime-infra`).

## Branch protection

Recommended rules for `main` (*Settings → Rules → Rulesets*, or *Branches →
Branch protection rules*):

- Require a pull request before merging (no direct pushes).
- Require status checks to pass, and require branches to be up to date
  before merging. Required checks:
  `Build`, `Lint`, `Vulnerabilities (govulncheck)`, `Test`,
  `Integration tests`, `Image (build, scan, e2e)`.
- Block force pushes and deletion of `main`.
- Optionally require one approving review (for a single maintainer this
  blocks every merge, so leave it off or allow bypass for administrators).

A tag ruleset for `v*` (restrict creation, update and deletion to
maintainers) keeps published versions from being moved.

The same rules with the GitHub CLI:

```sh
gh api -X PUT repos/artemyaroshev1ch/uptime-monitor/branches/main/protection \
  --input - <<'EOF'
{
  "required_status_checks": {
    "strict": true,
    "contexts": ["Build", "Lint", "Vulnerabilities (govulncheck)", "Test",
                 "Integration tests", "Image (build, scan, e2e)"]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": {"required_approving_review_count": 0},
  "restrictions": null,
  "allow_force_pushes": false,
  "allow_deletions": false
}
EOF
```
