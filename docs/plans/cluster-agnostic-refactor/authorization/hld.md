**Purpose:** Describe the architecture, components, flows, and design rationale; detailed API contracts, implementation steps, and per-phase tests are outside this document's scope.

## Summary

For backend maintainers and deployment operators: add opt-in backend authorization for LDAP while production retains existing authentication and authorization. Stage/dev validates the new mode before migration. Scope follows the [plan](../plan.md) and [research](../research.md). The LLD will define schemas, interfaces, and migration procedures.

## Goals / Non-goals

- Preserve platform functionality, editable memberships, and effective access controls through migration; equivalent requests use identical policy semantics across instances, with bounded differences while policy changes propagate.
- In the new mode, users access only capp-backend; backend service identities alone hold Kubernetes credentials and permissions for platform operations. Minimize additional services.
- Exclude authentication redesign, broader OpenShift cleanup, GitOps concurrency, new product capabilities, and disaster recovery.

## Architecture

- **Mode boundary:** legacy behavior remains the default for all existing authentication modes. Enable new authorization only with LDAP; reject incompatible configuration. Supporting it with OpenShift or other identity providers is future work, keeping this implementation small. Passthrough remains legacy-only because it forwards caller Kubernetes credentials.
- **Authorizer:** use embedded [Casbin scoped RBAC](https://casbin.apache.org/docs/rbac-with-domains/) to evaluate identity, action, and target scope. This reuses an authorization library without another service, rather than maintaining a custom evaluator. A small backend integration loads policy, supplies verified LDAP identities, and enforces decisions on every operation, including privileged handlers and cross-cluster actions. Deny unknown operations; never fall back to legacy enforcement.
- **Policy authority:** protected ConfigMaps in a backend-only namespace on each managed cluster remain authoritative. Each backend replica maintains one shared policy store per target cluster. Initial loading, watch-triggered refreshes, and periodic authoritative verification produce validated policy generations; compile only when policy records change. Requests capture a generation and keep verified caller identity separate. Every decision checks policy availability and age before evaluating locally; no per-request policy reads or decision cache. Namespace identity remains a direct request-time read. Detect concurrent edit conflicts and retain Kubernetes availability dependence once cached policy expires.
- **Execution:** the existing cluster manager supplies scoped backend credentials for approved operations. Kubernetes RBAC bounds backend capabilities; application policy bounds caller access. Removing user-binding dependencies makes complete enforcement and caller auditing essential.

## ADR — Shared policy with bounded freshness

- **Status:** accepted on 2026-10-26; the user accepts a maximum permission-update delay of five seconds. Implementation and stage validation remain pending.
- **Context:** listing every policy record and rebuilding Casbin on every resource request ties policy traffic and compilation to request volume even when permissions are unchanged.
- **Decision:** share validated policy generations per target/replica, refreshed in the background. Use Kubernetes change notifications for prompt refresh and periodic authoritative reads to enforce a maximum policy age of five seconds. Keep caller identity request-local and namespace metadata authoritative. Publish complete generations atomically and retain one generation per request/target, subject to freshness checks before each decision.
- **Consequences:** ordinary requests avoid policy reads and compilation; replicas may temporarily disagree, including allowing a revoked grant within the five-second window. Cache lifecycle, memory, concurrency, and refresh health become backend responsibilities. Expired policy fails closed; cached policy does not provide indefinite outage tolerance. A successful membership write does not mean every replica has observed it.
- **Alternatives:** per-request authoritative reads retain stronger visibility but repeat policy transfer; compilation reuse alone saves CPU but not reads; polling alone is simpler but delays all updates until the next poll. Watch notifications alone do not establish a maximum age. A separate policy service adds operational dependencies without removing the freshness decision.
- **References:** [requirements](requirements.md#constraints-and-compatibility), [refresh contract](phase-3/lld.md#2-policy-records-and-casbin-integration), [Kubernetes LIST/WATCH](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes), [client-go informer consistency](https://pkg.go.dev/k8s.io/client-go/tools/cache#SharedInformer).

## Data model

- Bind canonical users/groups to actions within cluster/namespace scope; preserve distinctions between resource access, namespace administration, and membership delegation.
- Combine matching user/group allow grants within their explicit scopes; deny without a matching grant. Cluster grants cover namespaces only when explicitly scoped to do so. Delegation grants specify which permissions the caller may assign; resource access alone grants no implicit delegation.
- Membership operations use application policy in the new mode and RoleBindings in legacy mode. General resource endpoints cannot expose or modify protected policy.
- Namespace memberships belong to a namespace instance; deletion invalidates them, and recreating the same name must not inherit stale grants. The LLD defines identity binding and cleanup.
- The fixed Casbin model ships embedded in the backend binary and changes with a rebuild; roles and bindings remain runtime policy in ConfigMaps. The LLD specifies the model and ConfigMap mapping, including delegation limits and namespace identity; the backend retains ownership of policy validation, persistence, and enforcement.

## Workflow / Sequence

- **Resource access:** verify identity → resolve target and configured namespace restrictions → evaluate policy → execute with backend credentials. Restrictions remain an additional boundary.
- **Discovery:** apply action/scope checks to namespace and aggregate resource lists, including Capps, ConfigMaps, and Secrets; return only authorized items, totals, and capabilities. Projects/SSAR filtering remains in legacy mode.
- **Membership changes:** authorize the caller's delegation rights, then persist the scoped policy change. Publish grants only after required namespace/quota setup succeeds.
- **Cross-cluster operations:** authorize source and destination actions before side effects; deny if either check fails. Writes remain non-transactional across clusters.

## Rollout stages

1. **Coexisting authorization modes** — ship legacy behavior by default; explicitly enable the new mode in stage/dev with protected policy storage and backend credentials. Keep replicas behind each endpoint on consistent mode/configuration.
2. **Controlled migration** — map effective permissions, including existing membership-delegation rights and custom grants. Briefly freeze membership changes for final reconciliation before switching traffic; no automatic dual-write synchronization is assumed. Require acceptable listing latency and policy-read volume, with budgets defined in the LLD. Retire user Kubernetes grants to complete the backend-only boundary. Rollback must reconcile policy changes and may require restoring reviewed legacy grants, temporarily restoring legacy access.

## Security / Failure modes

- Fail closed before initial policy loading, on observed missing/malformed required policy, or once policy exceeds its five-second maximum age; distinguish dependency failures from denials. A transient refresh failure permits use only of previously validated policy still within that age. Failed policy writes report failure.
- Protect credentials and policy from resource endpoints and user-controlled workloads; only authorized membership flows may alter policy.
- Changes affect new authorization decisions within five seconds, or those decisions fail closed. Already-authorized operations may finish. Token-carried groups retain existing authentication freshness limits.

## Observability

- Audit caller, action, target, mode, policy version, and outcome; correlate service-identity operations without logging credentials. Monitor policy age, refresh failures/duration, compilation, policy bytes, denials, latency, and configuration drift.

## Assumptions

- Stage/dev and production use separate deployments; instances share canonical identity mapping, cluster identifiers, and compatible configuration.
- Operators provision protected policy storage and initial administrative grants. Existing custom permissions require inventory and explicit mapping; they are not assumed equivalent to default CAPP membership.
