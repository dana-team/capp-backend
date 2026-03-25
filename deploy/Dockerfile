# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.25 AS builder

WORKDIR /workspace

# Cache module downloads separately from source so that a source change does
# not invalidate the module download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a statically linked binary so it runs in the distroless image without
# glibc. CGO_ENABLED=0 ensures pure Go networking is used (no cgo resolver).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o bin/server ./cmd/server/...

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/bin/server /server

# Run as the nonroot user provided by the distroless image (UID 65532).
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/server"]
