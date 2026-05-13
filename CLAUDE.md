# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build           # Build server binary → bin/server
make run             # Run locally with default config (config/config.yaml)
make test            # Unit tests with race detection (go test -v -race ./...)
make test-cover      # Generate HTML coverage report
make lint            # golangci-lint
make fmt             # gofmt
make vet             # go vet
make tidy            # go mod tidy
make docker-build    # Build Docker image
make build-cli       # Build cappctl CLI binary → bin/cappctl
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

> **Rule:** Every route change or addition must also update `api/openapi.yaml`. Use the `gin-openapi-sync` skill to regenerate it.

```
/healthz, /readyz, /metrics, /docs, /openapi.yaml     (no auth)

GET    /api/v1/auth/mode                               (no auth)
POST   /api/v1/auth/login                              (no auth)
POST   /api/v1/auth/refresh                            (no auth)
GET    /api/v1/auth/openshift/authorize                (no auth, openshift mode only)
POST   /api/v1/auth/openshift/callback                 (no auth, openshift mode only)
GET    /api/v1/clusters[/:cluster]                     (auth)

GET    /api/v1/clusters/:cluster/namespaces            (auth + cluster) — lists CAPP-labeled namespaces, filtered by SelfSubjectAccessReview
POST   /api/v1/clusters/:cluster/namespaces            (auth + cluster) — creates namespace with dana.io/capp-ns=true label; response includes canCreate field

GET    /api/v1/clusters/:cluster/capps                 (auth + cluster) — list all capps across namespaces
       /api/v1/clusters/:cluster/namespaces/:namespace/capps[/:name]         (CRUD)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync      (auth + cluster) — trigger Capp sync; returns 501 if syncer not configured
       /api/v1/clusters/:cluster/namespaces/:namespace/configmaps[/:name]    (CRUD)
       /api/v1/clusters/:cluster/namespaces/:namespace/secrets[/:name]       (CRUD)
GET    /api/v1/clusters/:cluster/configmaps            (auth + cluster) — list all configmaps across namespaces (dana.io/capp-managed=true only)
GET    /api/v1/clusters/:cluster/secrets               (auth + cluster) — list all secrets across namespaces
```

### Key Patterns

**ResourceHandler interface** (`internal/resources/registry.go`): Pluggable handlers register routes and are enabled/disabled via config. Current handlers: `capps`, `namespaces`, `configmaps`, `secrets`. To add a new resource type, implement the interface and register it in `cmd/server/main.go`.

**AuthManager interface** (`internal/auth/manager.go`): Factory selects implementation based on `auth.mode` config. Each mode implements `Authenticate`, `Login`, `Refresh`. JWT and Dex modes use an in-memory session store with background cleanup. OpenShift mode is fully stateless — it validates tokens via TokenReview and impersonates users on managed clusters. `GET /api/v1/auth/mode` returns the active mode (used by cappctl auto-detection).

**ClusterManager** (`internal/cluster/manager.go`): Holds connections to all configured clusters, runs health checks every 30s, and creates per-request scoped clients.

**DTO conversion** (`internal/resources/namespaced/capps/convert.go`): Converts between API request/response types and Kubernetes Capp resources. The CLI (`internal/cli/capps/capps.go`) imports these same types directly — no duplication.

### Configuration

Loaded from YAML file (`--config` flag) + environment variable overrides with `CAPP_` prefix and `_` separators (e.g., `CAPP_AUTH_MODE=jwt`). Priority: env vars > YAML > code defaults.

Default config: `config/config.yaml`. Key settings:
- `auth.mode`: passthrough | jwt | static | dex | openshift
- `clusters[]`: array of cluster connections with credentials
- `resources.<name>.enabled`: feature flags for resource handlers
- `server.corsAllowedOrigins`: defaults to Vite dev server (`localhost:5173`)

### Project Layout

```
cmd/
  server/main.go                # Server entry point, startup sequence
  cappctl/main.go               # CLI entry point
internal/
  server/                       # Gin engine setup, route registration, auth route handlers
  config/                       # Config structs, Viper loading, validation
  auth/                         # AuthManager implementations (passthrough, jwt, static, dex, openshift)
  cluster/                      # ClusterManager, health checks, credential loading
  middleware/                   # Auth, cluster, logging, metrics, CORS, rate limit, recovery
  resources/                    # Resource handler registry
    cluster/
      namespaces/               # Namespace list + create handler (with SelfSubjectAccessReview filtering)
    namespaced/
      capps/                    # Capp CRUD handler, DTO types, K8s conversion
      configmaps/               # ConfigMap CRUD handler (dana.io/capp-managed=true filter on list)
      secrets/                  # Secret CRUD handler
  cli/                          # cappctl CLI packages (no Gin/K8s imports)
    config/                     # ~/.config/cappctl/config.yaml read/write, context CRUD
    client/                     # HTTP client: auth header, JSON encode/decode, API error mapping
    output/                     # table/wide/json/yaml renderer
    resource/                   # ResourceCommand interface + registry
    root/                       # State struct, root Cobra command, PersistentPreRunE, token refresh
    auth/                       # login, logout, context commands
    capps/                      # Capps ResourceCommand: get/list/create/update/delete
  apierrors/                    # Canonical error types
pkg/k8s/                        # Kubernetes scheme builder (CRD type registration)
api/openapi.yaml                # OpenAPI 3.1 spec (embedded in binary)
helm/
  capp-backend/                 # Helm chart for the server
  capp-template/                # Generic Helm chart for deploying a single Capp resource
```

### cappctl CLI

Binary at `cmd/cappctl/`. Built with Cobra. No Gin or K8s imports — pure Go + stdlib.

**Command tree:**
```
cappctl login / logout
cappctl context list | use <name> | current
cappctl get      capps [name]
cappctl create   capps
cappctl update   capps <name>
cappctl delete   capps <name>
```

**Global flags:** `--server`, `--token`, `--cluster`, `--namespace`, `--context`, `--output` (table|wide|json|yaml), `--insecure`

**Env var overrides (CI-friendly):** `CAPP_SERVER`, `CAPP_TOKEN`, `CAPP_CLUSTER`, `CAPP_NAMESPACE`

**Config file:** `~/.config/cappctl/config.yaml` (0600, atomic write). Stores named contexts with server URL, auth mode, token pair, cluster, namespace.

**Token refresh:** `PersistentPreRunE` in `internal/cli/root/root.go` checks `token-expires-at` before every request; refreshes transparently if expiry < 30s away.

**Adding a resource:** implement `ResourceCommand` interface (`internal/cli/resource/handler.go`), call `registry.Register` in `cmd/cappctl/main.go`. No other files change.

### Startup Sequence
1. Load config (YAML + env vars) → validate
2. Init zap logger
3. Build K8s scheme (register CRD types)
4. Create ClusterManager → connect to clusters → start health checks
5. Create AuthManager (based on auth.mode) → start session cleanup (jwt/dex)
6. Build resource handler registry
7. Create Gin server → listen on :8080
8. Graceful shutdown on SIGTERM/SIGINT (30s drain)
