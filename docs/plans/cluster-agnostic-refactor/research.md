# Cluster-Agnostic Refactor — Research

## Goal

Deploy capp-backend as a Capp on N clusters behind a single global DNS. Any
request must succeed regardless of which instance handles it. All instances
manage the same set of clusters with identical config.

**Identity provider:** Active Directory.

## Constraints

- **Keep `openshift` auth mode** — existing login, refresh, and token validation
  flows remain supported. Cluster-independent authentication must coexist with
  these flows; cluster independence does not require removing OpenShift auth.
- **Keep `passthrough` auth mode** — lightweight (~60 LOC, zero deps), useful
  for local dev and CI where the caller already has a K8s token. Not intended
  for production multi-instance deployment.

---

## Gap Groups

### Group 1: Authentication — OpenShift OAuth is cluster-bound

The `openshift` auth mode cannot work in multi-instance cross-cluster deployment.
The root cause is that token validation uses `rest.InClusterConfig()`, which
binds authentication to whichever cluster the pod runs on. This deployment
model requires authentication that is independent of the hosting cluster.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| A1 | **OpenShift auth is cluster-bound** — `Authenticate()` validates bearer tokens via `authv1.TokenReview` against the pod's local API server (`rest.InClusterConfig()`). A token issued by cluster-A's OAuth is rejected by cluster-B's TokenReview. All OAuth flows (login, refresh, authorize, callback, PasswordLogin challenge-response) depend on this single cluster's OAuth server. | Blocker | `openshift.go:103` (InClusterConfig), `openshift.go:224` (TokenReview), `openshift.go:260-364` (OAuth flows) |
| A2 | **No external IdP auth mode** — `auth.mode` only accepts `passthrough \| openshift`. No OIDC/JWT mode exists to validate tokens from an external IdP. | Blocker | `config.go:96-99` (enum), `validation.go:10-12` (validation map) |

**Affected files:**
`internal/auth/openshift.go`, `internal/auth/manager.go`, `internal/config/config.go`,
`internal/config/validation.go`, `internal/server/auth_routes.go`

---

### Group 2: Authorization — K8s RBAC assumes cluster-local identities

Authorization is currently coupled to K8s RBAC via SSAR and impersonation.
With OpenShift, users exist *in* the cluster so RBAC bindings work naturally.
When users come from an external IdP (AD/LDAP), K8s clusters may not have
RoleBindings for those identities — SSAR would deny everything despite valid
authentication. This group is independent of Group 1: authentication can be
replaced without changing authorization, and vice versa.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| B1 | **SSAR depends on K8s RoleBindings for external identities** — `canCreateCapps()` and `canAccessNamespaces()` submit SSAR to check permissions via the impersonated client. When the impersonated identity comes from AD/LDAP, every managed cluster must have RoleBindings for those AD usernames/groups. Without them, SSAR denies all operations even with valid authentication. Currently there is no application-level RBAC alternative. | Significant | `namespaces/handler.go:126-140`, `namespaces/handler.go:518-530` |
| B2 | **Namespace operations use admin client with SSAR as gate** — the admin client performs the actual create/update/delete, while SSAR on the user client only checks permission. If SSAR fails due to missing RoleBindings for AD identities, the admin client's capabilities are unreachable. Moving to application-level RBAC could decouple authorization from per-cluster K8s RoleBinding setup. | Significant | `middleware/cluster.go:85-88`, `namespaces/handler.go:83,260,297,391,462` |
| B3 | **Impersonation ClusterRole/Binding gated on `auth.mode=openshift`** — the Helm chart only creates the impersonator ClusterRole (impersonate users/groups) when `auth.mode=openshift`. Any other auth mode using impersonation needs these RBAC resources too; TokenReview permissions are only needed by modes that use TokenReview. | Significant | `helm/capp-backend/templates/clusterrole.yaml:1`, `helm/capp-backend/templates/clusterrolebinding.yaml:1` |

**Affected files:**
`internal/resources/cluster/namespaces/handler.go`, `internal/middleware/cluster.go`,
`helm/capp-backend/templates/clusterrole.yaml`, `helm/capp-backend/templates/clusterrolebinding.yaml`

---

### Group 3: OpenShift platform coupling

Runtime and Helm-level branches based on OpenShift detection. These are not
blockers for multi-instance. C2/C3 tie platform permissions and service-account
selection to OpenShift authentication. Projects provides access-filtered listing;
optional platform-specific optimizations are compatible with cluster independence.
The vanilla Kubernetes listing path performs one SSAR per CAPP-managed namespace,
not on every backend request.
Any replacement requires agreement on authorization semantics and validation of
listing correctness, latency, and API-call volume at expected namespace counts
and concurrency, with agreed latency and API-call budgets; a new RBAC model is
not assumed.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| C1 | **Namespace listing branches on `IsOpenShift`** — `list()` uses the OpenShift Projects API (`project.openshift.io/v1`) on OpenShift clusters and falls back to admin-list + per-NS SSAR on vanilla K8s. Two code paths for the same operation. `IsOpenShift` is probed at startup per cluster via `/apis/route.openshift.io`. | Significant | `namespaces/handler.go:104-108`, `namespaces/handler.go:142-165` (Projects), `namespaces/handler.go:168-189` (K8s), `pkg/k8s/helpers.go:53-77`, `cluster/manager.go:109-120` |
| C2 | **capp-user ClusterRole conditionally includes Projects** — the Helm template adds `project.openshift.io` verbs only when `auth.mode=openshift`. On a mixed-platform fleet, the RBAC must be per-cluster, not per-auth-mode. | Minor | `helm/capp-backend/templates/capp-user-cr.yaml:20-24` |
| C3 | **Deployment template gates SA on `auth.mode=openshift`** — in the Deployment template, `serviceAccountName` is only set when auth mode is openshift. Any auth mode using impersonation needs a service account. (The Capp template gates SA on `.Values.serviceAccount.name` which is always set — no issue there.) | Minor | `helm/capp-backend/templates/deployment.yaml:27-28` |

