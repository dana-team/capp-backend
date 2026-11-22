# Backend authorization configuration

See [values.yaml](values.yaml) for available settings. `kubernetes` is the default
authorization mode. `backend-policy` requires a policy namespace and one inline
backend credential for each target cluster. The chart provisions permissions for
LDAP, OpenShift and passthrough. Server configuration validation permits all three
providers with backend-policy when their required settings are supplied. When
overriding the server image version, ensure it supports the selected combination.

The Capp selects `serviceAccount.name` for backend-policy and for OpenShift/LDAP
Kubernetes authorization. Set `serviceAccount.create: false` to use an existing
account; the workload and bindings still reference that name. Only OpenShift
accounts created by the chart receive OAuth redirect annotations. For a pre-existing
OpenShift account, configure those annotations separately.

Keep authorization settings identical across replicas and restart all replicas
after changing them. Prefix restrictions apply to policy bindings, not login
identities or configured operators.

## Backend credentials and permissions

Supply each target-issued service-account token through `secret.clusterTokens`
in the same order as `config.clusters`. Observation, administration and authorized
resource operations share this credential. Deployment owns token issuance and
rotation; update the Secret and restart every replica before the old token expires.

In backend-policy mode the chart renders one `<fullname>-role` ClusterRole with
the required resource, namespace, quota and policy permissions. If any target uses
`https://kubernetes.default.svc`, it also renders one ClusterRoleBinding to
`serviceAccount.name` in `namespace`; that target's token must identify this account.
For remote targets, install the equivalent role and binding on the target cluster
and provide its endpoint, CA and service-account token. Follow the
[remote binding instructions](../../docs/backend-policy.md#infrastructure-onboarding-and-rotation).

The binding grants infrastructure access cluster-wide, including protected storage.
User access remains controlled by application policies, operator checks, protected
namespaces and `allowedNamespaces`. Operator lists do not change the role's grants.
Kubernetes-mode RBAC is unchanged. Keep backend identities and credentials outside
user-manageable namespaces and prevent workloads from using them.

## Identity review permissions

Backend-policy grants depend on the authentication provider:

| Provider | TokenReview `create` permission |
| --- | --- |
| LDAP | None; identities are verified locally. |
| OpenShift | The workload service account on the home cluster only. |
| Passthrough | Each target's configured backend identity. |

For OpenShift with a local target, the consolidated role includes the home review
grant. Multiple local aliases still produce only one binding. With only remote
targets, the chart instead adds a home-only `<fullname>-tokenreviewer` role and
binding; the rendered execution role has no review permission. Do not copy the
home review grant to remote OpenShift execution identities.

For passthrough, the consolidated role includes review permission. Operators must
install this role and a binding for the configured backend identity on every
remote target, just as they provision its resource permissions. Configure each
reviewer endpoint with HTTPS, a valid `caCert`, and `insecure: false`. Reviews use
the existing backend token; there is no separate reviewer credential.

Backend-policy roles grant neither impersonation nor user-RBAC management.
Rendered grants do not certify live permissions; verify each identity's permissions
on its intended cluster before deployment.
