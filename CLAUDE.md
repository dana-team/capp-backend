# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build           # Build binary → bin/server
make run             # Run locally with default config (config/config.yaml)
make test            # Unit tests with race detection (go test -v -race ./...)
make test-cover      # Generate HTML coverage report
make lint            # golangci-lint
make fmt             # gofmt
make vet             # go vet
make tidy            # go mod tidy
make docker-build    # Build Docker image
```

## Architecture

Multi-cluster API gateway for Capp resources, built with **Go + Gin**.

### Request Flow
```
Request → Recovery → Logging → Metrics → CORS → RateLimit → Auth → ClusterResolution → ResourceHandler
```

1. **Auth middleware** validates the token (mode-dependent) and attaches a credential to context
2. **Cluster middleware** resolves `:cluster` param, checks health, builds a per-request scoped K8s client with the user's credential, validates namespace against allowed list
3. **Resource handler** performs the CRUD operation using the scoped client

### API Routes
```
/healthz, /readyz, /metrics, /docs, /openapi.yaml     (no auth)

POST   /api/v1/auth/login                              (no auth)
POST   /api/v1/auth/refresh                            (no auth)
GET    /api/v1/auth/openshift/authorize                (no auth, openshift mode)
POST   /api/v1/auth/openshift/callback                 (no auth, openshift mode)
GET    /api/v1/clusters[/:cluster]                     (auth)

GET    /api/v1/clusters/:cluster/namespaces             (auth + cluster)
GET    /api/v1/clusters/:cluster/capps                  (auth + cluster)
       /api/v1/clusters/:cluster/namespaces/:namespace/capps[/:name]  (CRUD)
```

### Key Patterns

**ResourceHandler interface** (`internal/resources/registry.go`): Pluggable handlers register routes and are enabled/disabled via config. Current handlers: `capps`, `namespaces`. To add a new resource type, implement the interface and register it.

**AuthManager interface** (`internal/auth/manager.go`): Factory selects implementation based on `auth.mode` config. Each mode implements `Authenticate`, `Login`, `Refresh`. JWT and Dex modes use an in-memory session store with background cleanup. OpenShift mode is fully stateless — it validates tokens via TokenReview and impersonates users on managed clusters.

**ClusterManager** (`internal/cluster/manager.go`): Holds connections to all configured clusters, runs health checks every 30s, and creates per-request scoped clients.

**DTO conversion** (`internal/resources/capps/convert.go`): Converts between API request/response types and Kubernetes Capp resources. Similar to the frontend's `cappBuilder.ts`.

### Configuration

Loaded from YAML file (`--config` flag) + environment variable overrides with `CAPP_` prefix and `_` separators (e.g., `CAPP_AUTH_MODE=jwt`). Priority: env vars > YAML > code defaults.

Default config: `config/config.yaml`. Key settings:
- `auth.mode`: passthrough | jwt | static | dex | openshift
- `clusters[]`: array of cluster connections with credentials
- `resources.<name>.enabled`: feature flags for resource handlers
- `server.corsAllowedOrigins`: defaults to Vite dev server (`localhost:5173`)

### Project Layout

```
cmd/server/main.go              # Entry point, startup sequence
internal/
  server/                       # Gin engine setup, route registration
  config/                       # Config structs, Viper loading, validation
  auth/                         # AuthManager implementations (passthrough, jwt, static, dex, openshift)
  cluster/                      # ClusterManager, health checks, credential loading
  middleware/                   # Auth, cluster, logging, metrics, CORS, rate limit, recovery
  resources/                    # Resource handler registry
    capps/                      # Capp CRUD handler + DTO types + conversion
    namespaces/                 # Namespace listing handler
  apierrors/                    # Canonical error types
pkg/k8s/                        # Kubernetes scheme builder (CRD type registration)
api/openapi.yaml                # OpenAPI 3.1 spec (embedded in binary)
```

### Startup Sequence
1. Load config (YAML + env vars) → validate
2. Init zap logger
3. Build K8s scheme (register CRD types)
4. Create ClusterManager → connect to clusters → start health checks
5. Create AuthManager (based on auth.mode) → start session cleanup (jwt/dex)
6. Build resource handler registry
7. Create Gin server → listen on :8080
8. Graceful shutdown on SIGTERM/SIGINT (30s drain)
