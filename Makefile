# Developer tasks for Uptime Monitor. Run `make help` for the list.
#
# Portability: recipes are POSIX sh. On Linux/macOS that is the default. On
# native Windows, GNU Make uses sh.exe when it finds it in PATH (Git for
# Windows puts it in "C:\Program Files\Git\bin"); otherwise it silently falls
# back to cmd.exe, so we check for that up front.
ifeq ($(shell echo $$0),$$0)
$(error This Makefile needs a POSIX shell, but make is using cmd.exe. Add "C:\Program Files\Git\bin" to PATH or run make from Git Bash)
endif

ifeq ($(OS),Windows_NT)
# Make starts simple recipe lines (no shell metacharacters) directly, without
# sh, so coreutils (echo, date, rm, mkdir, awk) must be in PATH. Git for
# Windows keeps them in usr\bin, which is not in PATH outside Git Bash. The
# redirect forces this command through sh, which knows where /usr/bin is.
POSIX_BIN := $(shell cygpath -w /usr/bin 2>/dev/null)
export PATH := $(POSIX_BIN);$(PATH)
endif

.DEFAULT_GOAL := help

GO ?= go

# Executable suffix: ".exe" on Windows, empty elsewhere.
GOEXE := $(shell $(GO) env GOEXE)

BIN_DIR   := bin
TOOLS_DIR := $(BIN_DIR)/tools

# Binaries under cmd/: one per command for local runs, plus the all-in-one
# binary of the Docker image (see internal/app).
CMDS := api worker migrate uptime-monitor

# Build metadata injected into package main of every binary via -ldflags.
# Release tags are vX.Y.Z; the version itself is SemVer without the "v".
# Override from the command line, e.g. `make build VERSION=1.2.3`.
VERSION  := $(patsubst v%,%,$(or $(shell git describe --tags --match "v[0-9]*" --dirty 2>/dev/null),0.0.0-dev))
COMMIT   := $(or $(shell git rev-parse --short=12 HEAD 2>/dev/null),unknown)
BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.builtAt=$(BUILT_AT)

# The race detector needs cgo, i.e. a C compiler. Native Windows usually has
# none (CGO_ENABLED=0), so -race is used only where it works; CI always has it.
ifeq ($(shell $(GO) env CGO_ENABLED),1)
RACE := -race
endif

# golangci-lint is pinned and built with the local Go toolchain, so the linter
# always supports the Go version from go.mod. The version is part of the file
# name: bumping it triggers a reinstall. Keep in sync with ci.yml.
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint-$(GOLANGCI_LINT_VERSION)$(GOEXE)

# Other checks run by CI. CI calls these make targets too, so a local run
# and a CI run are the same command. Go tools run with `go run pkg@version`
# (cached by Go, nothing added to go.mod); hadolint and trivy run as pinned
# Docker images, so they need nothing installed besides Docker.
GOVULNCHECK_VERSION := v1.1.4
ACTIONLINT_VERSION  := v1.7.12
HADOLINT_IMAGE      := hadolint/hadolint:v2.15.1
TRIVY_IMAGE         := aquasec/trivy:0.76.2

# Image that docker-build builds and scan checks.
IMAGE ?= uptime-monitor:$(VERSION)

# Packages with a coverage floor (PROMPT.md §13) and the floor in percent.
COVER_PKGS := ./internal/service ./internal/checker
COVER_MIN  := 70

# Exports variables from .env (if present) into the recipe's shell. The file
# is sourced by sh, so it must contain plain KEY=value lines.
LOAD_ENV := if [ -f .env ]; then set -a; . ./.env; set +a; fi;

##@ Development

.PHONY: help
help: ## Show this help
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } \
		/^[a-zA-Z0-9_-]+:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# api and worker share .env but need different ops ports, so OPS_ADDR is set
# per target (it overrides .env). Loopback avoids the Windows Firewall prompt
# and keeps the ops ports private. Override: make run-api API_OPS_ADDR=...
API_OPS_ADDR    ?= 127.0.0.1:9090
WORKER_OPS_ADDR ?= 127.0.0.1:9091

# The binary is built and started instead of `go run`: its path stays the same
# between runs (Windows Firewall asks once per executable path) and signals
# such as Ctrl+C reach the server process directly.
.PHONY: run-api
run-api: build ## Build and run the API (loads .env if present; ops port 9090)
	@$(LOAD_ENV) OPS_ADDR=$(API_OPS_ADDR) ./$(BIN_DIR)/api$(GOEXE)

.PHONY: run-worker
run-worker: build ## Build and run the worker (loads .env if present; ops port 9091)
	@$(LOAD_ENV) OPS_ADDR=$(WORKER_OPS_ADDR) ./$(BIN_DIR)/worker$(GOEXE)

.PHONY: migrate-up
migrate-up: build ## Apply all pending migrations (loads .env)
	@$(LOAD_ENV) ./$(BIN_DIR)/migrate$(GOEXE) up

.PHONY: migrate-down
migrate-down: build ## Roll back the most recent migration (loads .env)
	@$(LOAD_ENV) ./$(BIN_DIR)/migrate$(GOEXE) down

.PHONY: migrate-status
migrate-status: build ## Show which migrations are applied (loads .env)
	@$(LOAD_ENV) ./$(BIN_DIR)/migrate$(GOEXE) status

