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
- Success criterion: Only callers with the required delegation rights can change membership; operator grants survive API edits. Failed setup publishes no grant and concurrent edits report conflicts without lost updates. Membership remains UID-bound: once the namespace cache reflects a replacement UID, predecessor grants do not match. Until that update, old grants may authorize name-addressed operations against the replacement until namespace-cache convergence, without a fixed maximum delay; this is an explicit exception to immediate recreation isolation.
- Source: [Research — editable application membership](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities); membership-safety criteria reviewed on 2026-10-25, with bounded namespace staleness accepted on 2026-10-26 and superseded by unbounded eventual consistency on 2026-10-28.

### R03

- Requirement: Support controlled migration and rollback that preserve reviewed effective application access.
- Success criterion: An allow/deny comparison covers existing users/groups, custom restrictions, invitations, and benchmark access. Unrepresentable permissions keep a deployment on legacy mode rather than broadening access. Cutover and rollback reconcile membership changes and use homogeneous serving pools; completing migration retires reviewed user Kubernetes grants. Rollback explicitly accounts for restoring reviewed legacy access.
- Source: [Research — compatibility and migration](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities); migration criteria retained through the requirements review on 2026-10-25.

### R04

- Requirement: Meet the agreed namespace-list correctness and latency budgets, eliminate per-request policy/namespace authorization reads, and verify watch-driven policy/namespace convergence and recovery before migration.
- Success criterion: With two replicas, 1,000 managed namespaces per cluster, and 20 concurrent list requests for five minutes after warm-up, p95 end-to-end latency is at most one second, with no authorization failures or leaks under healthy dependencies and unchanged policy/namespace state. Warm requests perform no policy or namespace authorization API reads or policy compilation; namespace discovery uses cached namespace metadata and at most one data read (quotas), independent of namespace count. Actual resource reads/writes still use Kubernetes. Measure background policy and namespace traffic separately and demonstrate it follows refreshes/changes rather than request count. A separate ordinary resource-request profile verifies both caches. Across both replicas, measure policy and namespace convergence without treating observations as a guaranteed maximum delay. Verify watch recovery, failed-refresh retries without another event, and continued use of last valid data during watch/read failures, with failure closed for initial load, confirmed invalid/missing policy or explicit invalidation. Test silent-stall stale access and namespace-recreation exposure explicitly, followed by rejection after convergence. Healthy unchanged watches must not cause periodic authoritative verification reads. Failure to meet recovery and invalidation contracts blocks migration.
- Source: [Research — listing correctness and performance budgets](../research.md#group-3-openshift-platform-coupling); workload/latency targets were confirmed on 2026-10-25. Policy/namespace reuse was accepted on 2026-10-26; watch-driven eventual consistency without a fixed update deadline superseded its age limit on 2026-10-28. See the [HLD decision](hld.md#adr--watch-driven-policy-and-namespace-consistency).

### R05

- Requirement: Make authorization outcomes attributable to callers without exposing sensitive data.
- Success criterion: Audit records identify the verified caller, action, target, evaluated namespace UID, mode, policy/namespace versions, and outcome, including denials and policy failures. Logs exclude credentials, raw policy, and Secret data; monitoring distinguishes authorization outcomes without unbounded identity/target labels.
- Source: Audit outcome and criteria retained through the requirements review on 2026-10-25.

## Constraints and compatibility

- Initially enable backend authorization only with LDAP; reject incompatible configuration and preserve legacy authorization as the default under [shared R02](../requirements.md#r02). Keep HTTP shapes, quotas, resource operations, and both workload variants supported.
- Policy and backend credentials must be inaccessible through generic resource endpoints and user-controlled workloads. Configured namespace restrictions remain additional boundaries regardless of grants; backend Kubernetes permissions are limited to required operations.
- Use target-cluster policy without another database/policy service. Watch-driven eventual consistency is accepted with no fixed permission-update deadline: replicas may disagree and removed grants may authorize new decisions until convergence. Do not poll periodically or expire usable policy solely by age. Retry failed refreshes without requiring new events, expose watch/recovery failures, and keep serving the last valid cache during transport/API read failures. Fail closed before initial load, for confirmed missing/invalid required policy, and after explicit invalidation or shutdown; retries must never resurrect invalidated data. Silent watch stalls may remain undetected and do not trigger a time-based denial. Already-authorized operations may finish; token-carried groups retain authentication freshness limits, which do not bound policy revocation. Multi-resource/cross-cluster writes remain non-transactional. The HLD records the decision; the LLD defines recovery.
- Apply the same watch-driven contract independently to namespace identity/metadata. Deletion/recreation under the same name can let a former member read or modify replacement resources until convergence, with no fixed exposure bound and consequences that outlast convergence. Discovery may show deleted namespaces or omit new ones. Every decision checks availability/validity of each required collection; recovery of one cannot restore the other. No request-time namespace GET fallback is required. UID-specific memberships, namespace-delete UID preconditions, and configured/protected-name restrictions remain in force. An optional operator note may describe stopping access across all replicas and verifying reload if urgent revocation is needed; no emergency-revocation feature or rehearsal is required for rollout.

## Assumptions

- Operators control protected namespaces, credentials, admission/service-account isolation, and initial grants on every target cluster; stage/dev and production are separate deployments.
- Policy/namespace storage, per-replica memory, and background refreshes fit the initial deployment. The R04 workload is an acceptance target, not measured capacity; representative membership sizes, resources, dependency latency, bytes, CPU, and measured permission-update delay must accompany results.
