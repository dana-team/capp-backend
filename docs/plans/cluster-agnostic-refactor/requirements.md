**Purpose:** Define shared outcomes, constraints, and success criteria for the cluster-agnostic refactor; domain requirements, architecture, delivery sequencing, and implementation detail have separate owners.

## Goal

Support interchangeable capp-backend instances across hosting clusters while preserving platform functionality and effective access through an additive migration.

## Scope

- Shared obligations apply across the refactor. [Authentication requirements](authentication/requirements.md) and [authorization requirements](authorization/requirements.md) specialize them without duplicating them.
- The current feature scope adds LDAP authentication and backend authorization alongside existing modes. Making the new feature the sole option and removing legacy modes are future work, as confirmed during requirements review on 2026-10-25.
- Non-goals: new product capabilities outside platform management, disaster recovery, and provisioning global DNS/routing. Later platform, GitOps, and ADFS work retains its roadmap scope; this extraction does not invent its acceptance criteria.

## Requirements

IDs are stable within each requirements file; references identify both the file and ID.

### R01

- Requirement: Platform behavior must be independent of the hosting instance in a consistently configured cluster-independent deployment, supporting growth across instances and managed clusters.
- Success criterion: With the same verified identity, target state, policy, and available dependencies, equivalent requests through different instances produce equivalent results and access decisions without sticky sessions. This does not require retained legacy authentication modes to become cluster-independent.
- Source: [Research goal](research.md#goal), [authentication boundary](research.md#group-1-authentication--openshift-oauth-is-cluster-bound).

### R02

- Requirement: Introduce the new modes additively, preserving existing platform functionality and effective application access through explicit migration.
- Success criterion: Existing OpenShift/passthrough and Kubernetes authorization remain supported; deployments can retain legacy behavior. Migration preserves reviewed allowed and denied operations, and consumers move only when compatible. Documented upgrade prerequisites are applied rather than silently losing access.
- Source: [Research constraints](research.md#constraints), [authorization compatibility and migration](research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities); additive scope confirmed during requirements review on 2026-10-25.

## Constraints and compatibility

- Minimize additional external services; shared dependencies are permitted. Cluster independence does not require dependency failover.
- Instances behind one interchangeable endpoint use compatible authentication, authorization, identity mapping, and configuration. Existing modes may remain on separate deployments; no automatic provider or authorization fallback is introduced. See [research compatibility](research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities).
- The no-user-Kubernetes-access boundary belongs to the new authorization mode, as specified in [authorization R01](authorization/requirements.md#r01); it does not remove legacy modes from the supported scope.

## Assumptions

- Operators provide equivalent target-cluster connectivity and compatible configuration across instances; canonical identity mapping and clocks are consistent.
- Platform listing budgets beyond the agreed authorization scope, GitOps conflict handling, ADFS protocol choices, and long-lived-token behavior require their own scope decisions; research gaps alone do not establish new obligations. See [research groups](research.md#gap-groups) and [open questions](research.md#open-questions).
