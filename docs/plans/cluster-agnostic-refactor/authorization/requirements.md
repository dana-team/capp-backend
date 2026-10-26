**Purpose:** Define backend authorization outcomes, constraints, and success criteria; architecture, policy/API contracts, migration procedures, and task execution remain in their existing documents.

**Inherited requirements:** [Shared R01](../requirements.md#r01), [shared R02](../requirements.md#r02). Verified identities and their freshness follow [authentication requirements](../authentication/requirements.md).

## Goal

Add backend-owned authorization that preserves platform access and editable memberships without requiring end-user Kubernetes credentials or grants in the new mode.

## Scope

- In scope: opt-in LDAP/backend authorization, protected application policy, resource/discovery enforcement, membership editing, auditing, and controlled migration.
- Non-goals: removing legacy modes or making backend authorization the sole option, authentication redesign, new identity providers, a policy-management API, automatic Kubernetes RBAC translation, broader platform cleanup, and GitOps concurrency.

## Requirements

### R01

- Requirement: Enforce application policy for every protected operation in backend mode, with capp-backend as the sole Kubernetes client for platform operations.
- Success criterion: Users need no Kubernetes credentials or grants; approved operations execute with backend service identities, without end-user impersonation or SSAR. Unknown operations and missing, invalid, or unavailable policy fail closed, without legacy fallback. Discovery exposes only authorized items, metadata, totals, and capabilities; cross-cluster operations authorize both ends before dependent reads or writes. Denials remain distinguishable from policy failures.
- Source: [Research — backend authorization boundary](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities), [namespace visibility](../research.md#group-3-openshift-platform-coupling); enforcement criteria retained through the requirements review on 2026-10-25.

### R02

- Requirement: Preserve safe, explicitly authorized namespace membership editing.
- Success criterion: Only callers with the required delegation rights can change membership; operator grants survive API edits. Failed setup publishes no grant, concurrent edits report conflicts without lost updates, and a recreated namespace cannot inherit its predecessor's membership.
- Source: [Research — editable application membership](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities); membership-safety criteria retained through the requirements review on 2026-10-25.

### R03

- Requirement: Support controlled migration and rollback that preserve reviewed effective application access.
- Success criterion: An allow/deny comparison covers existing users/groups, custom restrictions, invitations, and benchmark access. Unrepresentable permissions keep a deployment on legacy mode rather than broadening access. Cutover and rollback reconcile membership changes and use homogeneous serving pools; completing migration retires reviewed user Kubernetes grants. Rollback explicitly accounts for restoring reviewed legacy access.
- Source: [Research — compatibility and migration](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities); migration criteria retained through the requirements review on 2026-10-25.

### R04

- Requirement: Meet the agreed namespace-list correctness and latency budgets, eliminate per-request policy loading, and bound permission-update delay before migration.
- Success criterion: With two replicas, 1,000 managed namespaces per cluster, and 20 concurrent list requests for five minutes after warm-up, p95 end-to-end latency is at most one second, with no authorization failures or leaks under healthy dependencies. Warm requests perform no policy API reads or policy compilation; namespace listing uses at most two data reads (namespaces and quotas), independent of namespace count. Measure background policy traffic separately and demonstrate it follows refreshes/changes rather than request count. A separate ordinary resource-request profile verifies the same policy reuse. Across both replicas, grants and revocations affect new authorization decisions within the configured maximum policy age, or those decisions fail closed. Failure blocks migration.
- Source: [Research — listing correctness and performance budgets](../research.md#group-3-openshift-platform-coupling); workload/latency targets were confirmed on 2026-10-25. Policy reuse and the five-second update-delay trade-off were accepted on 2026-10-26; see the [HLD decision](hld.md#adr--shared-policy-with-bounded-freshness).

### R05

- Requirement: Make authorization outcomes attributable to callers without exposing sensitive data.
- Success criterion: Audit records identify the verified caller, action, target, mode, policy version, and outcome, including denials and policy failures. Logs exclude credentials, raw policy, and Secret data; monitoring distinguishes authorization outcomes without unbounded identity/target labels.
- Source: Audit outcome and criteria retained through the requirements review on 2026-10-25.

## Constraints and compatibility

- Initially enable backend authorization only with LDAP; reject incompatible configuration and preserve legacy authorization as the default under [shared R02](../requirements.md#r02). Keep HTTP shapes, quotas, resource operations, and both workload variants supported.
- Policy and backend credentials must be inaccessible through generic resource endpoints and user-controlled workloads. Configured namespace restrictions remain additional boundaries regardless of grants; backend Kubernetes permissions are limited to required operations.
- Use target-cluster policy without another database/policy service. A maximum permission-update delay of five seconds is accepted: replicas may temporarily disagree, and removed grants may authorize new decisions within that window. A replica must stop authorizing when it cannot establish policy freshness within five seconds; observed missing/invalid required policy invalidates authorization immediately. Already-authorized operations may finish, and token-carried groups retain authentication freshness limits. Multi-resource/cross-cluster writes remain non-transactional. The HLD records this decision; the LLD defines the refresh contract.

## Assumptions

- Operators control protected namespaces, credentials, admission/service-account isolation, and initial grants on every target cluster; stage/dev and production are separate deployments.
- Policy storage, per-replica memory, and background refreshes fit the initial deployment. The R04 workload is an acceptance target, not measured capacity; representative membership sizes, resources, dependency latency, bytes, CPU, and measured permission-update delay must accompany results.
