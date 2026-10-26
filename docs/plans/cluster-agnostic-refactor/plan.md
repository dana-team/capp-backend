## Goal

Make capp-backend cluster-agnostic so it can be deployed on N clusters behind a single global DNS, with any request succeeding regardless of which instance handles it.

## Requirements

- [Shared requirements](requirements.md) — R01–R02, topic scope, and compatibility constraints.
- [Authentication requirements](authentication/requirements.md) — R01–R03, inheriting the shared requirements.
- [Authorization requirements](authorization/requirements.md) — R01–R05, inheriting the shared requirements.

## Plan

Based on [research](research.md). Designs are grouped by domain, with phase numbers matching the delivery order below:

1. **Authentication — LDAP backend and deployment (merged)** — Delivered LDAPS authentication, backend tokens, API contracts, Helm configuration, impersonation permissions, and API/operator documentation alongside existing OpenShift auth. Also removed OpenShift-only service-account/impersonation gates and made Projects permissions independent of auth mode. Research: [Group 1](research.md#group-1-authentication--openshift-oauth-is-cluster-bound), [Group 5](research.md#group-5-cli-and-api-surface).
2. **Authentication — LDAP CLI and automation (next)** — Deliver secure unattended login, token import/storage, expiry handling, and CLI documentation against the merged backend API. The backend is independently usable through the HTTPS API; LDAP CLI support remains in this phase. Frontend implementation remains outside this repository.
3. **Decouple authorization from cluster-local identities** — Introduce opt-in backend authorization for LDAP, keeping capp-backend the sole Kubernetes client in the new mode; users access platform operations only through capp-backend. Other authentication modes retain legacy authorization. [Group 2](research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities)
4. **Extend backend authorization to all authentication modes** — Apply the authorization engine introduced for LDAP to every supported authentication mode. Supply verified identities from each mode to the same policy engine and execute approved platform operations with backend credentials. Define identity mapping, configuration compatibility, migration, and validation for each mode before rollout.
5. **Decouple from OpenShift platform requirements** — [Group 3](research.md#group-3-openshift-platform-coupling); retain Projects until authorization and listing performance are resolved.
6. **GitOps multi-instance support** — [Group 4](research.md#group-4-gitops--single-instance-concurrency-model)
7. **Add ADFS authentication** — [Open questions](research.md#open-questions); future authentication work, outside the two LDAP phases, integrating with the shared authorization engine.

LDAP authorization is covered by its [HLD](authorization/hld.md) and [phase specification](authorization/phase-3/lld.md). Subsequent authorization, platform, and GitOps work will receive domain design updates and phase specifications when designed.
