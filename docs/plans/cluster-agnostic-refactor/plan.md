## Goal

Make capp-backend cluster-agnostic so it can be deployed on N clusters behind a single global DNS, with any request succeeding regardless of which instance handles it.

## Requirements

- **Cluster independence:** operate consistently regardless of the hosting cluster.
- **Compatibility:** preserve existing functionality and access controls through a smooth migration.
- **Scope:** remain focused on platform management; introduce no new product capabilities as part of this refactor.
- **Dependencies:** minimize reliance on additional external services.
- **Scalability:** support growth across instances and clusters; disaster recovery is outside this refactor's scope.

## Plan

Based on [research](research.md). Designs are grouped by domain, with phase numbers local to each domain. The delivery order is:

1. **Authentication phase 1 — LDAP backend and deployment** — Deliver directory authentication, backend tokens, API contracts, Helm configuration, impersonation permissions, and API/operator documentation alongside existing OpenShift auth. Research: [Group 1](research.md#group-1-authentication--openshift-oauth-is-cluster-bound), [Group 5](research.md#group-5-cli-and-api-surface).
2. **Authentication phase 2 — LDAP CLI and automation** — Deliver secure unattended login, token import/storage, expiry handling, and CLI documentation against the phase 1 API. Phase 1 is independently usable through the HTTPS API; LDAP CLI support is delivered here. Frontend implementation remains outside this repository.
3. **Decouple authorization from cluster-local identities** — [Group 2](research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities)
4. **Decouple from OpenShift platform requirements** — [Group 3](research.md#group-3-openshift-platform-coupling); retain Projects until authorization and listing performance are resolved.
5. **GitOps multi-instance support** — [Group 4](research.md#group-4-gitops--single-instance-concurrency-model)
6. **Add ADFS authentication** — [Open questions](research.md#open-questions); future authentication work, outside the two LDAP phases.

Authorization, platform, and GitOps work will receive their own domain HLDs and phase specifications when designed.
