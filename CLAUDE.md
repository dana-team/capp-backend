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
DELETE /api/v1/clusters/:cluster/namespaces/:namespace (auth + cluster) — deletes empty namespace; 409 if Capps exist

GET    /api/v1/clusters/:cluster/capps                 (auth + cluster) — list all capps across namespaces
       /api/v1/clusters/:cluster/namespaces/:namespace/capps[/:name]         (CRUD)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync      (auth + cluster) — enable Git sync / re-sync; returns 501 if gitops feature is disabled in backend
DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/sync      (auth + cluster) — disable Git sync (deletes values file); returns 501 if gitops feature is disabled in backend
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate  (auth + cluster) — migrate Capp to another cluster/namespace with dependent resources; requires targetHostname on copy when source has a custom hostname
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions                     (auth + cluster) — list CappRevisions (label rcs.dana.io/cappName), newest first
GET    /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision           (auth + cluster) — get revision by number (spec.revisionNumber, not object name)
POST   /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/revisions/:revision/rollback  (auth + cluster) — apply revision's spec/labels/annotations to live Capp (backup-to-git label kept as live); pushes to Git if synced
       /api/v1/clusters/:cluster/namespaces/:namespace/configmaps[/:name]    (CRUD)
GET    /api/v1/clusters/:cluster/namespaces/:namespace/configmaps/names     (auth + cluster) — list configmap names only
       /api/v1/clusters/:cluster/namespaces/:namespace/secrets[/:name]       (CRUD)
GET    /api/v1/clusters/:cluster/namespaces/:namespace/secrets/names         (auth + cluster) — list secret names only
GET    /api/v1/clusters/:cluster/configmaps            (auth + cluster) — list all configmaps across namespaces (dana.io/capp-managed=true only)
GET    /api/v1/clusters/:cluster/secrets               (auth + cluster) — list all secrets across namespaces
```

### Key Patterns

**ResourceHandler interface** (`internal/resources/registry.go`): Pluggable handlers register routes and are enabled/disabled via config. Current handlers: `capps`, `capprevisions`, `namespaces`, `configmaps`, `secrets`. To add a new resource type, implement the interface and register it in `cmd/server/main.go`.

**AuthManager interface** (`internal/auth/manager.go`): Factory selects implementation based on `auth.mode` config. Each mode implements `Authenticate`, `Login`, `PasswordLogin`, and `Refresh`. Passthrough mode forwards the caller's bearer token as-is. OpenShift mode is fully stateless — it validates tokens via TokenReview and impersonates users on managed clusters. LDAP mode verifies passwords with directory search-and-bind, issues EdDSA access tokens, and verifies them locally for Kubernetes impersonation without a directory call on resource requests. `GET /api/v1/auth/mode` returns the active mode.

**LDAP contracts:** `POST /api/v1/auth/login` accepts `username`/`password` and returns the existing `TokenPair` with an empty `refreshToken` and `Cache-Control: no-store`. Verified LDAPS protects both the search-account connection and the separate target-user bind; complete group resolution is required before issuance. Unknown/ambiguous users and rejected user passwords return 401 / `UNAUTHORIZED` / `invalid credentials`; directory failures return sanitized 503 / `INTERNAL_ERROR` / `directory service unavailable`. Missing/invalid bearer tokens return 401 / `UNAUTHORIZED` / `request is not authenticated`; expired tokens return 401 / `UNAUTHORIZED` / `token has expired`. Raw-token login is unsupported, refresh returns 501 / `NOT_SUPPORTED` without parsing a body, and OAuth routes remain OpenShift-only. Preserve existing-mode behavior and never log credentials, tokens, keys, or raw directory diagnostics.

LDAP clients submit passwords over HTTPS, retain access tokens securely, and log in again on expiry (900 seconds by default). LDAP has no refresh sessions, ADFS SSO, or MFA.

**ClusterManager** (`internal/cluster/manager.go`): Holds connections to all configured clusters, runs health checks every 30s, and creates per-request scoped clients.

**DTO conversion** (`internal/resources/namespaced/capps/convert.go`): Converts between API request/response types and Kubernetes Capp resources. The CLI (`internal/cli/capps/capps.go`) imports these same types directly — no duplication. `TestConvertCoverage` (`drift_test.go`) fails when a new `CappRequest` field is not populated in `fullCappRequest()` and round-tripped through `ToK8s`/`FromK8s`, so every new field must be wired both ways.

List fields are full-replace on update (`PUT`): `env`, `volumeMounts`, `secretVolumes`, `configMapVolumes`, `nfsVolumes` and `imagePullSecrets` are rebuilt from the request, so omitting one clears it. `imagePullSecrets` (`[]string` of Secret names) maps to the pod spec's `imagePullSecrets`; names are validated as DNS-1123 subdomains and must be unique, but Secret existence/type is not checked (same as env and volume refs).

### Configuration

Loaded from YAML file (`--config` flag) + environment variable overrides with `CAPP_` prefix and `_` separators (e.g., `CAPP_AUTH_MODE=openshift`). Priority: env vars > YAML > code defaults.

Default config: `config/config.yaml`. Key settings:
- `auth.mode`: passthrough | openshift | ldap
- `auth.ldap`: directory URL/CA, search DN/filter/attribute settings, login deadline, token lifetime, issuer/audience, and Ed25519 signing/trust keys; validated only in LDAP mode
- `clusters[]`: array of cluster connections with credentials
- `resources.<name>.enabled`: feature flags for resource handlers
- `server.corsAllowedOrigins`: defaults to `localhost:3000` (capp-frontend dev server)

`auth.ldap.url` accepts only `ldaps://` endpoints, with port 636 when omitted. Both directory connections verify certificates and hostnames before sending credentials. `auth.ldap.caCert` supplies a base64 PEM CA bundle; empty uses system roots. StartTLS is unsupported, and `ldap://` URLs fail startup validation.

