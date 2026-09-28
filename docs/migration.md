# Capp Migration Guide

Migrate a Capp and its dependent resources (Secrets, ConfigMaps) from one
cluster or namespace to another.

---

## Endpoint

```
POST /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name/migrate
```

The `:cluster` and `:namespace` path parameters identify the **source** Capp.
The target is specified in the request body.

---

## Request

```json
{
  "targetCluster": "production-west",
  "targetNamespace": "team-beta",
  "deleteSource": false
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `targetCluster` | string | yes | Name of the target cluster. |
| `targetNamespace` | string | yes | Namespace on the target cluster. |
| `deleteSource` | boolean | no | Delete the source Capp after successful creation on the target. Defaults to `false`. When the delete fails, the response is still `200` with `"sourceDeleted": false` — the Capp exists on both clusters and you can retry the delete separately. |

---

## Response

```json
{
  "name": "my-app",
  "sourceCluster": "production-east",
  "sourceNamespace": "team-alpha",
  "targetCluster": "production-west",
  "targetNamespace": "team-beta",
  "sourceDeleted": false,
  "copiedSecrets": ["db-creds"],
  "copiedConfigMaps": ["app-config"]
}
```

`copiedSecrets` and `copiedConfigMaps` list the managed resources that were
copied to the target namespace. If a resource with the same name already
exists on the target, the request fails with `409 Conflict` before any writes.

---

## DNS and hostname migration

When a Capp has a `routeSpec.hostname`, the migrate handler adds a bypass
annotation so the target cluster's webhook does not reject the create due to
the hostname already being in use. When `deleteSource` is `true` and the
source is successfully deleted, the annotation is automatically removed.

> **Prerequisite:** The `container-app-operator` must include bypass annotation
> support. See
> [container-app-operator#730](https://github.com/dana-team/container-app-operator/pull/730).

---

## Error responses

| Status | Condition |
|---|---|
| 400 | Invalid request body or same cluster and namespace as source |
| 403 | Target namespace denied, user lacks RBAC, or target webhook rejected the Capp |
| 404 | Source Capp not found, target cluster not configured, or target namespace missing |
| 409 | Capp, Secret, or ConfigMap already exists on the target |
| 500 | Internal server error |
| 503 | Target cluster is unhealthy |

**Webhook rejections (403):** The target cluster may reject the Capp due to
hostname pattern mismatch, scale limit violations, or Kafka consumer limits.
The webhook error message is returned directly. Adjust the Capp spec or the
target cluster's `CappConfig` to resolve.

---

## Limitations

- All managed Secrets and ConfigMaps in the source namespace are copied — the
  namespace is the ownership boundary, not individual Capp references.
- Status and Knative revision history are not migrated — the target Capp
  starts fresh.
- If a dependent resource create fails mid-batch, previously created resources
  remain on the target and must be cleaned up manually.

---

## Examples

### Migrate without deleting the source

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targetCluster": "production-west", "targetNamespace": "team-beta"}' \
  https://capp.example.com/api/v1/clusters/production-east/namespaces/team-alpha/capps/my-app/migrate
```

### Migrate and delete the source

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targetCluster": "production-west", "targetNamespace": "team-beta", "deleteSource": true}' \
  https://capp.example.com/api/v1/clusters/production-east/namespaces/team-alpha/capps/my-app/migrate
```
