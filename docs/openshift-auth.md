
# OpenShift Authentication

This guide covers deploying `capp-backend` with OpenShift OAuth authentication. It is intended for cluster administrators deploying the CAPP platform into an OpenShift cluster.

## How it works

In `openshift` mode the backend delegates all authentication to the OpenShift OAuth server. It never issues its own JWTs and keeps no session state, so multiple replicas work without a shared store.

**Browser login flow:**
1. The frontend calls `GET /api/v1/auth/openshift/authorize` to get the OAuth authorize URL.
2. The user is redirected to the OpenShift OAuth server and approves access.
3. OpenShift redirects back to the frontend with an authorization code.
4. The frontend exchanges the code via `POST /api/v1/auth/openshift/callback`. The backend calls OpenShift's token endpoint and returns an access token + refresh token to the browser.

**Per-request authorization:**
The backend verifies bearer tokens through the in-cluster home TokenReview API, caching successful identities for `tokenCacheTTLSeconds`. It checks application assignments using the verified username/groups and executes authorized operations with each target's backend credential, without impersonation. The home workload account needs TokenReview permission; remote execution accounts do not. Confirm the OAuth server and home reviewer represent the same intended authority.

---

## Prerequisites

Before installing the Helm chart you need:

1. An OpenShift cluster where the backend will run.
2. An `OAuthClient` resource registered in that cluster (see below).
3. The external URL of the OpenShift API server (`oc whoami --show-server`).
4. The frontend deployed and its callback URL known (e.g. `https://console.example.com/auth/callback`).
5. For each **managed cluster** (clusters the backend will forward Capp requests to): a backend service account with consolidated resource/policy permissions, protected policy storage and a target-issued token.
6. Explicit `config.authorization.mode: backend-policy`, reviewed assignments and configured namespace operators. Follow the [upgrade prerequisites](backend-policy.md#manual-cutover-and-rollback) for an existing installation.

### Register an OAuthClient

Create the following resource in the OpenShift cluster where the backend runs:

```yaml
apiVersion: oauth.openshift.io/v1
kind: OAuthClient
metadata:
  name: capp-backend          # Must match config.auth.openshift.clientId
secret: "<client-secret>"     # Choose a strong random value
redirectURIs:
  - "https://console.example.com/auth/callback"   # Must exactly match config.auth.openshift.redirectUri
grantMethod: auto
```

```bash
oc apply -f oauthclient.yaml
```

> The `redirectURIs` list must **exactly** match the `redirectUri` you configure in the Helm chart. A mismatch will cause the OAuth exchange to fail.

### Prepare backend credentials for each managed cluster

Follow [infrastructure onboarding](backend-policy.md#infrastructure-onboarding-and-rotation) to create a service account in protected storage and install the consolidated backend role/binding on each target. The chart binds the local target only when its endpoint is `https://kubernetes.default.svc`; other targets require separately installed grants. The home workload account always needs `create` on `authentication.k8s.io/tokenreviews`, including when every execution target is remote.

Issue a target token according to your deployment's expiry and rotation policy, supply it through `secret.clusterTokens`, and restart every replica after rotation. The backend does not renew tokens. Do not provision impersonation grants. Replace any home TokenReview grant inherited from an old impersonator role before removing that role; see [cleanup](backend-policy.md#obsolete-grant-cleanup).

---

## Deploy with Helm

### Minimal values file

Create a `values-openshift.yaml` with the required fields. This example uses the local target so the chart binds its backend role. Replace the CA placeholder and operator identity with reviewed values. The container-app-operator and protected policy namespace must exist; create initial assignments as described in the [policy guide](backend-policy.md#policies-and-namespace-identity).

```yaml
config:
  authorization:
    mode: backend-policy
    restrictedBindingNamePrefixes: ["system:"]
  auth:
    mode: "openshift"
    openshift:
      apiServer: "https://api.my-cluster.example.com:6443"
      clientId: "capp-backend"
      redirectUri: "https://console.example.com/auth/callback"
      scopes:
        - "user:info"
        - "user:check-access"

  clusters:
    - name: "production"
      displayName: "Production Cluster"
      credential:
        inline:
          apiServer: "https://kubernetes.default.svc"
          caCert: "<base64-encoded PEM CA bundle>"
          insecure: false
          token: ""   # overridden by secret.clusterTokens[0]
      allowedNamespaces: []
      authorization:
        policyNamespace: capp-platform-system
        protectedNamespaces: [kube-system, kube-public, kube-node-lease]
        operatorUsers: [alice]
        operatorGroups: []

secret:
  openshiftClientSecret: "<client-secret>"
  clusterTokens:
    - "<sa-token-for-production-cluster>"
```

### Install

```bash
helm install capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
  --namespace capp-platform-system \
  --create-namespace \
  --values values-openshift.yaml
```

Or pass sensitive values directly on the CLI to avoid storing secrets in a file:

```bash
helm install capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
  --namespace capp-platform-system \
  --create-namespace \
  --values values-openshift.yaml \
  --set secret.openshiftClientSecret="<client-secret>" \
  --set secret.clusterTokens[0]="<sa-token>"
```

### What the chart creates in openshift mode

| Resource | Name | Purpose |
|---|---|---|
| `Capp` | `capp-backend` | Backend workload managed by container-app-operator |
| `ConfigMap` | `capp-backend` | Non-sensitive configuration |
| `Secret` | `capp-backend` | `openshiftClientSecret`, cluster tokens |
| `ServiceAccount` | `capp-backend` | Home reviewer and local backend identity |
| `ClusterRole` | `capp-backend-role` | Backend execution, observation, administration and local home review |
| `ClusterRoleBinding` | `capp-backend-role` | Binds local target grants to the configured account |

---

For remote-only targets the chart creates a separate home `capp-backend-tokenreviewer` role/binding. Remote grants are installed separately. Existing accounts (`serviceAccount.create: false`) still receive workload/binding references; configure their OAuth redirect annotations separately.

## Add a managed cluster

To add a cluster after initial deployment:

1. Create the service account, ClusterRole, and ClusterRoleBinding on the target cluster (see [Prerequisites](#prerequisites)).

2. Issue a target token and arrange rotation before its expiry:
   ```bash
   kubectl create token capp-backend -n capp-platform-system --duration=8760h --context=<target-cluster>
   ```

3. Add the cluster to your complete values and upgrade. Preserve existing target endpoints, credentials and policy settings; this example keeps production local and adds remote staging:
   ```yaml
   config:
     clusters:
       - name: "production"
         displayName: "Production Cluster"
         credential:
           inline:
             apiServer: "https://kubernetes.default.svc"
             caCert: "<base64-encoded PEM CA bundle>"
             insecure: false
             token: ""
         allowedNamespaces: []
         authorization:
           policyNamespace: capp-platform-system
           protectedNamespaces: [kube-system, kube-public, kube-node-lease]
           operatorUsers: [alice]
           operatorGroups: []
       - name: "staging"
         displayName: "Staging Cluster"
         credential:
           inline:
             apiServer: "https://api.staging.example.com:6443"
             caCert: "<base64-encoded PEM CA bundle>"
             insecure: false
             token: ""
         allowedNamespaces: []
         authorization:
           policyNamespace: capp-platform-system
           protectedNamespaces: [kube-system, kube-public, kube-node-lease]
           operatorUsers: [alice]
           operatorGroups: []

   secret:
     clusterTokens:
       - "<sa-token-for-production>"
       - "<sa-token-for-staging>"
   ```

   ```bash
   helm upgrade capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
     --namespace capp-platform-system \
     --values values-openshift.yaml
   ```

> Tokens in `secret.clusterTokens` are positional — index `N` maps to `config.clusters[N]`. Keep them in the same order.

---

## Configuration reference

### `config.auth.openshift`

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `apiServer` | string | Yes | — | External URL of the OpenShift API server. Obtain with `oc whoami --show-server`. |
| `caCert` | string | No | `""` | Base64-encoded PEM CA bundle for TLS to the OpenShift API server. Leave empty to use the system trust store. |
| `clientId` | string | Yes | — | Name of the `OAuthClient` resource registered in OpenShift. |
| `redirectUri` | string | Yes | — | Frontend callback URL. Must exactly match one of the `redirectURIs` in the `OAuthClient`. |
| `scopes` | []string | No | `["user:info", "user:check-access"]` | Scopes requested during OAuth login; application assignments govern resource access. |
| `tokenCacheTTLSeconds` | int | No | `60` | How long a validated token's identity is cached in-memory per pod before re-validation. |

### `secret`

| Field | Type | Required | Description |
|---|---|---|---|
| `openshiftClientSecret` | string | Yes (openshift mode) | Secret of the `OAuthClient`. Injected as `CAPP_AUTH_OPENSHIFT_CLIENTSECRET`. |
| `clusterTokens` | []string | Yes | Bearer tokens for the service account on each managed cluster. Positional: index `N` maps to `config.clusters[N]`. Injected as `CAPP_CLUSTERS_<N>_CREDENTIAL_INLINE_TOKEN`. |

### `config.clusters[]`

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Identifier used as the `:cluster` path parameter in API URLs. |
| `displayName` | string | No | Human-readable label shown in the UI. |
| `credential.inline.apiServer` | string | Yes | Kubernetes API server URL for this cluster. |
| `credential.inline.caCert` | string | No | Base64-encoded PEM CA bundle. Supply the target CA bundle when not trusted by system roots. |
| `credential.inline.token` | string | — | Leave empty — overridden by `secret.clusterTokens[N]`. |
| `allowedNamespaces` | []string | No | Restrict API access to these namespaces. Empty adds no name restriction; policy and protected-namespace checks still apply. |

### `serviceAccount`

| Field | Type | Default | Description |
|---|---|---|---|
| `create` | bool | `true` | Whether the chart creates the service account. Set to `false` if you manage it externally. |
| `name` | string | `"capp-backend"` | Home workload service account and local backend grant subject. |

---

## Verify

1. **Check the backend is running:**
   ```bash
   kubectl get pods -n capp-platform-system
   kubectl logs -n capp-platform-system <backend-pod>
   ```

2. **Query the auth mode endpoint:**
   ```bash
   curl https://api.capp-backend.example.com/api/v1/auth/mode
   # Expected: {"mode":"openshift"}
   ```

3. **Fetch the authorize URL:**
   ```bash
   curl https://api.capp-backend.example.com/api/v1/auth/openshift/authorize
   # Expected: {"authorizeUrl":"https://oauth-openshift.apps.my-cluster.example.com/oauth/authorize?..."}
   ```

4. **Open the authorize URL in a browser** and complete the login flow. If you reach the frontend callback page without an error, the full OAuth round-trip is working.

---

## Troubleshooting

### OAuth callback fails with `invalid_redirect_uri`

The `redirectUri` in `config.auth.openshift.redirectUri` does not exactly match any entry in the `OAuthClient`'s `redirectURIs` list. Check both values for trailing slashes, http vs https, or port differences.

### Authentication or resource calls fail after login

A `401 UNAUTHORIZED` means caller credentials were rejected. A `503` with `identity verification is unavailable` means the home reviewer is unavailable or its workload credential lacks TokenReview permission. Verify that home account's `create` permission on `tokenreviews.authentication.k8s.io` using administrator credentials; a remote execution token cannot repair home review access.

A `403 POLICY_DENIED` requires checking assignments, exact identity, namespace boundaries and UID association. If an authorized operation fails at Kubernetes, check the target backend token and consolidated grants. Update the credential Secret and roll out every Capp/Knative replica after rotation. Neither Helm `--wait` nor readiness proves reviewer or execution authority; validate actual calls before restoring traffic.

### `x509: certificate signed by unknown authority`

Set `config.auth.openshift.caCert` to the base64-encoded CA bundle of your OpenShift cluster:

```bash
oc config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}'
```

Paste the output as the value of `config.auth.openshift.caCert` in your values file and upgrade.

### Backend pod crashes on startup

Check the logs for config validation errors:

```bash
kubectl logs -n capp-platform-system <backend-pod>
```

Common causes:
- `config.authorization.mode` is not explicitly `backend-policy`, or per-target policy settings are missing.
- `secret.openshiftClientSecret` is empty — required when `auth.mode` is `openshift`.
- `config.auth.openshift.apiServer` or `clientId` or `redirectUri` is not set.
- A cluster token at index `N` is empty while `config.clusters[N]` is defined.

For legacy-release recovery, use the [earlier-release procedure](backend-policy.md#earlier-release-recovery); switching this release to `kubernetes` fails. No rollback candidate is qualified by this guide.