LDAP passwords and PEM private keys come from `CAPP_AUTH_LDAP_BINDPASSWORD` and `CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY`; Helm supplies them from `secret.ldapBindPassword` and `secret.ldapSigningPrivateKey`. Supply public `auth.ldap.verificationKeys` maps through mounted YAML, not JSON environment variables. Key IDs use lowercase ASCII letters/digits, `-`, and `_`. All replicas need consistent issuer/audience and signing/trust configuration plus synchronized clocks. Distribute a new public key before activating its private key, and retain retiring keys until their tokens expire. Directory account/password/group changes do not revoke issued tokens; removing a signing key fleet-wide invalidates all its tokens after rollout.

OpenShift and LDAP require inline service-account tokens with users/groups impersonation permissions for every managed cluster. Canonical usernames/groups must match User/Group RoleBindings; the default LDAP filter resolves direct membership only, without guaranteed nested/primary groups. Keep required Projects access for namespace listing. Helm grants TokenReview permissions and OAuth annotations only in OpenShift mode. Directory operators own AD lockout policy/recovery, platform operators own distributed gateway controls, and automation must stop on rejected credentials rather than retry. The optional backend rate limiter is per IP and per instance.

### Project Layout

```
cmd/
  server/main.go                # Server entry point, startup sequence
  cappctl/main.go               # CLI entry point
internal/
  server/                       # Gin engine setup, route registration, auth route handlers
  config/                       # Config structs, Viper loading, validation
  auth/                         # AuthManager implementations (passthrough, openshift, ldap)
  cluster/                      # ClusterManager, health checks, credential loading
  gitops/                       # GitOps client: clone, commit, push per-capp values files to a remote repo
  middleware/                   # Auth, cluster, logging, metrics, CORS, rate limit, recovery
  resources/                    # Resource handler registry
    cluster/
      namespaces/               # Namespace list + create handler (with SelfSubjectAccessReview filtering)
    namespaced/
      capps/                    # Capp CRUD, sync, migrate handler, DTO types, K8s conversion
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
    capps/                      # Capps ResourceCommand: get/list/create/update/delete/sync/migrate/rollback
    capprevisions/              # CappRevisions ResourceCommand: get only
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
cappctl mcp-headers
cappctl get      capps [name]
cappctl create   capps
cappctl update   capps <name>
cappctl delete   capps <name>
cappctl sync     capps <name> [--disable]
cappctl migrate  capps <name>
cappctl rollback capps <name> --to-revision N [-y]
cappctl get      capprevisions --capp <name> [revision]
```

