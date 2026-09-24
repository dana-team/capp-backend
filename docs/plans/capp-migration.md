# Capp Cross-Cluster Migration — Design Justification

## Problem

Users need to move a running Capp from one managed cluster to another. Today this is manual and error-prone: export the spec, recreate on the target, copy Secrets/ConfigMaps, clean up the source.

## Why not sync-to-git

The existing `/sync` endpoint pushes a Helm values file to Git for a GitOps engine to consume. It was considered for migration but rejected:

- **Indirect & slow** — Git push → GitOps poll → reconcile, with no inline status feedback.
- **No dependent resources** — only writes `{name, namespace, spec}`; Secrets and ConfigMaps are missing.
- **No source cleanup** — one-way write; nothing deletes or disables the source Capp.
- **No success signal** — caller gets a commit SHA, not confirmation the Capp is running on the target.
- **Not yet mature** — sync-to-git is not fully tested or deployed; building on it compounds risk.

Sync-to-git remains valuable for backup/DR but is not the right foundation for live migration.

## Chosen approach: direct API migration

A new endpoint reads the live Capp from the source cluster and creates it on the target via two `ClientFor()` calls in the same request.

- **Synchronous** — immediate success/failure response.
- **Composable** — dependent resources can be copied in the same request.
- **Minimal new code** — reuses existing cluster client, DTO conversion, and handler patterns.

## Scope

| Phase | What |
|---|---|
| **1** | Migrate the Capp resource (read → create → optionally delete source). |
| **2** | Copy referenced Secrets and ConfigMaps to the target cluster. |
| **3 (optional)** | Sync migrated Capp to Git for audit trail. |