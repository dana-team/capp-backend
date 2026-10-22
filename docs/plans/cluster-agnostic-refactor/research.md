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
- **Keep `passthrough` auth mode** — useful
  for local dev and CI where the caller already has a K8s token. Not intended
  for production multi-instance deployment.

---

## Gap Groups

Authentication phase 1 is merged into `main`. This inventory describes the
remaining work; original IDs are retained for open gaps. Resolved gaps A2, B3,
C2, C3, and E2 are no longer included in the gap counts.

### Group 1: Authentication — OpenShift OAuth is cluster-bound

Phase 1 delivered `ldap` alongside `openshift` and `passthrough`. Direct LDAPS
search-and-bind authenticates AD credentials; backend-issued access tokens are
verified locally on any instance with consistent issuer/audience, signing/trust
configuration, token policy, and clocks. This supplies the cluster-independent
authentication path. See the [authentication HLD](authentication/hld.md).

The original A1 limitation remains specific to `openshift` mode: TokenReview uses
`rest.InClusterConfig()` against the hosting cluster. Keeping that mode does not
block LDAP deployment, and changing OpenShift's validation path is not required
by the LDAP design. Instances behind one interchangeable endpoint must use the
same auth mode and compatible configuration.

---

### Group 2: Authorization — K8s RBAC assumes cluster-local identities

Phase 1 preserves K8s RBAC via SSAR and impersonation. Every managed cluster
must grant the canonical LDAP usernames/groups the required permissions through
RoleBindings or ClusterRoleBindings. Authentication alone grants no application
permissions; user bindings and implicit Kubernetes groups still affect access.
The Helm chart now creates impersonation RBAC for both OpenShift and LDAP, with
TokenReview permissions restricted to OpenShift. This describes the current
implementation, not the target authorization architecture.

**Decided direction:** in the new authorization mode, capp-backend owns application
authorization and is the sole Kubernetes client for platform operations. Backend service identities hold
Kubernetes permissions; end users receive no Kubernetes credentials or grants.
Application policy replaces end-user impersonation and SSAR as the authorization
mechanism. Preserve editable namespace membership without creating user
RoleBindings. Store cluster-scoped policy and namespace memberships in protected
ConfigMaps in a backend-only namespace on each managed cluster; all backend
instances use that target cluster's policy, independently of their hosting cluster.
This avoids an additional database or policy service. Policy availability depends
on the target cluster.

**Compatibility and migration:** release the new authorization mode alongside
existing authentication and Kubernetes authorization, preserving existing behavior
by default. Production can continue using OpenShift authentication and current
authorization while separate stage/dev deployments explicitly enable and validate
the new mode. Authentication and authorization selection are separate concerns;
the backend-only access requirement applies to the new mode, not retroactively to
legacy deployments. Migration is explicit and must preserve effective application
access. Keep serving replicas on a consistent authorization mode and policy
configuration; do not fall back to legacy authorization after a policy denial or
failure. Retire user Kubernetes grants only as part of the migration, with rollback
and the resulting access boundary accounted for in the authorization HLD.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| B1 | **SSAR depends on per-cluster K8s RBAC** — `canCreateCapps()` and `canAccessNamespaces()` submit SSAR through the impersonated client. Managed clusters must grant the canonical usernames/groups the required permissions; operations without matching grants are denied despite valid authentication. There is no application-level RBAC alternative. | Significant | `namespaces/handler.go:126-140`, `namespaces/handler.go:518-530` |
| B2 | **Namespace operations use admin client with SSAR as gate** — the admin client performs the actual create/update/delete, while SSAR on the user client only checks permission. If SSAR fails due to missing RoleBindings for AD identities, the admin client's capabilities are unreachable. The selected application authorization model must replace these gates and membership RoleBindings. | Significant | `middleware/cluster.go:85-88`, `namespaces/handler.go:83,260,297,391,462` |

**Affected files:**
`internal/resources/cluster/namespaces/handler.go`, `internal/middleware/cluster.go`

