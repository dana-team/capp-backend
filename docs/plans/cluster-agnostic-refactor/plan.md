## Goal

Make capp-backend cluster-agnostic so it can be deployed on N clusters behind a single global DNS, with any request succeeding regardless of which instance handles it.

## Plan

Based on [research](research.md).

The research's proposed OpenShift auth removal is deferred: phase 1 preserves existing OpenShift authentication and adds selectable LDAP search-and-bind authentication for web users and unattended CLI automation. The HLD/LLD supersede the earlier REST-service proposal; auth retirement requires separate work.

1. **Add LDAP auth for web and automation alongside existing OpenShift auth** — [Group 1](research.md#group-1-authentication--openshift-oauth-is-cluster-bound), [Group 5](research.md#group-5-cli-and-api-surface)
2. **Decouple authorization from cluster-local identities** — [Group 2](research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities)
3. **Remove OpenShift platform coupling** — [Group 3](research.md#group-3-openshift-platform-coupling)
4. **GitOps multi-instance support** — [Group 4](research.md#group-4-gitops--single-instance-concurrency-model)