**Global flags:** `--server`, `--token`, `--cluster`, `--namespace`, `--context`, `--output` (table|wide|json|yaml), `--insecure`

**Env var overrides (CI-friendly):** `CAPP_SERVER`, `CAPP_TOKEN`, `CAPP_CLUSTER`, `CAPP_NAMESPACE`

**Config file:** `~/.config/cappctl/config.yaml` (0600, atomic write). Stores named contexts with server URL, auth mode, access token, expiry, cluster, and namespace; OpenShift contexts may also have a refresh token. LDAP login clears previous refresh credentials and never persists the password.

**LDAP login:** `cappctl login --auth-mode ldap` supports `--username` with LDAP-only `--password-stdin`, or `--token` import. Stdin is bounded to 16 KiB raw bytes, validated as UTF-8, and has at most one terminal LF/CRLF removed; other bytes are preserved. Reject empty passwords, `--password` (even empty), and token/password-flag combinations. Explicit/detected non-LDAP modes reject `--password-stdin` before reading input or starting an interactive flow. LDAP login requires verified HTTPS, rejects `--insecure`, never prompts/opens a browser, and rejects all credential-request redirects without saving a context. Imported JWT `exp` must be usable, positive, numeric, and in the future; parsing it without signature verification supplies local metadata only, never authentication.

**Token expiry and refresh:** `PersistentPreRunE` in `internal/cli/root/root.go` checks selected context tokens before authenticated commands. OpenShift contexts with refresh credentials refresh when expiry is at most 30 seconds away. LDAP contexts never refresh or silently reauthenticate: at or after known expiry, return exactly `LDAP access token expired; run cappctl login again` before handler/network execution. Unexpired LDAP tokens remain usable within the last 30 seconds. Preserve flag > environment > context precedence; `--token`/`CAPP_TOKEN` overrides do not inherit expired active/named context metadata, including when using its server. Backend verification still applies to overrides.

**LDAP automation and trust:** Use a dedicated AD account and supply stdin from a secret store, as shown in [docs/cappctl.md](docs/cappctl.md#ldap). Repeat login explicitly on expiry; stop on rejected credentials and rotate stale secrets rather than retrying. Install private backend HTTPS CAs in the trust store of the workstation/container running the CLI. `auth.ldap.caCert` is the backend's directory trust setting, not CLI backend trust. Preserve existing OpenShift password/browser/direct-token and passthrough behavior. Run `make lint` and `make test` after CLI documentation changes as well as code changes.

**MCP integration:** `cappctl mcp-headers` (`internal/cli/auth/headers.go`) prints `{"Authorization": "Bearer <token>"}` after the same root authentication checks as resource commands. Expired LDAP context tokens produce a nonzero exit and no headers; the helper cannot renew them. Explicit login must precede reconnecting after LDAP expiry. OpenShift refresh remains supported. Configure a Claude Code MCP `headersHelper` with `"headersHelper": "cappctl mcp-headers --context <name>"`; see [the CLI reference](docs/cappctl.md#mcp--claude-code).

**Adding a resource:** implement `ResourceCommand` interface (`internal/cli/resource/handler.go`), call `registry.Register` in `cmd/cappctl/main.go`. No other files change. The interface requires `RegisterSyncCommand(parent *cobra.Command)`, `RegisterMigrateCommand(parent *cobra.Command)` and `RegisterRollbackCommand(parent *cobra.Command)` — implement as no-ops if sync, migrate or rollback is not applicable to the resource.

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
