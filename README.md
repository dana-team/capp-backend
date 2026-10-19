# capp-backend

`capp-backend` is the REST API server for the [Capp Console](https://github.com/dana-team/capp-frontend). It sits between the frontend and one or more Kubernetes/OpenShift clusters, handling authentication, cluster routing, and lifecycle management of [**Capp**](https://github.com/dana-team/container-app-operator) (ContainerApp) custom resources.

## Features

- **Pluggable authentication** — Three modes: `passthrough`, `openshift` (OpenShift OAuth), and `ldap` (directory password login with backend-issued access tokens). OpenShift and LDAP use Kubernetes impersonation.
- **Multi-cluster support** — Configure any number of clusters; each is health-checked every 30 seconds.
- **Capp CRUD** — Full create / read / update / delete for `Capp` resources across namespaces.
- **Namespace listing** — Returns the namespaces visible to each cluster's credentials.
- **Interactive API docs** — Embedded [Scalar](https://scalar.com/) UI served at `/docs` alongside a raw OpenAPI 3.1 spec at `/openapi.yaml`. Both work in air-gapped environments (assets are compiled into the binary).
- **Observability** — Structured JSON logging (zap), Prometheus metrics (`/metrics`), and optional OTLP tracing.
- **Rate limiting** — Token-bucket rate limiter on all endpoints (configurable; on by default).
- **cappctl CLI** — Command-line client for the API. Supports passthrough and OpenShift authentication, named contexts, token refresh, and `table`/`wide`/`json`/`yaml` output. See [docs/cappctl.md](docs/cappctl.md).
- **capp-mcp server** — Exposes the REST API as [MCP](https://modelcontextprotocol.io/) tools over Streamable HTTP, so LLM agents can inspect and manage clusters, namespaces, Capps, ConfigMaps, and (read-only) Secret metadata. Every tool call runs with the caller's own bearer token against capp-backend, so results are scoped by the same RBAC the API already enforces.

## Prerequisites

- **Go** 1.25 or later (for building from source)
- One or more Kubernetes/OpenShift clusters with the [`container-app-operator`](https://github.com/dana-team/container-app-operator) installed (provides the `rcs.dana.io/v1alpha1` API group)
- For `openshift` auth mode: an OpenShift cluster with an OAuthClient registered, and a service account with `impersonate` permissions on each managed cluster
- For `ldap` auth mode: direct LDAPS directory access, a search account, support for binding as the target user, Ed25519 signing keys, and a service account with `impersonate` permissions on each managed cluster

## Getting Started

### Build from source

```bash
go build -o bin/server ./cmd/server/...
```

### Run

```bash
./bin/server --config config/config.yaml
```

The server listens on port `8080` by default. Scalar configuration can be overridden via environment variables prefixed with `CAPP_` (e.g. `CAPP_SERVER_PORT=9090`). Supply LDAP public verification-key maps through the configuration file.

### Docker

```bash
docker build -f deploy/Dockerfile -t capp-backend .
docker run -p 8080:8080 -v $(pwd)/config/config.yaml:/etc/capp/config.yaml capp-backend --config /etc/capp/config.yaml
```

## Configuration

Configuration is loaded from a YAML file specified with the `--config` flag (e.g. `--config config/config.yaml`). Scalar fields can be overridden by an environment variable — replace dots with underscores and prefix with `CAPP_` (e.g. `auth.mode` → `CAPP_AUTH_MODE`). Supply `auth.ldap.verificationKeys` through mounted YAML; a JSON map in an environment variable is not supported. If `--config` is omitted, no config file is read and only environment variables and built-in defaults are used.

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
  # Mode: passthrough | openshift | ldap
  mode: passthrough

  openshift:
    apiServer: "https://api.my-cluster.example.com:6443"  # External OpenShift API URL
    caCert: ""                # Optional: base64-encoded PEM CA bundle
    clientId: "capp-backend"  # OAuthClient name registered in OpenShift
    # Inject via CAPP_AUTH_OPENSHIFT_CLIENTSECRET.
    clientSecret: ""
    redirectUri: "https://console.example.com/auth/callback"
    scopes: ["user:info", "user:check-access"]
    tokenCacheTTLSeconds: 60  # How long validated tokens are cached

  # Validated only when mode is ldap.
  ldap:
    url: "ldaps://directory.example.com:636"
    caCert: ""                # Base64 PEM CA bundle; empty uses system roots
    bindDN: "CN=capp-reader,OU=Services,DC=example,DC=com"
    # Inject via CAPP_AUTH_LDAP_BINDPASSWORD; do not store a password here.
    userBaseDN: "OU=Users,DC=example,DC=com"
    userFilter: "(sAMAccountName={username})"
    usernameAttribute: "sAMAccountName"
    groupBaseDN: "OU=Groups,DC=example,DC=com"
    groupFilter: "(&(objectClass=group)(member={userDN}))"
    groupNameAttribute: "sAMAccountName"
    timeoutSeconds: 10        # Total directory login deadline
    tokenTTLSeconds: 900      # Access-token lifetime; no refresh token
    issuer: "capp-backend"    # Shared across backend instances
    audience: "capp-api"      # Shared across backend instances
    signingKeyID: "key-2026"
    # Inject PEM Ed25519 private key via CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY.
    verificationKeys: {}      # Additional key ID -> PEM Ed25519 public key

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

gitops:
  enabled: false
  repoURL: "https://github.com/org/capp-gitops.git"
  branch: "main"
  authMethod: "token"    # "token" or "ssh"
  token: ""              # inject via CAPP_GITOPS_TOKEN
  sshKeyPath: ""         # path to SSH private key (when authMethod is "ssh")
  pathPrefix: "sites"    # prefix for values files in the repo
  timeoutSeconds: 20     # timeout for each git sync/delete operation

clusters:
  - name: "local"                 # Used as path parameter in /api/v1/clusters/:cluster
    displayName: "Local Cluster"  # Human-readable label for the UI
    allowedNamespaces: []         # Empty = all namespaces allowed
    gitOpsPath: ""                # Sub-path within gitops.pathPrefix for this cluster
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
  configmaps:
    enabled: true
  secrets:
    enabled: true
```

## Authentication Modes

### `passthrough` (default)

The client's `Authorization: Bearer <token>` header is forwarded directly to each Kubernetes API server. No server-side state is created. Suitable for initial deployment and setups where clients already hold cluster tokens.

### `openshift`

Authenticates users via the OpenShift OAuth server of the cluster where the backend is deployed. In this mode, the backend is fully stateless — it never issues its own JWTs. Instead, OpenShift-managed tokens are used directly.

**Browser flow:** The frontend redirects the user to the OpenShift OAuth authorize endpoint. After consent, the frontend receives an authorization code and exchanges it via `POST /api/v1/auth/openshift/callback` for an OpenShift access token and refresh token.

**Programmatic flow:** Any valid OpenShift bearer token can be passed directly in the `Authorization: Bearer <token>` header. The backend validates it via the Kubernetes TokenReview API.

On every authenticated request, the backend extracts the user's identity (username + groups) from the token and **impersonates** that user when making requests to managed clusters. Each managed cluster must have a service account token configured (`credential.inline.token`) with `impersonate` permissions on `users` and `groups` resources.

Requires `auth.openshift.apiServer`, `auth.openshift.clientId`, `auth.openshift.clientSecret`, `auth.openshift.redirectUri`, and an inline token for every configured cluster.

### `ldap`

Authenticates a username and password through directory search-and-bind. The search account finds exactly one user, then a separate TLS-protected connection verifies that user's password. Only after successful group resolution does the backend issue an EdDSA access JWT. Resource requests verify the token locally without contacting LDAP and impersonate its canonical username and groups using each cluster's configured service-account token. The user's access token is never forwarded to Kubernetes.

**API and web integration:** Send credentials over HTTPS to `POST /api/v1/auth/login`. For example, this illustrative request uses placeholder credentials:

```http
POST /api/v1/auth/login HTTP/1.1
Host: capp.example.com
Content-Type: application/json

{"username":"alice","password":"<user-password>"}
```

A successful response has HTTP 200 and `Cache-Control: no-store`, with this shape:

```json
{"accessToken":"<access-jwt>","refreshToken":"","expiresAt":"2026-10-18T12:15:00Z"}
```

The timestamp is illustrative; expiry is the issuance time plus `tokenTTLSeconds` (default 900 seconds). Web clients must retain the token securely, send `Authorization: Bearer <access-jwt>` on resource requests, and return to login when it expires.

LDAP has no refresh sessions, OAuth browser routes, ADFS SSO, or MFA. `POST /api/v1/auth/refresh` returns HTTP 501 / `NOT_SUPPORTED` even without a body. Raw `cluster`/`token` login fields cannot authenticate an LDAP user.

| Condition | HTTP / error code | Message |
|---|---|---|
| Missing or empty login fields | 400 / `BAD_REQUEST` | `username and password are required` |
| Malformed JSON | 400 / `BAD_REQUEST` | JSON binding error |
| Unknown/ambiguous user or rejected user password | 401 / `UNAUTHORIZED` | `invalid credentials` |
| Directory TLS/network/deadline failure, search-account failure, referrals, or incomplete results | 503 / `INTERNAL_ERROR` | `directory service unavailable` |
| Missing/invalid bearer token | 401 / `UNAUTHORIZED` | `request is not authenticated` |
| Expired bearer token | 401 / `UNAUTHORIZED` | `token has expired` |
| Token issuance/internal failure | 500 / `INTERNAL_ERROR` | `internal server error` |
| Kubernetes denies a resource operation | 403 | Existing authorization error response |

**Directory and RBAC prerequisites:** Use an `ldaps://` endpoint; the default port is 636 when omitted. Certificates and hostnames are verified; StartTLS and plaintext connections are unsupported. `caCert` is a base64 PEM CA bundle; leaving it empty uses system roots. Configure valid search DNs and filters with exactly one `{username}` or `{userDN}` placeholder respectively. Both timeouts and token lifetimes must be positive. The returned username and group attributes must be unambiguous, preserve the case used in Kubernetes bindings, and contain neither surrounding whitespace nor reserved `system:` identities.

**Search bases and referrals:** User or group searches returning LDAP referrals are rejected with HTTP 503 (`directory service unavailable`). AD domain-root searches can return referrals, so choose `userBaseDN` and `groupBaseDN` OU/container bases that cover the required users and groups without returning referrals.

The default group filter covers direct membership only. Nested and primary-group membership are not guaranteed; configure directory-supported filters for the required coverage. An empty group result is valid and does not remove user-based or implicit `system:authenticated` grants. Operators must create matching User/Group RoleBindings on each managed cluster. Every cluster needs `credential.inline.token` with impersonation permissions; kubeconfig-only credentials are not accepted in LDAP mode. The chart's `capp-user` role includes Projects access used by namespace listing, but operators still choose which identities receive that role.

**Keys and rotation:** All replicas need consistent issuer, audience, signing/trust configuration, and synchronized clocks. Use PEM Ed25519 keys and key IDs containing only lowercase ASCII letters, digits, `-`, or `_`. The active public key is derived from the private key. `verificationKeys` maps additional key IDs to public PEM keys in mounted YAML; a conflicting active key is rejected at startup. Tokens identify the signing key with `kid` and include `iss`, `aud`, canonical `sub`, `groups` (including an empty array), `iat`, `exp`, and `token_use: access`. Verification uses only configured local keys and allows no clock-skew leeway.

1. Distribute the new public verification key to all replicas.
2. Activate its private key and `signingKeyID`, retaining the retiring public key.
3. Remove the retiring key only after the last token issued with it has expired.

AD account, password, and group changes affect newly issued tokens. Existing tokens remain usable until expiry unless their signing key is withdrawn across the fleet. Key removal invalidates every token signed by that key after rollout; it is not immediate per-user logout.

**Login-abuse and lockout ownership:** Directory operators own AD account-lockout policy and recovery. The optional backend limiter is per client IP and per instance, not per directory account or fleet-wide. Platform operators provide any distributed gateway controls; this backend adds no distributed limiter. Automation owners must stop on rejected credentials and rotate stale secrets rather than retry bad passwords. Passwords are transient; do not log login bodies, passwords, tokens, private keys, or raw directory diagnostics.

### Adding Clusters (openshift and ldap modes)

To add a new managed cluster in `openshift` or `ldap` mode:

1. **Create a ServiceAccount** on the target cluster with impersonation permissions:
   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: ClusterRole
   metadata:
     name: capp-backend-impersonator
   rules:
     - apiGroups: [""]
       resources: ["users", "groups"]
       verbs: ["impersonate"]
     - apiGroups: ["authentication.k8s.io"]
       # OpenShift only: omit this rule for LDAP.
       resources: ["tokenreviews"]
       verbs: ["create"]
   ---
   apiVersion: rbac.authorization.k8s.io/v1
   kind: ClusterRoleBinding
   metadata:
     name: capp-backend-impersonator
   roleRef:
     apiGroup: rbac.authorization.k8s.io
     kind: ClusterRole
     name: capp-backend-impersonator
   subjects:
     - kind: ServiceAccount
       name: capp-backend
       namespace: capp-platform-system
   ```

2. **Generate a long-lived token** for the ServiceAccount:
   ```bash
   kubectl create token capp-backend -n capp-platform-system --duration=8760h
   ```

3. **Add the cluster** to `config.clusters` and provide the token via the corresponding `secret.clusterTokens[N]` entry or `CAPP_CLUSTERS_N_CREDENTIAL_INLINE_TOKEN` environment variable.

## API Reference

The full OpenAPI 3.1 spec is embedded in the binary and served at runtime:

| Endpoint | Description |
|---|---|
| `GET /docs` | Interactive Scalar API documentation |
| `GET /openapi.yaml` | Raw OpenAPI 3.1 spec |

### Endpoint summary

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/api/v1/auth/mode` | — | Get active auth mode |
| `POST` | `/api/v1/auth/login` | — | Sign in (openshift or ldap mode) or return not-supported (passthrough mode) |
| `POST` | `/api/v1/auth/refresh` | — | Refresh access token (openshift mode); not supported in passthrough or ldap mode |
| `GET` | `/api/v1/auth/openshift/authorize` | — | Get OpenShift OAuth authorize URL (openshift mode) |
| `POST` | `/api/v1/auth/openshift/callback` | — | Exchange OAuth code for tokens (openshift mode) |
| `GET` | `/api/v1/clusters` | ✓ | List configured clusters |
| `GET` | `/api/v1/clusters/:cluster` | ✓ | Get cluster metadata |
| `GET` | `/api/v1/clusters/:cluster/namespaces` | ✓ | List namespaces |
| `POST` | `/api/v1/clusters/:cluster/namespaces` | ✓ | Create a namespace |
| `GET` | `/api/v1/clusters/:cluster/capps` | ✓ | List all Capps across namespaces |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps` | ✓ | List Capps in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps` | ✓ | Create a Capp |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Get a Capp |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Update a Capp |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name` | ✓ | Delete a Capp |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync` | ✓ | Enable Git sync / re-sync |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync` | ✓ | Disable Git sync |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate` | ✓ | Migrate Capp to another cluster/namespace; requires `targetHostname` on copy when source has a hostname |
| `GET` | `/api/v1/clusters/:cluster/configmaps` | ✓ | List all ConfigMaps across namespaces |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/configmaps` | ✓ | List ConfigMaps in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/configmaps` | ✓ | Create a ConfigMap |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name` | ✓ | Get a ConfigMap |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name` | ✓ | Update a ConfigMap |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name` | ✓ | Delete a ConfigMap |
| `GET` | `/api/v1/clusters/:cluster/secrets` | ✓ | List all Secrets across namespaces |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/secrets` | ✓ | List Secrets in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/secrets` | ✓ | Create a Secret |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/secrets/:name` | ✓ | Get a Secret |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/secrets/:name` | ✓ | Update a Secret |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/secrets/:name` | ✓ | Delete a Secret |
| `GET` | `/healthz` | — | Liveness probe |
| `GET` | `/readyz` | — | Readiness probe (healthy when ≥1 cluster is reachable) |
| `GET` | `/metrics` | — | Prometheus metrics (if enabled) |

### Pulling images from a private registry

A Capp can reference image pull secrets by name through `imagePullSecrets`. The backend writes them to the Capp's pod spec (`spec.configurationSpec.template.spec.imagePullSecrets`).

1. Create a `kubernetes.io/dockerconfigjson` Secret in the Capp's namespace. Values are plain text; the backend base64-encodes them:

   ```bash
   curl -X POST "$BACKEND/api/v1/clusters/$CLUSTER/namespaces/$NS/secrets" \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{
       "name": "ghcr-pull-secret",
       "type": "kubernetes.io/dockerconfigjson",
       "data": {".dockerconfigjson": "{\"auths\":{\"ghcr.io\":{\"username\":\"me\",\"password\":\"<token>\",\"auth\":\"<base64 of me:token>\"}}}"}
     }'
   ```

2. Reference it when creating or updating the Capp:

   ```json
   { "name": "my-app", "image": "ghcr.io/myorg/private-app:1.0.0", "imagePullSecrets": ["ghcr-pull-secret"] }
   ```

Notes:

- **Full replace on update.** `PUT` replaces the list, like `env` and volumes. Omit the field or send `[]` to remove all pull secrets.
- **Validation.** Names must be valid DNS-1123 subdomains and unique, otherwise the request fails with 400. The backend doesn't check that the Secret exists or has the right type; a missing Secret shows up later as an image pull error on the revision.
- **Migration and Git sync.** Migrate copies all managed Secrets in the namespace, including pull secrets created through the API. GitOps values files embed the Capp spec, so `imagePullSecrets` is synced too.

## cappctl CLI

`cappctl` is the official CLI for the capp-backend API.

### Build

```bash
make build-cli          # produces bin/cappctl
# or
go build -o bin/cappctl ./cmd/cappctl/
```

### Quick start

```bash
# Log in and save a named context
cappctl login --server https://capp.example.com --context prod --cluster my-cluster --token <token>

# List Capps
cappctl get capps --cluster my-cluster --namespace my-ns

# Create a Capp
cappctl create capps --name my-app --image nginx:latest --cluster my-cluster --namespace my-ns

# Switch context
cappctl context use staging
```

### Auth modes

| Mode | Login command |
|---|---|
| `passthrough` | `--token <raw-token>` |
| `openshift` | `--username <u>` (password prompted) **or** browser flow (URL opened automatically on Linux/Windows) **or** `--token <raw-token>` |

> Auth mode is auto-detected from the server if `--auth-mode` is omitted.

> **OpenShift browser flow:** `cappctl` binds a local callback server on port **18085**. The ServiceAccount used as the OAuth client must have the annotation `serviceaccounts.openshift.io/oauth-redirecturi.cli: "http://localhost:18085/callback"` (set automatically by the Helm chart). Ensure port 18085 is free when running `cappctl login` in OpenShift mode.

### Shell completion

`cappctl` supports tab completion for bash, zsh, fish, and PowerShell. See [docs/cappctl.md](docs/cappctl.md#shell-completion) for setup instructions.

See [docs/cappctl.md](docs/cappctl.md) for the full reference.

## MCP Server (capp-mcp)

`capp-mcp` is a separate binary that exposes capp-backend's REST API as [MCP](https://modelcontextprotocol.io/) tools over Streamable HTTP, so LLM agents can inspect and manage Capp resources. It holds no credentials of its own — every tool call is executed with the bearer token the connecting MCP client supplies (`Authorization: Bearer <token>`), forwarded as-is to capp-backend, which performs all real authentication and authorization. A session with no bearer token fails tool calls with an explicit "no bearer token on this session" error.

### Build & run

```bash
make build-capp-mcp                              # produces bin/capp-mcp
make run-capp-mcp                                 # runs against a local capp-backend on :8080
# or
go build -o bin/capp-mcp ./cmd/mcp/...
./bin/capp-mcp --backend-url http://localhost:8080 --port 8081
```

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--port` | `CAPP_MCP_PORT` | `8081` | Port to listen on |
| `--backend-url` | `CAPP_BACKEND_URL` | `http://localhost:8080` | capp-backend base URL to proxy tool calls to |
| `--insecure` | `CAPP_MCP_INSECURE` | `false` | Skip TLS certificate verification when calling capp-backend |

The server exposes `POST /mcp` (Streamable HTTP transport) and `GET /healthz` (liveness probe). Tool sets can be individually disabled via `mcpserver.Config.Enabled` (see `internal/mcpserver/registry.go`).

### Tool sets

| Tool set | Tools | Notes |
|---|---|---|
| `clusters` | `cluster_list`, `cluster_get` | Discover valid `cluster` values for every other tool |
| `namespaces` | `namespace_list`, `namespace_create`, `namespace_update`, `namespace_add_users` | `namespace_update` is a full replace; use `namespace_add_users` to append without disturbing existing users |
| `capps` | `capp_list`, `capp_get`, `capp_create`, `capp_update`, `capp_delete`, `capp_sync`, `capp_sync_disable` | `capp_update` is a full replace, not a patch — call `capp_get` first and send back the full desired state |
| `configmaps` | `configmap_list`, `configmap_get`, `configmap_create`, `configmap_update`, `configmap_delete` | Only ConfigMaps labeled `dana.io/capp-managed=true` are visible |
| `secrets` | `secret_list`, `secret_get` | **Read-only.** Returns metadata only (name, type, labels, data key names) — secret values are never exposed |

### Docker & Helm

```bash
make docker-build-capp-mcp    # builds ghcr.io/dana-team/capp-mcp:latest from deploy/Dockerfile.capp-mcp
```

A Helm chart is provided at [helm/capp-mcp](helm/capp-mcp), deploying `capp-mcp` as a Knative-autoscaled Capp (via the `container-app-operator`) alongside capp-backend. Key values:

- `backend.url` — the capp-backend URL to proxy to (defaults to the in-cluster service)
- `scaleSpec.minReplicas` — keep at least `1` warm instance if agents hold long-lived sessions, since scaling to zero mid-session drops any in-flight call
- `route.enabled` / `route.hostname` — expose the server externally over HTTPS for remote MCP clients; disabled by default (cluster-internal only)

## Project Structure

```
cmd/
├── server/         # Server entry point — wires config, auth, clusters, and HTTP server
├── cappctl/        # CLI entry point
└── mcp/            # capp-mcp entry point
api/                # OpenAPI 3.1 spec (embedded in binary via go:embed)
config/             # Default config.yaml
deploy/             # Dockerfiles, Kubernetes manifests, Helm chart skeleton
docs/               # Documentation (cappctl.md, openshift-auth.md)
internal/
├── apierrors/      # Canonical error types and Gin response helpers
├── auth/           # Auth manager interface + passthrough, openshift, ldap implementations
├── cli/            # cappctl CLI packages (no Gin/K8s imports)
│   ├── auth/       # login, logout, context commands
│   ├── capps/      # Capp CRUD commands
│   ├── client/     # HTTP client with token auth and TLS options
│   ├── config/     # ~/.config/cappctl/config.yaml read/write
│   ├── output/     # table/wide/json/yaml renderer
│   ├── resource/   # ResourceCommand interface + registry
│   └── root/       # Root Cobra command, PersistentPreRunE, token refresh
├── cluster/        # ClusterManager — multi-cluster routing and health checks
├── config/         # Config structs, Viper loading, and validation
├── gitops/         # GitOps client — clone, commit, push per-capp values files
├── mcpserver/      # capp-mcp: MCP tool sets (clusters, namespaces, capps, configmaps, secrets), auth middleware
├── middleware/      # Gin middleware: auth, cluster resolution, CORS, logging, metrics, rate limiting
├── resources/      # Resource handler registry
│   ├── cluster/
│   │   └── namespaces/  # Namespace list + create handler
│   └── namespaced/
│       ├── capps/       # Capp CRUD, sync, and migrate handler
│       ├── configmaps/  # ConfigMap CRUD handler
│       └── secrets/     # Secret CRUD handler
└── server/         # Gin engine setup, route registration, auth endpoints
pkg/k8s/            # Kubernetes scheme builder (registers CRD types)
helm/
├── capp-backend/   # Helm chart for the server
├── capp-mcp/       # Helm chart for the capp-mcp server
└── capp-template/  # Generic Helm chart for deploying a single Capp resource
```

## Kubernetes Deployment

A reference `Deployment`, `Service`, `Secret`, and `ConfigMap` are provided in `deploy/deployment.yaml` and `deploy/configmap.yaml`.

The container runs as a non-root user (`UID 65532`) with a read-only root filesystem. Liveness and readiness probes are pre-configured on `/healthz` and `/readyz`.

Sensitive values (cluster tokens, OpenShift client secret, LDAP search password and signing private key) should be provided via Kubernetes Secrets and injected as environment variables:

```bash
CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN=<sa-token>
CAPP_AUTH_OPENSHIFT_CLIENTSECRET=<client-secret>
CAPP_AUTH_LDAP_BINDPASSWORD='<search-account-password>'
CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY='<PEM-Ed25519-private-key>'
```

For Helm, set `config.auth.mode=ldap` and configure directory settings and public verification keys under `config.auth.ldap`. Supply `secret.ldapBindPassword` and `secret.ldapSigningPrivateKey` at install time (use `--set-file secret.ldapSigningPrivateKey=/path/to/key.pem` for the PEM file). Both Deployment and Capp workloads inject these through Secret references; neither private value is rendered into the ConfigMap. Provide per-cluster tokens through `secret.clusterTokens`. The chart creates and binds the configured service account for OpenShift and LDAP; OAuth annotations and TokenReview permissions remain OpenShift-only. Use an HTTPS ingress or route for password login.

> **Note:** In `openshift` mode, the backend is fully stateless — no sessions are stored. Multiple replicas work without shared state. Token validation results are cached in-memory per pod.

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
