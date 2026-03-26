# capp-backend

`capp-backend` is the REST API server for the [Capp Console](https://github.com/dana-team/capp-frontend). It sits between the frontend and one or more Kubernetes/OpenShift clusters, handling authentication, cluster routing, and lifecycle management of [**Capp**](https://github.com/dana-team/container-app-operator) (ContainerApp) custom resources.

## Features

- **Pluggable authentication** — Four modes: `passthrough`, `jwt`, `static`, and `dex` (Dex OIDC username/password).
- **Multi-cluster support** — Configure any number of clusters; each is health-checked every 30 seconds.
- **Capp CRUD** — Full create / read / update / delete for `Capp` resources across namespaces.
- **Namespace listing** — Returns the namespaces visible to each cluster's credentials.
- **Interactive API docs** — Embedded [Scalar](https://scalar.com/) UI served at `/docs` alongside a raw OpenAPI 3.1 spec at `/openapi.yaml`. Both work in air-gapped environments (assets are compiled into the binary).
- **Observability** — Structured JSON logging (zap), Prometheus metrics (`/metrics`), and optional OTLP tracing.
- **Rate limiting** — Token-bucket rate limiter on all endpoints (configurable; on by default).

## Prerequisites

- **Go** 1.25 or later (for building from source)
- One or more Kubernetes/OpenShift clusters with the [`container-app-operator`](https://github.com/dana-team/container-app-operator) installed (provides the `rcs.dana.io/v1alpha1` API group)
- For `dex` auth mode: a [Dex](https://dexidp.io/) instance with a static client that has `grantTypes: [password]` enabled

## Getting Started

### Build from source

```bash
go build -o bin/server ./cmd/server/...
```

### Run

```bash
./bin/server --config config/config.yaml
```

The server listens on port `8080` by default. All configuration can be overridden via environment variables prefixed with `CAPP_` (e.g. `CAPP_SERVER_PORT=9090`).

### Docker

```bash
docker build -f deploy/Dockerfile -t capp-backend .
docker run -p 8080:8080 -v $(pwd)/config/config.yaml:/etc/capp/config.yaml capp-backend --config /etc/capp/config.yaml
```

## Configuration

Configuration is loaded from a YAML file specified with the `--config` flag (e.g. `--config config/config.yaml`). Every field can be overridden by an environment variable — replace dots with underscores and prefix with `CAPP_` (e.g. `auth.jwt.secretKey` → `CAPP_AUTH_JWT_SECRETKEY`). If `--config` is omitted, no config file is read and only environment variables and built-in defaults are used.

### Full reference

```yaml
server:
  port: 8080
  readTimeoutSeconds: 30
  writeTimeoutSeconds: 30
  idleTimeoutSeconds: 60
  corsAllowedOrigins:
    - "http://localhost:3000"

auth:
  # Mode: passthrough | jwt | static | dex
  mode: passthrough

  jwt:
    # Required in jwt and dex modes. Inject via CAPP_AUTH_JWT_SECRETKEY.
    secretKey: ""
    tokenTTLMinutes: 60        # Access token lifetime
    refreshTTLMinutes: 1440    # Refresh token lifetime (24 h)

  static:
    # For development / CI only. List of accepted bearer tokens.
    apiKeys: []

  dex:
    endpoint: "https://dex.example.com"   # Dex issuer URL
    clientId: "capp-backend"
    # Inject via CAPP_AUTH_DEX_CLIENTSECRET.
    clientSecret: ""
    scopes: [openid, profile, email]
    # Optional: base64-encoded PEM CA bundle for TLS to the Dex server.
    caCert: ""

  rateLimit:
    enabled: true
    requestsPerSecond: 20
    burst: 40

logging:
  level: info       # debug | info | warn | error
  format: json      # json | console
  addCallerInfo: false

metrics:
  enabled: true
  path: /metrics

tracing:
  enabled: false
  otlpEndpoint: "localhost:4317"
  serviceName: "capp-backend"
  sampleRate: 0.1

clusters:
  - name: "local"                 # Used as path parameter in /api/v1/clusters/:cluster
    displayName: "Local Cluster"  # Human-readable label for the UI
    allowedNamespaces: []         # Empty = all namespaces allowed
    credential:
      # Option A: kubeconfig file
      kubeconfigPath: "/home/user/.kube/config"
      # Option B: inline credentials
      inline:
        apiServer: "https://my-cluster:6443"
        caCert: "<base64-encoded PEM>"
        token: "<service-account-token>"

resources:
  namespaces:
    enabled: true
  capps:
    enabled: true
```

## Authentication Modes

### `passthrough` (default)

The client's `Authorization: Bearer <token>` header is forwarded directly to each Kubernetes API server. No server-side state is created. Suitable for initial deployment and setups where clients already hold cluster tokens.

### `jwt`

Clients POST a cluster name and a raw Kubernetes bearer token to `/api/v1/auth/login`. The backend validates the token against the cluster, issues a short-lived **access JWT** and a long-lived **refresh JWT**, and stores a server-side session. The raw token never travels over the wire again after login.

Requires `auth.jwt.secretKey`.

### `static`

A fixed list of API keys in `auth.static.apiKeys`. All keys grant the same access. Intended for development or CI environments where key management is not needed.

### `dex`

Clients POST a `username` and `password` to `/api/v1/auth/login`. The backend exchanges the credentials for an OIDC ID token from Dex (Resource Owner Password Credentials grant), verifies it, and issues backend-managed JWTs identical to `jwt` mode. Kubernetes API calls use the cluster's **pre-configured service-account token** — user identity is not forwarded to Kubernetes.

Requires `auth.dex.endpoint`, `auth.dex.clientId`, `auth.dex.clientSecret`, and `auth.jwt.secretKey`. The Dex static client must have `grantTypes: [password]` enabled.

## API Reference

The full OpenAPI 3.1 spec is embedded in the binary and served at runtime:

| Endpoint | Description |
|---|---|
| `GET /docs` | Interactive Scalar API documentation |
| `GET /openapi.yaml` | Raw OpenAPI 3.1 spec |

### Endpoint summary

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/api/v1/auth/login` | — | Sign in (jwt / dex / static modes) |
| `POST` | `/api/v1/auth/refresh` | — | Refresh access token (jwt / dex modes) |
| `GET` | `/api/v1/clusters` | ✓ | List configured clusters |
| `GET` | `/api/v1/clusters/:cluster` | ✓ | Get cluster metadata |
| `GET` | `/api/v1/clusters/:cluster/namespaces` | ✓ | List namespaces |
| `GET` | `/api/v1/clusters/:cluster/capps` | ✓ | List all Capps across namespaces |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps` | ✓ | List Capps in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps` | ✓ | Create a Capp |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Get a Capp |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Update a Capp |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Delete a Capp |
| `GET` | `/healthz` | — | Liveness probe |
| `GET` | `/readyz` | — | Readiness probe (healthy when ≥1 cluster is reachable) |
| `GET` | `/metrics` | — | Prometheus metrics (if enabled) |

## Project Structure

```
cmd/server/         # Entry point — wires config, auth, clusters, and HTTP server
api/                # OpenAPI 3.1 spec (embedded in binary via go:embed)
config/             # Default config.yaml
deploy/             # Dockerfile, Kubernetes manifests, Helm chart skeleton
internal/
├── apierrors/      # Canonical error types and Gin response helpers
├── auth/           # Auth manager interface + passthrough, jwt, static, dex implementations
├── cluster/        # ClusterManager — multi-cluster routing and health checks
├── config/         # Config structs, Viper loading, and validation
├── middleware/      # Gin middleware: auth, cluster resolution, CORS, logging, metrics, rate limiting
├── resources/      # Resource handler registry
│   ├── capps/      # Capp list, get, create, update, delete handlers
│   └── namespaces/ # Namespace list handler
└── server/         # Gin engine setup, route registration, auth endpoints
pkg/k8s/            # Kubernetes scheme builder (registers CRD types)
```

## Kubernetes Deployment

A reference `Deployment`, `Service`, `Secret`, and `ConfigMap` are provided in `deploy/deployment.yaml` and `deploy/configmap.yaml`.

The container runs as a non-root user (`UID 65532`) with a read-only root filesystem. Liveness and readiness probes are pre-configured on `/healthz` and `/readyz`.

Sensitive values (`auth.jwt.secretKey`, cluster tokens) should be provided via Kubernetes Secrets and mounted as environment variables:

```bash
CAPP_AUTH_JWT_SECRETKEY=<secret>
CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN=<sa-token>
```

> **Note:** In `jwt` and `dex` modes, sessions are stored in-memory. Running more than one replica requires a shared session store (e.g. Redis). For single-replica deployments, the in-memory store is sufficient.

## Development

```bash
# Run tests
go test ./...

# Lint (requires golangci-lint)
golangci-lint run

# Build
go build ./...
```

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.
