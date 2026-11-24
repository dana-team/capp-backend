# capp-backend

`capp-backend` is the REST API server for the [Capp Console](https://github.com/dana-team/capp-frontend). It sits between the frontend and one or more Kubernetes/OpenShift clusters, handling authentication, cluster routing, and lifecycle management of [**Capp**](https://github.com/dana-team/container-app-operator) (ContainerApp) custom resources.

## Features

- **Pluggable authentication** — Three modes: `passthrough`, `openshift` (OpenShift OAuth), and `ldap` (directory password login with backend-issued access tokens). All three support Kubernetes or backend-policy authorization.
- **Multi-cluster support** — Configure any number of clusters; each is health-checked every 30 seconds.
- **Capp CRUD** — Full create / read / update / delete for `Capp` resources across namespaces.
- **Namespace listing** — Returns namespaces according to the active authorization mode and configured namespace boundary.
- **Interactive API docs** — Embedded [Scalar](https://scalar.com/) UI served at `/docs` alongside a raw OpenAPI 3.1 spec at `/openapi.yaml`. Both work in air-gapped environments (assets are compiled into the binary).
- **Observability** — Structured JSON logging (zap), Prometheus metrics (`/metrics`), and optional OTLP tracing.
- **Rate limiting** — Token-bucket rate limiter on all endpoints (configurable; on by default).
- **cappctl CLI** — Command-line client for the API. Supports passthrough, OpenShift, and LDAP authentication, named contexts, OpenShift token refresh, LDAP expiry checks, and `table`/`wide`/`json`/`yaml` output. See [docs/cappctl.md](docs/cappctl.md).
- **capp-mcp server** — Exposes the REST API as [MCP](https://modelcontextprotocol.io/) tools over Streamable HTTP, so LLM agents can inspect and manage clusters, namespaces, Capps, ConfigMaps, and (read-only) Secret metadata. Every tool call runs with the caller's own bearer token against capp-backend, so results are scoped by the same authorization rules the API enforces.

## Prerequisites

- **Go** 1.25 or later (for building from source)
- One or more Kubernetes/OpenShift clusters with the [`container-app-operator`](https://github.com/dana-team/container-app-operator) installed (provides the `rcs.dana.io/v1alpha1` API group)
- For `openshift` auth mode: an OpenShift cluster with an OAuthClient registered and home-cluster TokenReview permission; target service accounts need impersonation permissions in Kubernetes mode or consolidated backend permissions in backend-policy mode
- For `ldap` auth mode: direct LDAPS directory access, a search account, support for binding as the target user, Ed25519 signing keys, and per-cluster system credentials (impersonation permissions in Kubernetes mode; consolidated backend permissions in backend-policy mode)

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

### Authorization mode

`authorization.mode` defaults to `kubernetes`. With LDAP, OpenShift or passthrough authentication, `backend-policy` uses application viewer/editor assignments and configured namespace operators, with one backend credential per cluster and no user impersonation. See the [adoption and operations guide](docs/backend-policy.md) for configuration, Secret-read access, infrastructure permissions, namespace API examples, and manual cutover/rollback. This describes the implementation in this source tree, not release availability or completed deployment validation.

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

authorization:
  mode: kubernetes  # kubernetes | backend-policy (all three auth modes)
  restrictedBindingNamePrefixes: ["system:"]  # Required in backend-policy; [] disables prefix restrictions

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
    allowedNamespaces: []         # Empty = no extra name filter; mode-specific authorization still applies
    gitOpsPath: ""                # Sub-path within gitops.pathPrefix for this cluster
    credential:
      # Option A: kubeconfig file (Kubernetes-mode passthrough)
      kubeconfigPath: "/home/user/.kube/config"
      # Option B: inline credentials (remove kubeconfigPath when using this option)
      inline:
        apiServer: "https://my-cluster:6443"
        caCert: "<base64-encoded PEM>"
        token: ""  # Inject via CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN
        insecure: false

resources:
  namespaces:
    enabled: true
  capps:
    enabled: true
    maxLogStreams: 100   # open log streams per instance; more get 503
  configmaps:
    enabled: true
  secrets:
    enabled: true
```

## Authentication Modes

### `passthrough` (default)

Clients supply their existing `Authorization: Bearer <token>` header; login and refresh remain unsupported. In Kubernetes authorization mode, the token is forwarded to the Kubernetes API for resource access. In backend-policy mode, each requested target verifies it with TokenReview before policy checks. The backend identity authenticates both review and resource calls; the caller token appears only in the review body. A token may resolve to different usernames/groups at different targets, and review results are reused only within that HTTP request. In backend-policy mode, each target requires inline backend credentials, an HTTPS API endpoint, a valid base64 PEM `caCert`, and `insecure: false`; remove `kubeconfigPath`. See [verification and discovery errors](docs/backend-policy.md#provider-verification-and-discovery).

### `openshift`

Authenticates users via the OpenShift OAuth server of the cluster where the backend is deployed. In this mode, the backend is fully stateless — it never issues its own JWTs. Instead, OpenShift-managed tokens are used directly.

**Browser flow:** The frontend redirects the user to the OpenShift OAuth authorize endpoint. After consent, the frontend receives an authorization code and exchanges it via `POST /api/v1/auth/openshift/callback` for an OpenShift access token and refresh token.

**Programmatic flow:** Any valid OpenShift bearer token can be passed directly in the `Authorization: Bearer <token>` header. The backend validates it via the Kubernetes TokenReview API.

The backend verifies the token with its in-cluster home TokenReview service, preserving the returned username and groups. Successful results are cached for `auth.openshift.tokenCacheTTLSeconds`; execution targets never review the caller token. Kubernetes authorization mode impersonates the verified user using each target's service account, which needs `impersonate` on `users` and `groups`. Backend-policy mode instead checks assignments and executes with each target's backend token, without impersonation. The workload service account needs home-cluster TokenReview permission in either mode. Confirm that the configured OAuth server and home reviewer represent the intended authority.

Requires `auth.openshift.apiServer`, `auth.openshift.clientId`, `auth.openshift.clientSecret`, `auth.openshift.redirectUri`, and an inline token for every configured cluster.

### `ldap`

Authenticates a username and password through directory search-and-bind. The search account finds exactly one user, then a separate TLS-protected connection verifies that user's password. Only after successful group resolution does the backend issue an EdDSA access JWT. Resource requests verify the token locally without contacting LDAP. Kubernetes authorization mode impersonates its canonical username and groups using each cluster's configured service-account token; backend-policy mode checks application assignments and uses the configured backend service-account token. The user's access token is never forwarded to Kubernetes.

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

**Directory and RBAC prerequisites:** Use an `ldaps://` endpoint; the default port is 636 when omitted. Certificates and hostnames are verified; StartTLS and plaintext connections are unsupported. `caCert` is a base64 PEM CA bundle; leaving it empty uses system roots. Configure valid search DNs and filters with exactly one `{username}` or `{userDN}` placeholder respectively. Both timeouts and token lifetimes must be positive. The returned username and group attributes must be unambiguous, preserve the case used in Kubernetes or application policy bindings, and contain neither surrounding whitespace nor reserved `system:` identities.

**Search bases and referrals:** User or group searches returning LDAP referrals are rejected with HTTP 503 (`directory service unavailable`). AD domain-root searches can return referrals, so choose `userBaseDN` and `groupBaseDN` OU/container bases that cover the required users and groups without returning referrals.

The default group filter covers direct membership only. Nested and primary-group membership are not guaranteed; configure directory-supported filters for the required coverage. An empty group result is valid. In Kubernetes authorization mode, user-based or implicit `system:authenticated` grants still apply, and operators must create matching User/Group RoleBindings on each managed cluster. Every LDAP cluster needs `credential.inline.token`; kubeconfig-only credentials are not accepted. That token needs impersonation permissions only in Kubernetes authorization mode. In backend-policy mode, configure the consolidated backend role and binding as described in the [guide](docs/backend-policy.md). The chart's `capp-user` role includes Projects access used by namespace listing, but operators still choose which identities receive that role.

**Keys and rotation:** All replicas need consistent issuer, audience, signing/trust configuration, and synchronized clocks. Use PEM Ed25519 keys and key IDs containing only lowercase ASCII letters, digits, `-`, or `_`. The active public key is derived from the private key. `verificationKeys` maps additional key IDs to public PEM keys in mounted YAML; a conflicting active key is rejected at startup. Tokens identify the signing key with `kid` and include `iss`, `aud`, canonical `sub`, `groups` (including an empty array), `iat`, `exp`, and `token_use: access`. Verification uses only configured local keys and allows no clock-skew leeway.

1. Distribute the new public verification key to all replicas.
2. Activate its private key and `signingKeyID`, retaining the retiring public key.
3. Remove the retiring key only after the last token issued with it has expired.

AD account, password, and group changes affect newly issued tokens. Existing tokens remain usable until expiry unless their signing key is withdrawn across the fleet. Key removal invalidates every token signed by that key after rollout; it is not immediate per-user logout.

**Login-abuse and lockout ownership:** Directory operators own AD account-lockout policy and recovery. The optional backend limiter is per client IP and per instance, not per directory account or fleet-wide. Platform operators provide any distributed gateway controls; this backend adds no distributed limiter. Automation owners must stop on rejected credentials and rotate stale secrets rather than retry bad passwords. Passwords are transient; do not log login bodies, passwords, tokens, private keys, or raw directory diagnostics.

### Adding Clusters (openshift and ldap modes)

The steps below apply to **Kubernetes authorization mode** with `openshift` or `ldap` authentication. For backend-policy, use [infrastructure onboarding](docs/backend-policy.md#infrastructure-onboarding-and-rotation).

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
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/logs` | ✓ | Live logs of the Capp's pods as Server-Sent Events (`?pod=`, `?container=`, `?tailLines=`, or `?podSinceTime=<pod>=<time>` per pod plus `?sinceTime=` to resume); Kubernetes authorization mode only, denied in backend-policy mode |
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
| `GET` | `/api/v1/cfunc/flavors` | — | List the configured cfunc flavor names (only when `resources.cfunc.enabled`) |
| `GET` | `/api/v1/cfunc/sizes` | — | List the configured cfunc size names (only when `resources.cfunc.enabled`) |
| `GET` | `/api/v1/clusters/:cluster/cfunc/pools` | ✓ | List all cfunc pools across namespaces |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools` | ✓ | List pools in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools` | ✓ | Create a pool from a flavor and a size |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name` | ✓ | Get a pool |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name` | ✓ | Update a pool |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name` | ✓ | Delete a pool; 409 while Functions in the namespace use it |
| `GET` | `/api/v1/clusters/:cluster/cfunc/functions` | ✓ | List all cfunc functions across namespaces (function routes exist only when `resources.cfunc.functions.enabled`) |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions` | ✓ | List functions in a namespace |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions` | ✓ | Create a function with its triggers from a git repository |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name` | ✓ | Get a function with its triggers, deployed commit, build state and build log |
| `PUT` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name` | ✓ | Update a function: clones again, replaces the triggers |
| `DELETE` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name` | ✓ | Delete a function with its package and triggers |
| `POST` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/test` | ✓ | Call the function one time through one of its http triggers; 501 without `cfuncRouterURL` |
| `GET` | `/api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/logs` | ✓ | Recent log lines of the pods that serve the function now |
| `GET` | `/healthz` | — | Liveness probe |
| `GET` | `/readyz` | — | Readiness probe (≥1 healthy cluster; backend-policy also requires its initial policy/namespace sync) |
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

### cfunc (function as a service)

Disabled by default; set `resources.cfunc.enabled: true` on a backend whose clusters run [Fission](https://fission.io). A **pool** is a set of warm pods of one language runtime that functions run in. It is stored on the cluster as a Fission `Environment`.

A client never names a runtime image. It sends a **flavor** (the language) and a **size** (the CPU and memory of each warm pod); the backend configuration holds what is behind them:

```yaml
resources:
  cfunc:
    enabled: true
    registryPrefix: "ghcr.io/fission"   # joined with each image as <prefix>/<image>
    imagePullSecret: ""                 # Secret name; must exist in each namespace that holds a pool
    allowCustomBuild: false             # true lets a request name its own build image and command
    maxPoolSize: 10                     # highest poolSize a request may ask for
    defaultSize: small
    sizes:
      small:
        requests: {cpu: "100m", memory: "128Mi"}
        limits:   {cpu: "200m", memory: "256Mi"}
    flavors:
      python:
        image: "python-env:1.35.0"
        buildImage: "python-builder:1.35.0"
        builderEnv:
          - name: PIP_INDEX_URL
            value: "https://registry.example.internal/pypi/simple"
      java:
        image: "jvm-env:1.32.0"
        buildImage: "jvm-builder:1.32.0"
        keepArchive: true
```

The Helm chart ships no sizes and no flavors: set `config.resources.cfunc.sizes`, `flavors` and `defaultSize` yourself. A flavor image must speak the Fission v2 interface; the v1-only `perl-env` starts warm pods but cannot load a function. The server does not start with a flavor image that is not a valid reference, a flavor or size name that contains a dot, a negative quantity, or a build command without a build image.

```bash
curl "$BACKEND/api/v1/cfunc/flavors"   # {"items":["java","python"],"total":2}
curl "$BACKEND/api/v1/cfunc/sizes"     # {"items":["small"],"total":1}

curl -X POST "$BACKEND/api/v1/clusters/$CLUSTER/namespaces/$NS/cfunc/pools" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name": "my-python", "flavor": "python", "size": "small", "poolSize": 3}'
```

- **`poolSize`** is the number of warm pods kept ready (default 3, at most `maxPoolSize`). Idle cost is `poolSize` × the size's requests. `0` keeps no warm pod.
- **`env`** is an optional list of environment variables for the warm pods, in the same shape as a Capp's `env`: `{"name": "LOG_LEVEL", "value": "debug"}` or a `valueFrom` with `secretKeyRef` or `configMapKeyRef`. The pods are shared, so every function that runs in the pool sees them. `PUT` replaces the list; omit it to remove all.
- **`advanced`** is an optional block for the few clients that need it. `resources` lays over the size's CPU and memory: a value it leaves out comes from the size, so a pool always has all four, and no value may exceed the largest configured size. `buildImage` and `buildCommand` replace the flavor's build settings, but only when the backend sets `allowCustomBuild: true`; it is off by default, because anyone who can create a pool could otherwise make the Fission builder run any image and command. `advanced.buildImage` is a full image reference and is used as written.
- **Full replace on update.** `PUT` takes the same body as `POST` (the name comes from the URL); an omitted field falls back to its default. It also applies the flavor's current configuration, so it is how an existing pool picks up a new image tag.
- **A response can be sent back.** When a pool has custom resources or build settings, the response carries them in an `advanced` block, so a client that reads a pool, changes one field and sends it back keeps them.
- **Nothing is stored about the flavor or size.** A read reports them by matching the pool's runtime image and resources against the configuration, so both are empty for an Environment created outside this API, and the flavor is empty once the configuration moves it to another image.
- **Environments created outside this API.** A `PUT` keeps the fields this API does not expose, such as pod specs, and keeps an environment variable it cannot express (a `fieldRef`), which `GET` leaves out. A stored grace period of 0 becomes 360. An Environment with interface version 1 or 2 can be read and deleted but not updated (409): Fission ignores the pool size below version 3 and does not let the version change.
- **`DELETE`** returns 409 while a Function in the same namespace uses the pool. The check is partial: a Function in another namespace can name the pool and is not seen, so deleting the pool breaks it.
- A cluster without the Fission CRDs answers 501 `NOT_SUPPORTED`.
- **Known Fission limit with `tenancy.mode=dynamic` (measured on v1.27.0).** In a namespace that was onboarded while Fission runs, Fission does not reuse a function's pod: every request takes a new pod from the pool, and the pods stay until the idle time passes. A function that gets more requests in that time than its `concurrency` answers 429, and the pool is drained and refilled all the time. In a namespace Fission knew at start (for example `default`), twelve requests in a row used one pod. The cause is in Fission's executor, which checks a pod through a cache of the namespaces known at start. Until Fission fixes it, prefer `tenancy.mode=cluster`, or `static` with the namespaces listed, for clusters with real traffic, and leave `onboardNamespaces` off there. `cluster` mode was not tested here.
- **`onboardNamespaces: true`** makes the first pool of a namespace set the label `fission.io/enabled=true` on that namespace, which is how a Fission that runs with `tenancy.mode=dynamic` starts to serve it. The caller's credential is tried first. When it is refused, the backend's own cluster credential is used, but only for a caller who may create pools in that namespace (a SelfSubjectAccessReview), so one of the two needs `patch` on namespaces. The label is never removed.

#### Functions

A **function** is code from a git repository that runs in a pool, together with the triggers that call it. Set `resources.cfunc.functions.enabled: true`, the git server and its token:

```yaml
resources:
  cfunc:
    functions:
      enabled: true
      git:
        baseURL: "https://gitlab.example.com"   # token: CAPP_RESOURCES_CFUNC_FUNCTIONS_GIT_TOKEN
clusters:
  - name: prod
    cfuncRouterURL: "https://fn.example.com"    # optional: the Fission router of this cluster
```

```bash
curl -X POST "$BACKEND/api/v1/clusters/$CLUSTER/namespaces/$NS/cfunc/functions" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
        "name": "shop", "pool": "py",
        "source": {"url": "https://gitlab.example.com/team-a/functions.git", "ref": "main", "path": "shop"},
        "entrypoint": "main.handle",
        "secretVolumes": [{"secretName": "shop-db"}],
        "triggers": [
          {"name": "api", "type": "http", "path": "/orders", "methods": ["GET", "POST"]},
          {"name": "nightly", "type": "cron", "schedule": "0 2 * * *"}
        ]
      }'
```

- **One object, several Fission objects.** The backend writes a `Function`, the `Package` that holds its code, and one `HTTPTrigger` or `TimeTrigger` for each trigger, and deletes them together. The function always runs on Fission's pool manager, in a **pool of its own namespace**; it takes its runtime, CPU, memory and environment variables from that pool.
- **`source`** names a repository under `git.baseURL`, an optional `ref` (branch, tag or full commit hash; default: the default branch) and an optional `path` (a folder or one file; default: the repository root). The backend clones with its own token, because Fission cannot clone a private repository, and stores the code inline in the Package, so the code is limited to **256 KiB** as a zip archive. A client never sends a token, and the token is sent to `git.baseURL` only. The response shows the `commit` that is deployed. **`source.build`** says whether Fission builds the code: by default a folder is built when the pool has a build image, and one file is loaded as it is. A compiled language needs `"build": true` also for one file (`go/hello-world/hello.go`). The archive keeps the executable bit of a file, so a `build.sh` in the folder can be the `advanced.buildCommand`. A repository that does not exist, or that the token cannot read, answers 400.
- **`entrypoint`** is the function inside the code that the runtime calls. Its form depends on the flavor: `main.handle` (python), `handler` (ruby), `Handler` (go), `io.fission.HelloWorld` (java).
- **`triggers`** is one list; `type` is `http` or `cron`. An http trigger has a `path` and `methods` (default `GET`). The backend serves it under **`/<namespace>/<function><path>`**, because the Fission router has one URL space for the whole cluster and two namespaces could otherwise claim the same path; the response shows `fullPath` and, with `cfuncRouterURL`, the `url`. A cron trigger has a `schedule` (five fields, six with seconds, or `@hourly`, `@every 5m`) that must not run more often than `minCronIntervalSeconds`. Each trigger reports the `status` Fission gives it, such as `RouteAdmitted`, `RouteConflict` or `CronRegistered`. Triggers that already name the function, for example ones made with the `fission` CLI, belong to it: they are listed, and a create or update replaces them. When two of them would get the same name in a response, the one this API did not name gets a number (`api-2`), and the next `PUT` stores it under that name.
- **`secretVolumes`** and **`configMapVolumes`** are the Capp types. Create the Secret or ConfigMap through this API first, then name it. One difference from a Capp: Fission fixes the path, `/secrets/<namespace>/<name>/<key>` and `/configs/<namespace>/<name>/<key>`, so `name` and `mountPath` are optional and, when sent, must equal what a response shows. Deleting a Secret or ConfigMap answers 409 while a function reads it, and its `/bound` route lists those functions. A caller who may not list Fission Functions gets no such check and an empty `boundFunctions`: a Role with Secrets and ConfigMaps only keeps working as before.
- **`advanced`** holds `concurrency` (default `defaultConcurrency`, at most `maxConcurrency`; both are 500, Fission's own default, and a call answers 429 when the limit is reached), `requestsPerPod`, `idleTimeoutSeconds`, `onceOnly`, `retainPods`, `specializationTimeoutSeconds` and, only with `allowCustomBuild`, `buildCommand`.
- **Full replace on update.** `PUT` clones again, so it is also how new code of a branch is deployed; an unchanged commit starts no build. A trigger the body leaves out is deleted. A `GET` response can be sent back as the body.
- **Limits a client can hit.** Two http triggers of one function cannot serve the same method on the same path (400). The 256 KiB limit applies to the bytes that are stored: the zip for code that is built, the file itself for one file that is not. A Package of the function's name that was not created through this API is never replaced (409) and never deleted with the function. The backend clones at most 4 repositories at one time. That bounds the count, not the size: each clone is read into memory in full (the whole tree of a branch or tag, the whole history for a commit hash), so one clone is as large as its repository. Size the backend's memory for the largest repository under `git.baseURL`, or point `git.baseURL` at a group that holds only function repositories. A request that gets no free slot within the 60-second fetch time answers 503 `CFUNC_SOURCE_BUSY` and changes nothing; four slow clones can therefore hold back every other function create and update for up to that time.
- **Slow requests.** A create, an update and a test call can take longer than `server.writeTimeoutSeconds` (a clone has 60 seconds, a test call the function's timeout plus 10). The handler moves the write deadline of that one response, so the client still gets its answer; a proxy in front of the backend needs a timeout at least as long.
- **Build state.** `build.status` is `pending`, `running`, `succeeded`, `failed`, or `none` when the code needs no build, and `build.log` is the builder's output. A build runs after the request returns: read the function again. A `PUT` of a function whose build failed asks for the build again, also with the same code. The backend does not point the function at built code; Fission's builder does when the build succeeds, and the function then serves the new code. **A build that fails takes the function down** (measured on Fission v1.27.0): Fission drops the function's pods when it records the result, and a new pod cannot load a package whose build failed, so calls answer 500 until a build succeeds. A new pod also cannot load the package while a build runs. Fix the code and send the `PUT` again; check `build.status` before you rely on a change.
- **Failure and retry.** A request is validated, then the repository is cloned, and only then is anything written, in the order package, function, triggers. A failure after the function is saved answers 500 and says so; the same body in a `PUT` finishes the rest. A git server that cannot be reached answers 502 `CFUNC_SOURCE_FAILED` and changes nothing.
- **`test`** sends one request through an http trigger to the router at `cfuncRouterURL` and returns the status, headers and body. It never calls another address and does not follow redirects. A function with only cron triggers cannot be called this way.
- **`logs`** returns the end of the log of each pod that serves the function now (`?lines=`, default 100). A pod whose log cannot be read at that moment, for example one that was just removed, is in the answer with no lines. Fission takes an idle pod away after `idleTimeoutSeconds` and the log goes with it, so this shows recent runs only.

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
| `ldap` | `--username <u> --password-stdin` **or** `--token <access-jwt>` with a future numeric `exp`; verified HTTPS required |

> Auth mode is auto-detected from the server if `--auth-mode` is omitted.

LDAP automation can use a dedicated AD account and a secret-store mount without a prompt or browser:

```bash
cappctl login --server https://capp.example.com --context ldap-ci \
  --auth-mode ldap --username capp-automation --password-stdin \
  --cluster my-cluster --namespace my-ns < /run/secrets/capp-ldap-password
```

LDAP stdin must be valid UTF-8 and at most 16 KiB before removing one final LF/CRLF. LDAP login rejects password arguments, `--insecure`, and credential-request redirects. For private backend CAs, [install the CA in the CLI runtime's trust store](docs/cappctl.md#backend-ca-trust); `auth.ldap.caCert` configures the separate backend-to-directory connection.

The 0600 context file stores the LDAP access token and expiry, with no password or refresh credentials. Token import parses expiry as untrusted metadata; the backend authenticates the token on use. At known expiry, resource commands and `mcp-headers` fail with `LDAP access token expired; run cappctl login again`, and the header helper emits no headers. Repeat login explicitly from the secret store; stop on rejected credentials and rotate stale secrets rather than retrying them. Flag/environment token overrides retain precedence over context tokens. See the [LDAP CLI guide](docs/cappctl.md#ldap) for conflicts, import rules, and automation details.

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
| `capps` | `capp_list`, `capp_get`, `capp_create`, `capp_update`, `capp_delete`, `capp_sync`, `capp_sync_disable`, `capp_rollout`, `capp_rollout_strategy_remove` | `capp_update` is a full replace, not a patch — call `capp_get` first and send back the full desired state. The exception is `routeSpec.rolloutStrategy`, which only `capp_rollout_strategy_remove` removes, and that moves live traffic |
| `configmaps` | `configmap_list`, `configmap_get`, `configmap_create`, `configmap_update`, `configmap_delete`, `configmap_bound` | Only ConfigMaps labeled `dana.io/capp-managed=true` are visible. `configmap_bound` lists the Capps that reference one (deletion is refused while any do) and which of them restart on update |
| `secrets` | `secret_list`, `secret_get`, `secret_bound` | **Read-only.** Returns metadata only (name, type, labels, data key names) — secret values are never exposed. `secret_bound` lists the Capps that reference a Secret and which of them restart on update |

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
│       ├── secrets/     # Secret CRUD handler
│       ├── cfunc/pools/ # cfunc pool CRUD handler (Fission Environments; flavors and sizes from config)
│       └── cfunc/functions/ # cfunc function CRUD, test call and logs (Fission Function + Package + triggers)
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
