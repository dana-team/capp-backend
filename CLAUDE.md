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
make build-capp-mcp  # Build capp-mcp binary → bin/capp-mcp
make run-capp-mcp    # Run capp-mcp against a local backend on :8080
make fetch-ui        # Re-download the embedded Scalar bundle (internal/server/ui/scalar.min.js)

go test -race -run TestConvertCoverage ./internal/resources/namespaced/capps/   # single test
```

`make build`/`run`/`build-cli`/`build-capp-mcp` run `fmt` and `vet` first. CI is split by path into three workflows: `test.yaml` (server; vets, lints and tests everything **except** `internal/cli`, `cmd/cappctl`, `internal/mcpserver`, `cmd/mcp`, plus `helm lint` and a `go mod tidy` diff check), `test-cli.yaml` (`./internal/cli/... ./cmd/cappctl/...`, also cross-builds Windows), and `test-capp-mcp.yaml` (`./internal/mcpserver/... ./cmd/mcp/...`). `make test` runs all of them locally.

## Architecture

Multi-cluster API gateway for Capp resources, built with **Go + Gin**.

### Request Flow
```
Request → Recovery → Logging → Metrics → CORS → ClientVersion → RateLimit → Auth → ClusterResolution → ResourceHandler
```

1. **Auth middleware** validates the token (mode-dependent) and attaches a credential to context
2. **Cluster middleware** resolves `:cluster` param, checks health, builds a per-request scoped K8s client with the user's credential, validates namespace against allowed list. It also sets an admin client (`AdminK8sClientKey`, the cluster's own SA credential with no impersonation) for discovery-style listing such as namespaces. Context keys live in `internal/middleware/keys.go`.
3. **Resource handler** performs the CRUD operation using the scoped client

Handler tests use `internal/testutil` (fake controller-runtime clients with SAR allow/deny variants, `EngineHelper` for HTTP calls, and mock Auth/Cluster managers) instead of real clusters.

### API Routes

> **Rule:** Every route change or addition must also update `api/openapi.yaml`. Use the `gin-openapi-sync` skill to regenerate it. No test checks the spec against the registered routes, so drift is only caught in review.

```
/healthz, /readyz, /metrics, /docs, /openapi.yaml     (no auth)
GET    /api/v1/sizes                                   (no auth) — t-shirt size presets from config `cappSizes`

GET    /api/v1/auth/mode                               (no auth)
POST   /api/v1/auth/login                              (no auth)
POST   /api/v1/auth/refresh                            (no auth)
GET    /api/v1/auth/openshift/authorize                (no auth, openshift mode only)
POST   /api/v1/auth/openshift/callback                 (no auth, openshift mode only)
GET    /api/v1/clusters[/:cluster]                     (auth)

GET    /api/v1/clusters/:cluster/namespaces            (auth + cluster) — lists CAPP-labeled namespaces, filtered by SelfSubjectAccessReview
POST   /api/v1/clusters/:cluster/namespaces            (auth + cluster) — creates namespace with dana.io/capp-ns=true label; response includes canCreate field
GET    /api/v1/clusters/:cluster/namespaces/:namespace (auth + cluster)
PUT    /api/v1/clusters/:cluster/namespaces/:namespace (auth + cluster) — recreates the RoleBinding and ResourceQuota from the body (full replace)
PATCH  /api/v1/clusters/:cluster/namespaces/:namespace (auth + cluster) — updates the RoleBinding only; allowed for anyone who can create Capps in the namespace
DELETE /api/v1/clusters/:cluster/namespaces/:namespace (auth + cluster) — deletes empty namespace; 409 if Capps exist

GET    /api/v1/clusters/:cluster/capps                 (auth + cluster) — list all capps across namespaces
       /api/v1/clusters/:cluster/namespaces/:namespace/capps[/:name]         (CRUD)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync      (auth + cluster) — enable Git sync / re-sync; returns 501 if gitops feature is disabled in backend
DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync      (auth + cluster) — disable Git sync (deletes values file); returns 501 if gitops feature is disabled in backend
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/rollout   (auth + cluster) — promote/abort/retry a staged rollout by setting the rcs.dana.io/rollout annotation; 202, applied asynchronously by the operator; 409 when status shows the action cannot apply
DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/rollout/strategy  (auth + cluster) — remove the rollout strategy; moves live traffic (operator promotes a Ready candidate, otherwise pins to stable)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate  (auth + cluster) — migrate Capp to another cluster/namespace with dependent resources; requires targetHostname on copy when source has a custom hostname
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/metrics            (auth + cluster) — list configured metrics (resources.observability.metrics); 501 if observability disabled
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/metrics/:metric    (auth + cluster) — run a configured metric (single `query` or up to 10 `series` lines, queried in parallel, returned in legend order) with $selector replaced by Capp namespace/name/cluster matchers; instant by default, range with start+end+step; Capp read is the authz check; 502 PROMETHEUS_QUERY_FAILED on upstream errors
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions                     (auth + cluster) — list CappRevisions (label rcs.dana.io/cappName), newest first
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision           (auth + cluster) — get revision by number (spec.revisionNumber, not object name)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision/rollback  (auth + cluster) — apply revision's spec/labels/annotations to live Capp (backup-to-git label kept as live); pushes to Git if synced
       /api/v1/clusters/:cluster/namespaces/:namespace/configmaps[/:name]    (CRUD)
GET    /api/v1/clusters/:cluster/namespaces/:namespace/configmaps/names     (auth + cluster) — list configmap names only
       /api/v1/clusters/:cluster/namespaces/:namespace/secrets[/:name]       (CRUD)
