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
  "deleteSource": false,
  "targetHostname": "new-app.example.com"
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `targetCluster` | string | yes | Name of the target cluster. |
| `targetNamespace` | string | yes | Namespace on the target cluster. |
| `deleteSource` | boolean | no | Delete the source Capp after successful creation on the target. Defaults to `false`. When the delete fails, the response is still `200` with `"sourceDeleted": false` — the Capp exists on both clusters and you can retry the delete separately. |
| `targetHostname` | string | conditional | Replacement hostname for the target Capp. Required on copy (`deleteSource=false`) when the source has a custom hostname; must differ from the source. Optional on move (`deleteSource=true`) to rename the hostname. Must not be set when the source has no hostname. |

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

When a Capp has a `routeSpec.hostname`, copying it without a different
`targetHostname` is rejected to prevent duplicate DNS CNAME records across
clusters.

**Copy (`deleteSource=false`):** `targetHostname` is required and must differ
from the source hostname. The target Capp is created with the new hostname and
no bypass annotation — the operator webhook validates global uniqueness.

**Move (`deleteSource=true`):** `targetHostname` is optional. If omitted, the
existing hostname is preserved via a bypass annotation that is removed after
the source is deleted. If `targetHostname` is provided and differs from the
source, the bypass annotation is skipped and the webhook validates the new
hostname.

`targetHostname` must not be set when the source has no hostname.

> **Prerequisite:** The `container-app-operator` must include bypass annotation
> support. See
> [container-app-operator#730](https://github.com/dana-team/container-app-operator/pull/730).

---

## Error responses

| Status | Condition |
|---|---|
| 400 | Invalid request body, same cluster and namespace as source, or invalid `targetHostname` (missing on copy with hostname, same as source on copy, or set when source has no hostname) |
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

### Copy a Capp with a custom hostname

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targetCluster": "production-west", "targetNamespace": "team-beta", "targetHostname": "new-app.example.com"}' \
  https://capp.example.com/api/v1/clusters/production-east/namespaces/team-alpha/capps/my-app/migrate
```

### Copy without a custom hostname

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