# The next number is the highest existing prefix + 1. awk reads "0009" as 9,
# where sh arithmetic would treat a leading zero as octal.
.PHONY: migrate-create
migrate-create: ## Create an empty migration: make migrate-create name=add_monitors
	@echo "$(name)" | grep -Eq '^[a-z0-9_]+$$' \
		|| { echo "usage: make migrate-create name=<snake_case_name>" >&2; exit 1; }
	@next=$$(ls migrations | awk -F_ '/^[0-9]+_.*\.sql$$/ { n = $$1 + 0; if (n > max) max = n } END { printf "%04d", max + 1 }'); \
		file="migrations/$${next}_$(name).sql"; \
		printf -- '-- +goose Up\n\n-- +goose Down\n' > "$$file"; \
		echo "created $$file"

.PHONY: build
build: ## Build binaries into bin/
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/ $(addprefix ./cmd/,$(CMDS))

.PHONY: clean
clean: ## Remove built binaries and coverage reports
	rm -f $(addprefix $(BIN_DIR)/,$(addsuffix $(GOEXE),$(CMDS))) coverage.out coverage.html coverage-integration.out

##@ Local stack (Docker Compose)

# Build metadata for the image; compose passes them on as build args.
export VERSION COMMIT BUILT_AT

# --wait returns once every service is healthy (migrate: exited with 0), so
# `make up smoke` works.
.PHONY: up
up: ## Build the image and start the whole stack (postgres, migrate, api, worker)
	docker compose up -d --build --wait

.PHONY: db
db: ## Start only PostgreSQL (for make run-api / run-worker on the host)
	docker compose up -d --wait postgres

.PHONY: down
down: ## Stop the stack (the database volume is kept)
	docker compose down

.PHONY: logs
logs: ## Follow logs of the stack
	docker compose logs -f

.PHONY: docker-build
docker-build: ## Build the image uptime-monitor:<version> for this platform
	docker build -t $(IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILT_AT=$(BUILT_AT) .

# Fails on HIGH or CRITICAL vulnerabilities that have a fix (decision 044).
# trivy reads the image from the local Docker daemon through its socket; the
# named volume keeps the vulnerability database between runs.
# MSYS_NO_PATHCONV: Git Bash would otherwise rewrite /var/run/docker.sock
# into a Windows path; elsewhere the variable means nothing.
.PHONY: scan
scan: ## Scan the image for HIGH/CRITICAL vulnerabilities (trivy; make docker-build first)
	MSYS_NO_PATHCONV=1 docker run --rm \
		-v /var/run/docker.sock:/var/run/docker.sock -v uptime-trivy-cache:/root/.cache \
		$(TRIVY_IMAGE) image --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed \
		--exit-code 1 --no-progress $(IMAGE)

.PHONY: lint-docker
lint-docker: ## Lint the Dockerfile (hadolint)
	docker run --rm -i $(HADOLINT_IMAGE) < Dockerfile

.PHONY: smoke
smoke: ## Check a running stack end to end (make up first)
	sh scripts/smoke.sh

##@ Quality

.PHONY: test
test: ## Run unit tests (-race when cgo is available)
ifndef RACE
	@echo "note: cgo is disabled (no C compiler), running tests without -race"
endif
	$(GO) test $(RACE) ./...

# -count=1 disables the test cache: results depend on the containers, not
# only on the code.
.PHONY: test-integration
test-integration: ## Run unit and integration tests with coverage (needs Docker)
	$(GO) test $(RACE) -tags integration -count=1 \
		-covermode=atomic -coverprofile=coverage-integration.out ./...
	@$(GO) tool cover -func=coverage-integration.out | tail -n 1

.PHONY: cover
cover: ## Run unit tests with coverage, write coverage.html
	$(GO) test $(RACE) -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1
	$(GO) tool cover -html=coverage.out -o coverage.html

.PHONY: cover-check
cover-check: ## Fail if a key package is below the coverage floor (service, checker: 70%)
	sh scripts/cover-check.sh $(COVER_MIN) $(COVER_PKGS)

.PHONY: vuln
vuln: ## Check dependencies and the Go toolchain for known vulnerabilities (govulncheck)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: lint-ci
lint-ci: ## Lint the GitHub Actions workflows (actionlint)
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format code (gofmt + goimports)
	$(GOLANGCI_LINT) fmt ./...

.PHONY: tidy
tidy: ## Tidy and verify go.mod/go.sum
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: tools
tools: $(GOLANGCI_LINT) ## Install pinned dev tools into bin/tools

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION) (first run takes a minute)"
	@mkdir -p $(TOOLS_DIR)/tmp
	GOBIN="$(abspath $(TOOLS_DIR)/tmp)" $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv "$(TOOLS_DIR)/tmp/golangci-lint$(GOEXE)" "$@"
	@rm -rf "$(TOOLS_DIR)/tmp"

##@ Release

# Release notes from Conventional Commits since the previous tag. The release
# workflow uses the same script for the GitHub Release.
.PHONY: changelog
changelog: ## Print the changelog for a release: make changelog [TAG=v0.1.0]
	sh scripts/changelog.sh $(TAG)