**Affected files:**
`internal/resources/cluster/namespaces/handler.go`, `pkg/k8s/helpers.go`,
`internal/cluster/manager.go`, `helm/capp-backend/templates/`

---

### Group 4: GitOps — single-instance concurrency model

The GitOps client holds a local git clone guarded by an in-process mutex.
This works for single-instance but breaks with multiple instances pushing to
the same branch concurrently.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| D1 | **Local clone + mutex is per-process** — `gitops.Client` holds a `sync.Mutex` and a local clone in a temp dir. Two instances each have their own clone and mutex. Concurrent pushes to the same branch from different instances → one fails with non-fast-forward. The failed instance runs `resetToRemote` discarding its commit with no retry. The cluster write already succeeded, so the Capp and its Git backup diverge silently (user sees a warning and must manually re-sync). | Significant | `gitops/client.go:32` (mutex), `gitops/client.go:116-174` (pull-commit-push), `gitops/client.go:163-165` (push fail), `gitops/client.go:259-282` (resetToRemote) |

**Affected files:**
`internal/gitops/client.go`

---

### Group 5: CLI and API surface

The CLI and OpenAPI spec hardcode the two existing auth modes. Adding a new
mode requires updates across these surfaces while preserving existing modes.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| E1 | **CLI login branches on `openshift \| passthrough`** — `login()` switches on auth mode strings. An unsupported mode falls through to the default error case; the CLI has no external IdP login path. | Minor | `cli/auth/login.go:98-117` |
| E2 | **OpenAPI spec documents only openshift/passthrough** — auth endpoints reference OpenShift-specific paths. External IdP authentication is not represented in the API contract. | Minor | `api/openapi.yaml` |

**Affected files:**
`internal/cli/auth/login.go`, `internal/cli/auth/helpers.go`,
`internal/server/auth_routes.go`, `api/openapi.yaml`

---

## Already Multi-Instance Safe

| Area | Why |
|------|-----|
| Passthrough auth | Stateless; token validated by the target cluster, not the backend's cluster |
| Stateless HMAC-signed token pattern | CSRF state tokens are signed with a shared config secret; any instance can validate any other's tokens. Reusable for OIDC state in the new auth mode. |
| Cluster middleware | Uses config-provided credentials per cluster, not InClusterConfig |
| All resource handlers | Stateless; use per-request scoped client from context |
| Capp migration | Uses `ClusterManager.ClientFor()` with caller credential; works from any instance |
| Impersonation pattern | `ClusterManager.ClientFor()` applies `ImpersonateUser/Groups` from any identity source — not coupled to OpenShift |
| Rate limiter | Per-instance enforcement is standard for stateless horizontally-scaled services; each instance correctly applies its own limit |
| Health checks | Per-instance cluster health tracking is correct behavior; an instance that can't reach a cluster should return 503 for it |
| Token cache | Per-instance in-memory cache is correct behavior; same as rate limiter and health checks |

---

## Gap Summary

| Group | Blockers | Significant | Minor | Technical dependency |
|-------|----------|-------------|-------|-----------------|
| 1 — Authentication | 2 | 0 | 0 | Independent of authorization |
| 2 — Authorization | 0 | 3 | 0 | Independent of Group 1 |
| 3 — OpenShift coupling | 0 | 1 | 2 | C2/C3 depend on Group 1 (auth-mode gated); replacing C1 requires authorization agreement and performance validation |
| 4 — GitOps concurrency | 0 | 1 | 0 | Independent |
| 5 — CLI and API surface | 0 | 0 | 2 | Reflects the supported authentication modes in Group 1 |

---

## Open Questions

1. **Authorization model** — should authorization stay as K8s RBAC (requiring
   RoleBindings for AD identities on every cluster), or move to application-level
   RBAC in capp-backend? Projects replacement also depends on the evaluation
   described in [Group 3](#group-3-openshift-platform-coupling).
2. **Automation credentials** — should automation use dedicated machine
   credentials that obtain short-lived access tokens or long-lived API tokens?
   Evaluate stored-secret exposure,
   directory availability, account lockout, and permission freshness. Long-lived
   tokens need expiration, individual revocation, rotation, and permission limits
   enforced consistently across instances.
3. **ADFS integration** — which protocol and token-validation or exchange flow
   should support ADFS alongside backend-issued tokens, and how should both map
   to authorization identities? Define issuer, audience, signature, and token-purpose
   validation for each source, including whether ADFS access tokens are accepted
   directly or exchanged for backend tokens. This remains separate from LDAP integration.

## Confirmed capabilities

- **LDAP feasibility** — direct directory access, search-account lookup, and a
  separate bind using the target user's credentials are supported. ADFS integration
  does not block this approach.