GET    /api/v1/clusters/:cluster/namespaces/:namespace/secrets/names         (auth + cluster) — list secret names only
GET    /api/v1/clusters/:cluster/namespaces/:namespace/{configmaps,secrets}/:name/bound  (auth + cluster) — Capps referencing the resource + the opted-in subset (`toReload`)
GET    /api/v1/clusters/:cluster/configmaps            (auth + cluster) — list all configmaps across namespaces (dana.io/capp-managed=true only)
GET    /api/v1/clusters/:cluster/secrets               (auth + cluster) — list all secrets across namespaces
```

### Key Patterns

**ResourceHandler interface** (`internal/resources/registry.go`): Pluggable handlers register routes and are enabled/disabled via config. Current handlers: `capps`, `capprevisions`, `namespaces`, `configmaps`, `secrets`. To add a new resource type, implement the interface, register it in `cmd/server/main.go`, and add its name to the `enabledResources` map there (plus a `ResourcesConfig` field and default in `internal/config/config.go`). The registry skips only names explicitly set to `false`; a handler missing from the map is always mounted.

**capp-mcp** (`cmd/mcp/`, `internal/mcpserver/`, `helm/capp-mcp/`): A separate binary that exposes the REST API as MCP tools over Streamable HTTP (`POST /mcp`, `GET /healthz`; flags `--port`/`CAPP_MCP_PORT` 8081, `--backend-url`/`CAPP_BACKEND_URL`, `--insecure`). It holds no credentials: `AuthMiddleware` puts the client's bearer token in the request context, and each tool calls capp-backend through the cappctl HTTP client (`internal/cli/client`) using that token, so backend RBAC still applies. Tools are grouped into `ToolSet`s (`tools_*.go`) registered in `server.go`: clusters, namespaces, capps, capprevisions, configmaps, secrets (read-only, metadata only); configmaps and secrets each have a `*_bound` tool for the `/bound` route. A new backend route that agents should reach needs a matching tool here.

**Client version check** (`pkg/clientversion`, `internal/middleware/clientversion.go`): The server and cappctl both take their version from `-X main.version=<tag>` (release workflow, plus `ARG VERSION` in `deploy/Dockerfile`); local builds are `dev`, which disables the check. A client sends `User-Agent: <name>/<version>`, and the backend checks only clients in `clientversion.Known` (currently just cappctl), because other User-Agent versions use unrelated numbering. When a known client is at least one minor release behind the backend, the middleware sets `X-Client-Warning` and increments `capp_backend_outdated_client_requests_total{client,version}` before the chain (so 401/429 responses are covered), then logs `outdated client` after it with `status` and, on authenticated routes in openshift/ldap mode, `user` (`ClusterCredential.ImpersonateUser`; passthrough has only the token, which must never be logged); and cappctl prints that once to stderr. To check another client, declare it in `pkg/clientversion`, add it to `Known`, and have the client send its User-Agent and show the header; the middleware and comparison stay unchanged, and the middleware/server tests loop over `Known`. The metric's `version` label is major.minor, or `vN.x` for an older major, because the User-Agent is caller-controlled and checked before auth; never label it with the raw version. Build CLI clients with `root.State.NewClient`, not `client.New`, so they send the version and print the warning. capp-mcp and shell completion deliberately don't print it.

**AuthManager interface** (`internal/auth/manager.go`): Factory selects implementation based on `auth.mode` config. Each mode implements `Authenticate`, `Login`, `PasswordLogin`, and `Refresh`. Passthrough mode forwards the caller's bearer token as-is. OpenShift mode is fully stateless — it validates tokens via TokenReview and impersonates users on managed clusters. LDAP mode verifies passwords with directory search-and-bind, issues EdDSA access tokens, and verifies them locally for Kubernetes impersonation without a directory call on resource requests. `GET /api/v1/auth/mode` returns the active mode.

**LDAP contracts:** `POST /api/v1/auth/login` accepts `username`/`password` and returns the existing `TokenPair` with an empty `refreshToken` and `Cache-Control: no-store`. Verified LDAPS protects both the search-account connection and the separate target-user bind; complete group resolution is required before issuance. Unknown/ambiguous users and rejected user passwords return 401 / `UNAUTHORIZED` / `invalid credentials`; directory failures return sanitized 503 / `INTERNAL_ERROR` / `directory service unavailable`. Missing/invalid bearer tokens return 401 / `UNAUTHORIZED` / `request is not authenticated`; expired tokens return 401 / `UNAUTHORIZED` / `token has expired`. Raw-token login is unsupported, refresh returns 501 / `NOT_SUPPORTED` without parsing a body, and OAuth routes remain OpenShift-only. Preserve existing-mode behavior and never log credentials, tokens, keys, or raw directory diagnostics.

LDAP clients submit passwords over HTTPS, retain access tokens securely, and log in again on expiry (900 seconds by default). LDAP has no refresh sessions, ADFS SSO, or MFA.

**ClusterManager** (`internal/cluster/manager.go`): Holds connections to all configured clusters, runs health checks every 30s, and creates per-request scoped clients.

**DTO conversion** (`internal/resources/namespaced/capps/convert.go`): Converts between API request/response types and Kubernetes Capp resources. The CLI (`internal/cli/capps/capps.go`) imports these same types directly — no duplication. `TestConvertCoverage` (`drift_test.go`) fails when a new `CappRequest` field is not populated in `fullCappRequest()` and round-tripped through `ToK8s`/`FromK8s`, so every new field must be wired both ways.

List fields are full-replace on update (`PUT`): `env`, `volumeMounts`, `secretVolumes`, `configMapVolumes`, `nfsVolumes` and `imagePullSecrets` are rebuilt from the request, so omitting one clears it. `imagePullSecrets` (`[]string` of Secret names) maps to the pod spec's `imagePullSecrets`; names are validated as DNS-1123 subdomains and must be unique, but Secret existence/type is not checked (same as env and volume refs).

**Reload on Secret/ConfigMap change** (`internal/resources/namespaced/boundcapps`): `reloadOnBoundResourceChange` maps to the `rcs.dana.io/rollout-on-bound-resource-change` annotation (omitting it on `PUT` clears it). On every ConfigMap/Secret `PUT`, opted-in Capps that reference it (env, envFrom, volumes, projected volumes, imagePullSecrets) get the resource's `resourceVersion` recorded in the JSON annotation `capp.dana.io/bound-resource-versions` (`{"Secret/db":"123"}`, plus informational `restarted-at`/`restart-reason`), via `Update` with conflict retry. Restarts are keyed on resourceVersion, not a timestamp or a data diff: each change restarts exactly once, even two in the same second, and repeating a PUT whose restarts partly failed (500) restarts only the Capps still behind. An unchanged PUT skips the resource write so its resourceVersion stays put. Those keys must stay outside `rcs.dana.io/`: the operator drops `rcs.dana.io/*` Capp annotations when building the Knative template, so only a non-prefixed key creates a new revision. `DELETE` returns 409 while any Capp references the resource; for Secrets that also covers `logSpec.passwordSecret` and Kafka source `secretRef`, which are bound but never reloaded (the operator, not the pods, reads them).

`routeSpec.rolloutStrategy` is the exception to full replace: omitting it on `PUT` keeps the current strategy, because removing one mid-rollout makes the operator promote the candidate or pin traffic. Only `DELETE .../rollout/strategy` removes it. Stage ordering rules belong to the operator webhook and are not duplicated here.

### Configuration

Loaded from YAML file (`--config` flag) + environment variable overrides with `CAPP_` prefix and `_` separators (e.g., `CAPP_AUTH_MODE=openshift`). Priority: env vars > YAML > code defaults.

Default config: `config/config.yaml`. Key settings:
- `auth.mode`: passthrough | openshift | ldap
- `auth.ldap`: directory URL/CA, search DN/filter/attribute settings, login deadline, token lifetime, issuer/audience, and Ed25519 signing/trust keys; validated only in LDAP mode
- `clusters[]`: array of cluster connections with credentials
- `resources.<name>.enabled`: feature flags for resource handlers (all on by default)
- `resources.observability`: per-Capp metrics endpoints, off by default (`enabled`); when on, requires `queryURL` (a Prometheus-compatible server; inject via `CAPP_RESOURCES_OBSERVABILITY_QUERYURL`) and at least one entry in `metrics`. Each metric's query (or each of its `series`) is parsed as PromQL, and every vector selector (including a bare metric name with no braces) must contain the `$selector` placeholder; `clusterLabel` is required on every metric when more than one cluster is configured, because `queryURL` is shared — `resources.observability.labels` adds extra matchers (e.g. to pin a shared Prometheus) to every query
- `cappSizes`: small/medium/large resource presets served at `/api/v1/sizes` and passed to the capps and revisions handlers
- `gitops`: optional Git sync; per-cluster `clusters[].gitOpsPath` nests under `gitops.pathPrefix`
- `server.corsAllowedOrigins`: defaults to `localhost:3000` (capp-frontend dev server)
- `tracing.*` is parsed, but no OTLP exporter is wired up in code

`auth.ldap.url` accepts only `ldaps://` endpoints, with port 636 when omitted. Both directory connections verify certificates and hostnames before sending credentials. `auth.ldap.caCert` supplies a base64 PEM CA bundle; empty uses system roots. StartTLS is unsupported, and `ldap://` URLs fail startup validation.

LDAP passwords and PEM private keys come from `CAPP_AUTH_LDAP_BINDPASSWORD` and `CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY`; Helm supplies them from `secret.ldapBindPassword` and `secret.ldapSigningPrivateKey`. Supply public `auth.ldap.verificationKeys` maps through mounted YAML, not JSON environment variables. Key IDs use lowercase ASCII letters/digits, `-`, and `_`. All replicas need consistent issuer/audience and signing/trust configuration plus synchronized clocks. Distribute a new public key before activating its private key, and retain retiring keys until their tokens expire. Directory account/password/group changes do not revoke issued tokens; removing a signing key fleet-wide invalidates all its tokens after rollout.

OpenShift and LDAP require inline service-account tokens with users/groups impersonation permissions for every managed cluster. Canonical usernames/groups must match User/Group RoleBindings; the default LDAP filter resolves direct membership only, without guaranteed nested/primary groups. Keep required Projects access for namespace listing. Helm grants TokenReview permissions and OAuth annotations only in OpenShift mode. Directory operators own AD lockout policy/recovery, platform operators own distributed gateway controls, and automation must stop on rejected credentials rather than retry. The optional backend rate limiter is per IP and per instance.

### Project Layout

```
cmd/
  server/main.go                # Server entry point, startup sequence
  cappctl/main.go               # CLI entry point
  mcp/main.go                   # capp-mcp entry point
internal/
  mcpserver/                    # capp-mcp ToolSets, bearer-token middleware, backend client wrapper
  testutil/                     # Fake K8s clients, Gin engine helpers, mock managers for handler tests
  server/                       # Gin engine setup, route registration, auth route handlers
  config/                       # Config structs, Viper loading, validation
  auth/                         # AuthManager implementations (passthrough, openshift, ldap)
  cluster/                      # ClusterManager, health checks, credential loading
  gitops/                       # GitOps client: clone, commit, push per-capp values files to a remote repo
  middleware/                   # Auth, cluster, logging, metrics, CORS, rate limit, recovery
  resources/                    # Resource handler registry
    cluster/
      namespaces/               # Namespace CRUD handler (SelfSubjectAccessReview filtering, RoleBinding/ResourceQuota)
    namespaced/
      capps/                    # Capp CRUD, sync, migrate handler, DTO types, K8s conversion; metrics.go has the observability endpoints
        revisions/              # CappRevision list/get/rollback handler (reuses capps DTOs + BareFromK8s)
      configmaps/               # ConfigMap CRUD handler (dana.io/capp-managed=true filter on list)
      secrets/                  # Secret CRUD handler
  cli/                          # cappctl CLI packages (no Gin/K8s imports)
    config/                     # ~/.config/cappctl/config.yaml read/write, context CRUD
    client/                     # HTTP client: auth header, JSON encode/decode, API error mapping
    output/                     # table/wide/json/yaml renderer
    resource/                   # ResourceCommand interface + registry
    root/                       # State struct, root Cobra command, PersistentPreRunE, token refresh
    auth/                       # login, logout, context commands
    capps/                      # Capps ResourceCommand: get/list/create/update/delete/sync/migrate/rollback/rollout
    capprevisions/              # CappRevisions ResourceCommand: get only
  apierrors/                    # Canonical error types
pkg/k8s/                        # Kubernetes scheme builder (CRD type registration)
pkg/clientversion/              # backend↔client version check (known clients, User-Agent, warning header, minor comparison); no Gin/K8s imports
api/openapi.yaml                # OpenAPI 3.1 spec (embedded in binary)
helm/
  capp-backend/                 # Helm chart for the server (deployed as a Capp resource)
  capp-mcp/                     # Helm chart for capp-mcp (deployed as a Knative-autoscaled Capp)
  capp-template/                # Generic Helm chart for deploying a single Capp resource
deploy/                         # Dockerfiles (server, cappctl, capp-mcp) and reference manifests
```

### cappctl CLI

Binary at `cmd/cappctl/`. Built with Cobra. No Gin or K8s imports — pure Go + stdlib.

**Command tree:**
```
cappctl login / logout
cappctl context list | use <name> | current
cappctl mcp-headers
cappctl completion bash|zsh|fish|powershell
cappctl get      capps [name] [--bound-to secret/<name>|configmap/<name>]
cappctl create   capps
cappctl update   capps <name>
cappctl delete   capps <name>
cappctl sync     capps <name> [--disable]
cappctl migrate  capps <name>
cappctl rollback capps <name> --to-revision N [-y]
cappctl rollout  capps <name> --action promote|abort|retry [--revision REV] [-y]
cappctl rollout  capps <name> --remove-strategy [-y]
cappctl get      capprevisions --capp <name> [revision]
```

**Global flags:** `--server`, `--token`, `--cluster`, `--namespace`, `--context`, `--output` (table|wide|json|yaml), `--insecure`

**Env var overrides (CI-friendly):** `CAPP_SERVER`, `CAPP_TOKEN`, `CAPP_CLUSTER`, `CAPP_NAMESPACE`

**Config file:** `~/.config/cappctl/config.yaml` (0600, atomic write). Stores named contexts with server URL, auth mode, access token, expiry, cluster, and namespace; OpenShift contexts may also have a refresh token. LDAP login clears previous refresh credentials and never persists the password.

**LDAP login:** `cappctl login --auth-mode ldap` supports `--username` with LDAP-only `--password-stdin`, or `--token` import. Stdin is bounded to 16 KiB raw bytes, validated as UTF-8, and has at most one terminal LF/CRLF removed; other bytes are preserved. Reject empty passwords, `--password` (even empty), and token/password-flag combinations. Explicit/detected non-LDAP modes reject `--password-stdin` before reading input or starting an interactive flow. LDAP login requires verified HTTPS, rejects `--insecure`, never prompts/opens a browser, and rejects all credential-request redirects without saving a context. Imported JWT `exp` must be usable, positive, numeric, and in the future; parsing it without signature verification supplies local metadata only, never authentication.

**Token expiry and refresh:** `PersistentPreRunE` in `internal/cli/root/root.go` checks selected context tokens before authenticated commands. OpenShift contexts with refresh credentials refresh when expiry is at most 30 seconds away. LDAP contexts never refresh or silently reauthenticate: at or after known expiry, return exactly `LDAP access token expired; run cappctl login again` before handler/network execution. Unexpired LDAP tokens remain usable within the last 30 seconds. Preserve flag > environment > context precedence; `--token`/`CAPP_TOKEN` overrides do not inherit expired active/named context metadata, including when using its server. Backend verification still applies to overrides.

**LDAP automation and trust:** Use a dedicated AD account and supply stdin from a secret store, as shown in [docs/cappctl.md](docs/cappctl.md#ldap). Repeat login explicitly on expiry; stop on rejected credentials and rotate stale secrets rather than retrying. Install private backend HTTPS CAs in the trust store of the workstation/container running the CLI. `auth.ldap.caCert` is the backend's directory trust setting, not CLI backend trust. Preserve existing OpenShift password/browser/direct-token and passthrough behavior. Run `make lint` and `make test` after CLI documentation changes as well as code changes.

**MCP integration:** `cappctl mcp-headers` (`internal/cli/auth/headers.go`) prints `{"Authorization": "Bearer <token>"}` after the same root authentication checks as resource commands. Expired LDAP context tokens produce a nonzero exit and no headers; the helper cannot renew them. Explicit login must precede reconnecting after LDAP expiry. OpenShift refresh remains supported. Configure a Claude Code MCP `headersHelper` with `"headersHelper": "cappctl mcp-headers --context <name>"`; see [the CLI reference](docs/cappctl.md#mcp--claude-code).

**Adding a resource:** implement `ResourceCommand` interface (`internal/cli/resource/handler.go`), call `registry.Register` in `cmd/cappctl/main.go`. No other files change. The interface requires `RegisterSyncCommand(parent *cobra.Command)`, `RegisterMigrateCommand(parent *cobra.Command)`, `RegisterRollbackCommand(parent *cobra.Command)` and `RegisterRolloutCommand(parent *cobra.Command)` — implement as no-ops if sync, migrate, rollback or rollout is not applicable to the resource.

### Startup Sequence
1. Load config (YAML + env vars) → validate
2. Init zap logger
3. Build K8s scheme (register CRD types)
4. Create ClusterManager → connect to clusters → start health checks
5. Create AuthManager (based on auth.mode) → start background cleanup (if the auth manager implements it)
6. Create GitOps client (if `gitops.enabled`) → clone remote repo
7. Build resource handler registry
8. Create Gin server → listen on :8080
9. Graceful shutdown on SIGTERM/SIGINT (30s drain)
