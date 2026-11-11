# syntax=docker/dockerfile:1

# BINARY selects where /server comes from:
#   builder  (default) compile from source inside the image
#   prebuilt copy dist/server, built beforehand on the host (CI builds it with a
#            warm Go cache, which is much faster than compiling inside BuildKit)
ARG BINARY=builder

# ── Stage 1a: Build from source ───────────────────────────────────────────────
FROM golang:1.27 AS builder

WORKDIR /workspace

COPY . .

# VERSION is the release tag (e.g. v0.6.2). The backend warns cappctl clients
# that are at least one minor release behind it; "dev" disables the check.
ARG VERSION=dev

# Build a statically linked binary so it runs in the distroless image without
# glibc. CGO_ENABLED=0 ensures pure Go networking is used (no cgo resolver).
# The cache mounts keep modules and compiled packages across builds, so a
# source change recompiles only the affected packages.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o bin/server ./cmd/server/...

# ── Stage 1b: Prebuilt binary ─────────────────────────────────────────────────
FROM scratch AS prebuilt
COPY dist/server /workspace/bin/server

FROM ${BINARY} AS binary

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=binary /workspace/bin/server /server

# Run as the nonroot user provided by the distroless image (UID 65532).
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/server"]