---

### Group 3: OpenShift platform coupling

Phase 1 decoupled Projects permissions from authentication: the chart uses
`rbac.openShiftProjects` (default `false`) to include Projects access in `capp-user`.
Service-account wiring supports both OpenShift and LDAP. These Helm gaps are
resolved; runtime namespace listing still branches on the target platform.
Projects provides access-filtered listing; optional platform-specific
optimizations are compatible with cluster independence.
The vanilla Kubernetes listing path performs one SSAR per CAPP-managed namespace,
not on every backend request.
Any replacement requires agreement on authorization semantics and validation of
listing correctness, latency, and API-call volume at expected namespace counts
and concurrency, with agreed latency and API-call budgets. The selected backend
authorization model must also govern namespace visibility; current Projects/SSAR
filtering relies on the user Kubernetes grants that the target model removes.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| C1 | **Namespace listing branches on `IsOpenShift`** — `list()` uses the OpenShift Projects API (`project.openshift.io/v1`) on OpenShift clusters and falls back to admin-list + per-NS SSAR on vanilla K8s. Two code paths for the same operation. `IsOpenShift` is probed at startup per cluster via `/apis/route.openshift.io`. | Significant | `namespaces/handler.go:104-108`, `namespaces/handler.go:142-165` (Projects), `namespaces/handler.go:168-189` (K8s), `pkg/k8s/helpers.go:53-77`, `cluster/manager.go:109-120` |

**Affected files:**
`internal/resources/cluster/namespaces/handler.go`, `pkg/k8s/helpers.go`,
`internal/cluster/manager.go`

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

The merged API and OpenAPI specification support LDAP login, access tokens, and
the existing modes. Phase 2 remains responsible for CLI integration and automation
against that API; phase 1 is independently usable.

| ID | Gap | Severity | Evidence |
|----|-----|----------|----------|
| E1 | **LDAP CLI and automation delivery remains** — phase 2 must complete secure unattended login, token import/storage, expiry enforcement for resource and MCP-header commands, and CLI documentation. | Minor | `internal/cli/auth/login.go`, `internal/cli/root/root.go` |

**Affected files:**
`internal/cli/auth/login.go`, `internal/cli/auth/helpers.go`,
`internal/cli/client/client.go`, `internal/cli/root/root.go`

---

## Already Multi-Instance Safe

| Area | Why |
|------|-----|
| Passthrough auth | Stateless; token validated by the target cluster, not the backend's cluster |
| LDAP auth | LDAPS login issues short-lived EdDSA access tokens; consistently configured instances verify them locally without LDAP calls or sticky sessions |
| OpenShift CSRF state | HMAC-signed with a shared config secret; replicas using the same OpenShift configuration can validate each other's state |
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
| 1 — Authentication | 0 | 0 | 0 | LDAP backend merged; OpenShift's hosting-cluster limitation is retained |
| 2 — Authorization | 0 | 2 | 0 | Existing K8s RBAC remains; redesign is independent of LDAP authentication |
| 3 — OpenShift coupling | 0 | 1 | 0 | Replacing C1 requires authorization agreement and performance validation |
| 4 — GitOps concurrency | 0 | 1 | 0 | Independent |
| 5 — CLI and API surface | 0 | 0 | 1 | Phase 2 consumes the merged LDAP API |

---

## Open Questions

1. **Future long-lived tokens and revocation** — phases 1–2 use dedicated AD
   automation accounts to obtain short-lived access tokens. Long-lived tokens
   remain future work: define expiration, individual revocation, rotation, and
   permission limits enforced consistently across instances, accounting for
   stored-secret exposure and permission freshness.
2. **ADFS integration** — which protocol and token-validation or exchange flow
   should support ADFS alongside backend-issued tokens, and how should both map
   to authorization identities? Define issuer, audience, signature, and token-purpose
   validation for each source, including whether ADFS access tokens are accepted
   directly or exchanged for backend tokens. This remains separate from LDAP integration.
