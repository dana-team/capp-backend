# Backend policy authorization

This guide describes the backend-policy implementation in this source tree. It does not establish release availability or completed deployment validation; verify the deployed image and configuration before adoption. The default remains `authorization.mode: kubernetes`.

Backend-policy supports LDAP authentication. Users log in with directory credentials and send the returned access JWT to the backend. The backend checks application assignments before using the configured backend identity on Kubernetes. It does not issue end-user Kubernetes credentials, create user RoleBindings, or impersonate users. Kubernetes-mode authentication and RBAC remain available.

## Configuration and namespace boundaries

Merge this example into a complete [server configuration](../README.md#configuration), including LDAP keys and directory settings:

```yaml
auth:
  mode: ldap
authorization:
  mode: backend-policy
  restrictedBindingNamePrefixes: ["system:"]
clusters:
  - name: production
    allowedNamespaces: [team-a, team-b]
    credential:
      inline:
        apiServer: https://api.production.example.com:6443
        caCert: "<base64-encoded PEM>"
        token: "<backend-token>"
    authorization:
      policyNamespace: capp-platform-system
      protectedNamespaces: [kube-system, kube-public, kube-node-lease]
      operatorUsers: [alice]
      operatorGroups: [platform-operators]
```

The mode is process-wide. Each cluster needs its own policy namespace and backend service-account credential. LDAP still requires `credential.inline.token` for every cluster; kubeconfig-only configuration is not accepted. The policy namespace is automatically protected. Add all other credential/signing-key storage namespaces to `protectedNamespaces`; keep them outside user-manageable workloads.

`allowedNamespaces` is an outer boundary for both users and operators:

- `allowedNamespaces: []` adds no namespace-name restriction; role, managed-label, UID and protected-namespace checks still apply.
- `allowedNamespaces: [team-a, team-b]` permits only those exact names. Even a valid editor policy for `team-c` cannot grant backend access, and operators cannot create or administer `team-c` through the API.

The boundary also filters discovery and totals, aggregate reads, and both migration endpoints against their respective clusters. It does not restrict independent direct Kubernetes credentials.

Operator authority matches a verified username in `operatorUsers` **OR** any verified group in `operatorGroups`, exactly and case-sensitively. Empty lists grant no operator authority; blank entries are invalid. Operators administer namespaces, quotas and membership without resource assignments or ready observations, but gain no ordinary Capp, ConfigMap or Secret access. Existing mutation targets must be managed. PUT/PATCH require nonterminating namespaces; DELETE permits retries while terminating to finish guarded policy cleanup.

`restrictedBindingNamePrefixes` is required and must be non-null in backend-policy mode. There is no Go default. Deployment YAML/Helm recommends `["system:"]`; an explicit `[]` permits unrestricted prefixes, and empty-string entries are invalid. Matching is exact and case-sensitive for User and Group bindings. With `system:` restricted, service-account User names (`system:serviceaccount:...`) are excluded too. `ServiceAccount` is never a supported binding kind. These restrictions apply to policy bindings, not operator lists or authentication; disabling them does not change LDAP's own identity validation.

Keep mode, cluster mappings, operator lists, allowlists, restrictions and API versions consistent across replicas. Configuration is loaded at startup: restart **all replicas** after changes; there is no hot reload. Supply lists and per-cluster settings in YAML/Helm (`config.authorization` and `config.clusters` in values). `CAPP_AUTHORIZATION_MODE` is a scalar override; there is no new indexed authorization environment-variable syntax. In Kubernetes mode, backend-policy settings are inactive.

## Roles and API access

Assignments match the verified username or groups without normalization or directory lookups during requests. Grants are additive: removing a direct editor grant does not revoke a matching group editor grant. Application role names do not refer to Kubernetes ClusterRoles.

Here `C` is `/api/v1/clusters/:cluster` and `N` is `C/namespaces/:namespace`.

| API | Backend-policy authority |
|---|---|
| GET cluster/namespace discovery | Eligible viewer/editor namespaces, or operator administrative discovery |
| GET `N` | Eligible viewer/editor or configured operator |
| POST `C/namespaces`; PUT/PATCH/DELETE `N` | Configured operator only |
| GET `N/{capps,configmaps,secrets}` and object detail | viewer or editor |
| GET `N/{configmaps,secrets}/names` | viewer or editor |
| GET `N/{configmaps,secrets}/:name/bound` | viewer or editor |
| GET Capp revisions and revision detail | viewer or editor |
| GET `C/{capps,configmaps,secrets}` | viewer or editor separately in each included namespace |
| POST resource collections; PUT/DELETE resource objects | editor |
| Capp revision rollback, Git sync/unsync, including Git effects in update/delete | editor; feature settings still apply |
| Capp migration | editor at source **and** destination, even for copy-only or same-cluster requests |
| Capp benchmark and Capp metrics/dashboard/query APIs | Denied, even for operators/editors; no Prometheus calls |

Viewer reads include **Secret payloads** exposed by the existing API. There is no per-object ACL or field redaction. Disabling a resource route does not disable supporting operations of another enabled API: migration still copies managed Secrets and ConfigMaps. Review these compound operations before adoption.

Health/readiness, API docs/assets, size definitions, authentication endpoints and the configured monitoring metrics endpoint remain public as registered. Monitoring metrics are distinct from denied Capp metrics. Disabled features stay disabled; unclassified operations receive no grant. Transport-only CORS preflight can return 204 without executing a handler.

Nonoperators see only eligible namespaces and clusters with visible namespaces. Global cluster discovery omits targets with missing or initially unsynchronized stores and returns 200 with an empty `items` array when none are visible. Scoped discovery still returns 503 when required observations are unavailable; a fully observed but invisible cluster detail returns 404. Eligible unhealthy clusters may appear with `healthy: false`, while resource routes retain target-health checks. Operators can discover their configured clusters without synchronized observations and managed, nonprotected namespaces inside the allowlist, including terminating namespaces for inspection.

Aggregate lists return authorized resources in namespace/name order with filtered totals. Access changes or namespace/policy replacement observed before publication exclude affected results; any page failure fails the whole response. After initial synchronization, an empty eligible set returns empty success without resource reads. ConfigMap/Secret managed-resource filters remain in effect.

## Policies and namespace identity

Each target has one canonical policy ConfigMap per workload namespace in protected policy storage:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: capp-assignment-team-a
  namespace: capp-platform-system
  labels:
    rcs.dana.io/authorization: backend-policy
data:
  policy.json: |
    {"version":1,"namespace":"team-a","bindings":[
      {"kind":"User","name":"alice","role":"viewer"},
      {"kind":"Group","name":"developers","role":"editor"}
    ]}
```

The payload must use version 1, match the canonical namespace/name, contain a bindings array and only supported fields. Only User/Group and viewer/editor are accepted. Duplicate tuples, extra data keys, binary data, and payloads over 512 KiB are rejected. Empty bindings grant nothing. The same identity can have both roles.

Binding names must be nonblank, without leading/trailing Unicode whitespace or Unicode control characters (category Cc). Names are rejected rather than trimmed or case-normalized. Internal spaces are accepted and significant (`Platform Developers`); zero-width format characters such as U+200B (category Cf) remain unchanged. This is not a ban on every invisible character. Configured prefix restrictions apply in addition to this format contract. Invalid stored policies withdraw earlier grants once observed.

Ordinary access also requires `dana.io/capp-ns=true`, a nonterminating, nonprotected namespace, and a matching namespace UID stored in the policy's `rcs.dana.io/namespace-uid` annotation. A background worker associates an externally supplied unassociated policy with its first eligible namespace. API-created policies already carry the UID captured during creation. The worker patches metadata only and never overwrites an existing association or manages RBAC.

Removing/restoring the managed label does not reset the UID. A recreated namespace has a different UID and cannot inherit the old policy after replacement is observed. Recovery requires explicit administrator deletion/recreation of the canonical policy after reviewing the new namespace; clearing bindings or PUT replacement does not reset identity. There is no policy-only DELETE endpoint or automatic orphan cleanup.

## Existing namespace API

Use the existing namespace routes with an operator's LDAP JWT. No policy endpoint is added. Examples below use `C=/api/v1/clusters/production` and `N=$C/namespaces/team-a` as path shorthand; send JSON with `Content-Type: application/json` and `Authorization: Bearer <access-jwt>`.

Create through `POST C/namespaces`:

```json
{"name":"team-a","quota":{"cpu":"4","memory":"8Gi","pods":50},"users":["alice"]}
```

`users` is shorthand for direct User/editor grants. Omitted membership creates an empty policy; an existing namespace returns 409 and is never adopted by POST.

Use `PUT N` for a full binding replacement (also the repair path for a malformed owned policy):

```json
{"bindings":[{"kind":"User","name":"alice","role":"viewer"},{"kind":"Group","name":"developers","role":"editor"}],"policyResourceVersion":"12345"}
```

Use the version returned by a fresh GET, not the illustrative value above. `PUT {"users":["bob"]}` instead replaces only direct User/editor bindings, preserving User/viewer and all Group bindings. `PUT {"users":[]}` removes direct editors; `PUT {"bindings":[]}` removes all grants. Omitted membership leaves assignments unchanged. PUT can initialize a missing policy; omit the revision when doing so. Quota decreases below current usage are rejected.

Use `PATCH N` to add tuples:

```json
{"bindings":[{"kind":"User","name":"carol","role":"viewer"}],"policyResourceVersion":"12345"}
```

PATCH preserves other grants; existing tuples are no-ops. `users` adds direct editors instead. PATCH cannot remove grants or change quota, requires an existing policy (404 if missing), and rejects invalid policy with 409. Omitted membership is a no-op.

Never send both `users` and `bindings`, even as empty arrays; explicit null membership, duplicate input users/tuples, unknown fields, invalid names/roles, trailing JSON and bodies over 512 KiB are rejected. Kubernetes mode retains its RoleBinding behavior and explicitly rejects `bindings`.

Namespace reads include quota, direct-editor `users`, full `bindings` and `policyResourceVersion` when available. Groups are not expanded. `canEdit` (item) and `canCreate` (collection) indicate operator authority and are false for nonoperators.

Operators reading malformed owned policy receive `policyState: "invalid"` and its revision with membership omitted. These policies remain editable through full `bindings` PUT; users-only replacement cannot preserve unknown bindings safely. Policy ownership, termination or namespace-UID conflicts require manual recovery and return 409 for namespace detail reads and mutations. In operator namespace lists, a conflicting included namespace remains visible with `policyState: "invalid"` and `canEdit: false`, omitting membership and policy revision. Other namespaces remain visible; storage failures still fail the whole collection request.

### Conflicts, ownership and recovery

A supplied `policyResourceVersion` must match current storage, including on no-op PATCH; supplying one against a missing policy conflicts. Without it, the server applies the proposal to a fresh read and still uses conditional writes, but cannot detect changes since a client's earlier GET. Concurrent changes return 409 without silent retries. UID-association writes can also change the revision: GET again and reconsider the intended edit.

Immutable policy ConfigMaps reject data changes with 409 before quota/membership writes. Unchanged-data no-ops and quota-only updates remain possible. Explicit administrative recovery is required; the backend does not delete/recreate immutable policies for you.

Choose an owner for policy data: API/manual administration or GitOps. API changes do not update a GitOps policy repository, so reconciliation may overwrite them. GitOps must preserve the UID annotation and unrelated metadata and must not repeatedly recreate policies or erase associations. Restriction changes can invalidate existing policies; repair them with valid full bindings before relying on their grants.

Namespace, quota and policy writes are not a transaction. POST writes namespace, optional quota, then policy; PUT writes quota before membership. Errors after a completed step include `error.details.completedSteps` (for example `namespaceCreated`, `quotaSaved`). Inspect GET and complete recovery through PUT; blindly repeating POST returns an existing-name conflict. Success means **saved**, not observed by all replicas.

DELETE refuses a namespace containing Capps. After namespace deletion is accepted, it conditionally removes the captured owned policy; replacement or mismatched policies are never removed. Cleanup failure can report `namespaceDeletionAccepted`. Retrying while terminating can finish guarded cleanup; once the namespace is absent, the API returns not found and an administrator must clean up orphan policy storage manually.

## Infrastructure onboarding and rotation

Provision one backend service-account identity per target. Observation, administration and authorized resource operations use the existing `credential.inline.token`. Remote credentials must be issued by the target cluster. The [Helm templates](../helm/capp-backend/templates/) render one `<fullname>-role` ClusterRole and, for the `https://kubernetes.default.svc` target, one ClusterRoleBinding to the configured service account.

On each remote target, install the equivalent [ClusterRole](../helm/capp-backend/templates/clusterrole.yaml), then create its binding:

```sh
kubectl --context '<target-context>' create clusterrolebinding capp-backend-role \
  --clusterrole=capp-backend-role \
  --serviceaccount=capp-platform-system:capp-backend
```

Replace the context and example names for your target. The role name must match the installed ClusterRole, and the service-account namespace and name must identify the account used by that target's configured token. Keep that account in a protected namespace.

| Resources | Required Kubernetes verbs |
|---|---|
| `rcs.dana.io` Capps | get, list, create, update, patch, delete |
| `rcs.dana.io` CappRevisions | list |
| Core ConfigMaps | get, list, watch, create, update, patch, delete |
| Core Secrets | get, list, create, update, delete |
| Core Namespaces | get, list, watch, create, delete |
| Core ResourceQuotas | get, create, update |

Retain API discovery and `/version` reachability. The ClusterRoleBinding grants these permissions cluster-wide, including protected storage. Application policy checks, namespace restrictions and operator checks control user access; there is no separate Kubernetes permission boundary between resource execution and policy storage. Empty operator lists deny administration in the application without changing infrastructure grants. Namespace creation and policy writes do not create Kubernetes grants.

This mode needs no user impersonation, RoleBinding management, `bind`, TokenRequest, ServiceAccount/ClusterRole observation, or Pod/Job writes. Helm renders the consolidated role instead of the impersonator and `capp-user` roles in backend-policy mode; Kubernetes-mode roles remain unchanged. Keep backend identities and credentials in protected namespaces and prevent user workloads from selecting or mounting them.

Deployment owns token issuance, expiry monitoring and rotation. Update the inline credential Secret and restart every backend replica before expiry; there is no credential hot reload. Validate authorized requests against each target and replica before retiring the old credential. The backend does not renew tokens, fall back to another credential or replay failed operations. LDAP signing-key rotation is separate; follow the [LDAP key procedure](../README.md#ldap).

## Readiness, errors and revocation

`/readyz` returns 200 when at least one target is healthy and both its namespace and policy collections have initially synchronized; otherwise 503. Empty policies and individual pending UID associations do not prevent readiness. Public/auth routes and operator bootstrap can serve during initial sync. Readiness does not prove execution credentials or infrastructure grants work, nor that every target is ready for global discovery.

After initial sync, transient watch errors retain the last valid state and emit degraded/recovery logs; they do not independently remove readiness. Confirmed invalid/deleted state and authoritative relists withdraw grants. There is no staleness timeout or fixed revocation deadline. In-flight compound operations may finish under their initial decision; aggregate publication has additional rechecks. Namespace recreation is also subject to observation delay.

| Condition | HTTP / envelope |
|---|---|
| Missing/invalid/expired authentication | 401, existing authentication error |
| Role/target/allowlist denial | 403 `FORBIDDEN`, `details.reason: POLICY_DENIED`, `operation is not permitted` |
| Required initial observations incomplete, or assigned scoped target awaiting UID association | 503 `INTERNAL_ERROR`, `details.reason: POLICY_UNAVAILABLE`, `authorization policy is unavailable` |
| Namespace administration invalid/oversized input | 400 / 413 |
| Administration absent target/policy | 404 |
| Ownership, revision, immutable-data or namespace-UID conflict | 409; operator namespace lists instead include policy ownership, termination or namespace-UID conflicts as invalid, noneditable items |
| Administration infrastructure authentication/permission/availability failure | 503 `INTERNAL_ERROR`; not a claim that the caller lacks operator authority |

Migration destinations use a stricter disclosure contract: unknown clusters, denied or excluded namespaces, missing/unsynchronized observations, and pending UID association all return the same 403 `FORBIDDEN` / `POLICY_DENIED` envelope. Only after destination policy authorization and the namespace boundary pass may an unhealthy destination return 503 `CLUSTER_UNHEALTHY`. Source policy unavailability retains the 503 contract above. See [migration](migration.md).

After initial synchronization, invalid/missing policy, unmanaged/terminating/absent target and UID mismatch deny ordinary scoped access. Before synchronization, otherwise valid scoped targets can return unavailable even when protected; allowlist rejection is independent of readiness. Discovery excludes known ineligible/pending namespaces and can return empty success. Ordinary execution failures retain existing Kubernetes error mapping without fallback.

Directory account/group changes affect newly issued JWTs; existing JWT identities remain valid until expiry or signing-key withdrawal. Audits identify verified caller, operation, target, outcome and policy identity/revision; do not log bodies, policy contents, Secret data, tokens or raw credentials.

## Manual cutover and rollback

1. Compare required allows and denies against the API map, especially viewer Secret reads, editor workload creation, compound Git effects, both migration endpoints, operator-only administration and excluded benchmark/metrics features. Stay on Kubernetes mode if this model cannot preserve the reviewed access boundaries.
2. Review existing direct user tokens/RoleBindings and workload service accounts/admission restrictions. Remove or restrict unrelated access explicitly where necessary: application policies neither revoke direct Kubernetes access nor remove privileges available to a workload. Keep backend identities and signing keys inaccessible to user workloads.
3. Provision protected storage, backend credentials and consolidated grants on every target. Label intended namespaces, create policies and configure operator/allowlist/restriction settings consistently.
4. Validate the chosen build in a deployment before routing traffic: test role/target denials despite sufficient execution permissions, operator recovery, token rotation, observation convergence on every replica, and ordinary resource/Git/migration workflows.
5. Perform an explicit coordinated switch to backend-policy and restart all replicas. Check readiness, then the required per-target operations; do not rely on readiness as a permission test. There is no automatic cutover or fallback.
6. For rollback, first reconcile Kubernetes-mode grants and credentials against intervening policy edits/revocations. Restore the required impersonation/RBAC infrastructure and compatible client authentication, switch to `authorization.mode: kubernetes`, and restart all replicas before returning traffic. Backend policies do not control Kubernetes-mode access.
