# One image, three commands: /app/api, /app/worker and /app/migrate are
# links to a single static binary (cmd/uptime-monitor, see internal/app).
#
#   docker build -t uptime-monitor .                 # or: make docker-build
#   docker run --rm uptime-monitor /app/api -version
#
# Base images are pinned by tag and digest: the tag says what it is, the
# digest makes the build reproducible (a tag can be moved, a digest cannot).
# Bump both together.

# --- build ---------------------------------------------------------------
# BUILDPLATFORM: the compiler runs natively on the build machine and
# cross-compiles for TARGETOS/TARGETARCH, so a multi-arch build needs no
# emulation (CGO is off, Go cross-compiles on its own).
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build

WORKDIR /src

# Dependencies first: this layer is reused as long as go.mod and go.sum do
# not change. The cache mounts keep the module and build caches between
# builds without putting them into any layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY migrations/ migrations/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown

# CGO_ENABLED=0: a static binary that needs no libc, so it runs on
# distroless/static. -trimpath drops local paths from the binary (a
# reproducible build); -s -w drop the symbol table and DWARF debug info
# (about a quarter of the size; stack traces still have function names).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILT_AT}" \
      -o /out/app/uptime-monitor ./cmd/uptime-monitor \
 && ln -s uptime-monitor /out/app/api \
 && ln -s uptime-monitor /out/app/worker \
 && ln -s uptime-monitor /out/app/migrate

# --- runtime -------------------------------------------------------------
# distroless/static: CA certificates, tzdata and /etc/passwd, nothing else:
# no shell, no package manager, no curl. Less to patch and nothing for an
# attacker to work with. The "nonroot" variant runs as uid 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown
LABEL org.opencontainers.image.title="uptime-monitor" \
      org.opencontainers.image.description="Self-hosted uptime monitoring: api, worker and migrate" \
      org.opencontainers.image.source="https://github.com/artemyaroshev1ch/uptime-monitor" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILT_AT}"

# COPY keeps the links as links.
COPY --from=build /out/app/ /app/

# Numeric, so an orchestrator can verify "not root" without /etc/passwd.
USER 65532:65532

# api: 8080 public, 9090 ops. worker: 9091 ops. Ops ports stay internal.
EXPOSE 8080 9090 9091

# The command is chosen by the deployment (/app/worker, /app/migrate up);
# the API is the default. Health checks too: "/app/api -healthcheck" or
# "/app/worker -healthcheck" (no curl in the image).
CMD ["/app/api"]
