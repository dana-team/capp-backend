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
On every API call the backend validates the bearer token via the Kubernetes TokenReview API, extracts the user's identity (username + groups), and **impersonates** that user when forwarding requests to each managed cluster. Each managed cluster must have a pre-configured service account token with impersonation permissions — the user's own OpenShift identity is never forwarded directly to remote clusters.

---

## Prerequisites

Before installing the Helm chart you need:

1. An OpenShift cluster where the backend will run.
2. An `OAuthClient` resource registered in that cluster (see below).
3. The external URL of the OpenShift API server (`oc whoami --show-server`).
4. The frontend deployed and its callback URL known (e.g. `https://console.example.com/auth/callback`).
5. For each **managed cluster** (clusters the backend will forward Capp requests to): a service account with impersonation permissions and a long-lived token for it.

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

### Prepare impersonation credentials for each managed cluster

For each cluster that the backend will manage, create a service account and grant it impersonation permissions:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: capp-backend
  namespace: capp-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capp-backend-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
  - apiGroups: ["authentication.k8s.io"]
    resources: ["tokenreviews"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: capp-backend-impersonator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: capp-backend-impersonator
subjects:
  - kind: ServiceAccount
    name: capp-backend
    namespace: capp-system
```

Then generate a long-lived token:

```bash
kubectl create token capp-backend -n capp-system --duration=8760h
```

> The Helm chart creates this service account and RBAC automatically on the **cluster where the backend is deployed**. For any **additional managed clusters** you must create them manually as shown above.

---

## Deploy with Helm

### Minimal values file

Create a `values-openshift.yaml` with the required fields:

```yaml
config:
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
          apiServer: "https://api.my-cluster.example.com:6443"
          token: ""   # overridden by secret.clusterTokens[0]
      allowedNamespaces: []

secret:
  openshiftClientSecret: "<client-secret>"
  clusterTokens:
    - "<sa-token-for-production-cluster>"
```

### Install

```bash
helm install capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
  --namespace capp-system \
  --create-namespace \
  --values values-openshift.yaml
```

Or pass sensitive values directly on the CLI to avoid storing secrets in a file:

```bash
helm install capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
  --namespace capp-system \
  --create-namespace \
  --values values-openshift.yaml \
  --set secret.openshiftClientSecret="<client-secret>" \
  --set secret.clusterTokens[0]="<sa-token>"
```

### What the chart creates in openshift mode

| Resource | Name | Purpose |
|---|---|---|
| `Deployment` | `capp-backend` | Backend pods |
| `Service` | `capp-backend` | ClusterIP service on port 8080 |
| `ConfigMap` | `capp-backend` | Non-sensitive configuration |
| `Secret` | `capp-backend` | `openshiftClientSecret`, cluster tokens |
| `ServiceAccount` | `capp-backend` | Identity used for impersonation |
| `ClusterRole` | `capp-backend-impersonator` | Grants impersonate + tokenreviews permissions |
| `ClusterRoleBinding` | `capp-backend-impersonator` | Binds the role to the service account |

---

## Add a managed cluster

To add a cluster after initial deployment:

1. Create the service account, ClusterRole, and ClusterRoleBinding on the target cluster (see [Prerequisites](#prerequisites)).

2. Generate a long-lived token:
   ```bash
   kubectl create token capp-backend -n capp-system --duration=8760h --context=<target-cluster>
   ```

3. Add the cluster to your values and upgrade:
   ```yaml
   config:
     clusters:
       - name: "production"
         displayName: "Production Cluster"
         credential:
           inline:
             apiServer: "https://api.prod.example.com:6443"
             token: ""
         allowedNamespaces: []
       - name: "staging"
         displayName: "Staging Cluster"
         credential:
           inline:
             apiServer: "https://api.staging.example.com:6443"
             token: ""
         allowedNamespaces: []

   secret:
     clusterTokens:
       - "<sa-token-for-production>"
       - "<sa-token-for-staging>"
   ```

   ```bash
   helm upgrade capp-backend oci://ghcr.io/dana-team/helm-charts/capp-backend \
     --namespace capp-system \
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
| `scopes` | []string | No | `["user:info", "user:check-access"]` | OAuth scopes to request. The defaults are sufficient for impersonation. |
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
| `credential.inline.caCert` | string | No | Base64-encoded PEM CA bundle. Omit when using the in-cluster service account. |
| `credential.inline.token` | string | — | Leave empty — overridden by `secret.clusterTokens[N]`. |
| `allowedNamespaces` | []string | No | Restrict API access to these namespaces. Empty means all namespaces are allowed. |

### `serviceAccount`

| Field | Type | Default | Description |
|---|---|---|---|
| `create` | bool | `true` | Whether the chart creates the service account. Set to `false` if you manage it externally. |
| `name` | string | `"capp-backend"` | Name of the service account used for impersonation. |

---

## Verify

1. **Check the backend is running:**
   ```bash
   kubectl get pods -n capp-system
   kubectl logs -n capp-system deploy/capp-backend
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

### `404` on `/api/v1/auth/mode`

The running image predates the `/mode` endpoint (added in v0.0.3). Update the image tag in your values and upgrade:

```bash
helm upgrade capp-backend ... --set image.tag=v0.0.3
```

### OAuth callback fails with `invalid_redirect_uri`

The `redirectUri` in `config.auth.openshift.redirectUri` does not exactly match any entry in the `OAuthClient`'s `redirectURIs` list. Check both values for trailing slashes, http vs https, or port differences.

### `Unauthorized` on API calls after login

The service account token may have expired or lack impersonation permissions. Verify:

```bash
# Check token validity
kubectl auth can-i impersonate users --as=system:serviceaccount:capp-system:capp-backend

# Re-generate if needed
kubectl create token capp-backend -n capp-system --duration=8760h
```

Then update the secret and restart the deployment:

```bash
kubectl patch secret capp-backend -n capp-system \
  --type=json \
  -p='[{"op":"replace","path":"/data/clusterToken-0","value":"'$(echo -n "<new-token>" | base64)'"}]'
kubectl rollout restart deploy/capp-backend -n capp-system
```

### `x509: certificate signed by unknown authority`

Set `config.auth.openshift.caCert` to the base64-encoded CA bundle of your OpenShift cluster:

```bash
oc config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}'
```

Paste the output as the value of `config.auth.openshift.caCert` in your values file and upgrade.

### Backend pod crashes on startup

Check the logs for config validation errors:

```bash
kubectl logs -n capp-system deploy/capp-backend
```

Common causes:
- `secret.openshiftClientSecret` is empty — required when `auth.mode` is `openshift`.
- `config.auth.openshift.apiServer` or `clientId` or `redirectUri` is not set.
- A cluster token at index `N` is empty while `config.clusters[N]` is defined.
