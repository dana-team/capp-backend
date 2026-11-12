# Backend authorization configuration

See [values.yaml](values.yaml) for available settings. `kubernetes` is the default
authorization mode. `backend-policy` requires LDAP authentication, a policy
namespace, and one inline backend credential for each target cluster.

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
