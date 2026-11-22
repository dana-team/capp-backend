# Backend policy authorization

This guide describes the backend-policy implementation in this source tree. It does not establish release availability or completed deployment validation; verify the deployed image and configuration before adoption. Only explicit `authorization.mode: backend-policy` is accepted. Legacy Kubernetes authorization is retired; omitted, null, empty, `kubernetes` and unknown values fail startup. Helm requires explicit `config.authorization.mode: backend-policy` too.

Backend-policy supports LDAP, OpenShift and passthrough authentication. Clients keep their existing provider login and bearer-token flows. The backend checks application assignments before using the configured backend identity on Kubernetes. It does not issue end-user Kubernetes credentials, create user RoleBindings, or impersonate users. Caller-token resource execution, impersonation and RoleBinding-based membership are no longer available.

## Configuration and namespace boundaries

Merge this example into a complete [server configuration](../README.md#configuration), including the settings for your chosen authentication provider. This example selects LDAP; OpenShift and passthrough use the same authorization settings:

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
        token: "" # Inject via CAPP_CLUSTERS_0_CREDENTIAL_INLINE_TOKEN
        insecure: false
    authorization:
      policyNamespace: capp-platform-system
      protectedNamespaces: [kube-system, kube-public, kube-node-lease]
      operatorUsers: [alice]
      operatorGroups: [platform-operators]
```

The mode is process-wide. Each cluster needs its own policy namespace and backend service-account credential. All three providers require `credential.inline.token` for every target; kubeconfig-only configuration is not accepted. Supply tokens from Secrets through `CAPP_CLUSTERS_N_CREDENTIAL_INLINE_TOKEN` or Helm `secret.clusterTokens[N]`. Passthrough additionally requires an HTTPS `inline.apiServer`, a valid base64 PEM `inline.caCert`, `inline.insecure: false`, and no `kubeconfigPath`. The policy namespace is automatically protected. Add all other credential/signing-key storage namespaces to `protectedNamespaces`; keep them outside user-manageable workloads.

`allowedNamespaces` is an outer boundary for both users and operators:

- `allowedNamespaces: []` adds no namespace-name restriction; role, managed-label, UID and protected-namespace checks still apply.
- `allowedNamespaces: [team-a, team-b]` permits only those exact names. Even a valid editor policy for `team-c` cannot grant backend access, and operators cannot create or administer `team-c` through the API.

The boundary also filters discovery and totals, aggregate reads, and both migration endpoints against their respective clusters. It does not restrict independent direct Kubernetes credentials.

Operator authority matches a verified username in `operatorUsers` **OR** any verified group in `operatorGroups`, exactly and case-sensitively. Empty lists grant no operator authority; blank entries are invalid. Operators administer namespaces, quotas and membership without resource assignments or ready observations, but gain no ordinary Capp, ConfigMap or Secret access. Existing mutation targets must be managed. PUT/PATCH require nonterminating namespaces; DELETE permits retries while terminating to finish guarded policy cleanup.

`restrictedBindingNamePrefixes` is required and must be non-null in backend-policy mode. There is no Go default. Deployment YAML/Helm recommends `["system:"]`; an explicit `[]` permits unrestricted prefixes, and empty-string entries are invalid. Matching is exact and case-sensitive for User and Group bindings. With `system:` restricted, service-account User names (`system:serviceaccount:...`) are excluded too. `ServiceAccount` is never a supported binding kind. These restrictions apply to policy bindings, not operator lists or authentication; disabling them does not change LDAP's own identity validation.

Keep mode, cluster mappings, trusted authorities/CA bundles, operator lists, allowlists, restrictions and API versions equivalent across replicas. LDAP replicas also require consistent issuer, audience and signing/verification keys. Configuration is loaded at startup: restart **all replicas** after changes; there is no hot reload. Supply lists and per-cluster settings in YAML/Helm (`config.authorization` and `config.clusters` in values). `CAPP_AUTHORIZATION_MODE` is a scalar override; there is no new indexed authorization environment-variable syntax.

## Provider verification and discovery

| Provider | Identity verification | Client flow |
|---|---|---|
| LDAP | Local JWT signature, issuer, audience and expiry verification; no TokenReview or directory call on resource requests. | Directory login; log in again after expiry, with no refresh. |
| OpenShift | In-cluster home TokenReview using the workload service account; never the remote execution targets. | Existing OAuth browser, password/direct-token and refresh flows. |
| Passthrough | TokenReview at each requested target using that target's configured backend token. | Supply an existing bearer token; login and refresh remain unsupported. |

OpenShift and passthrough accept only authenticated, nonanonymous identities with a nonempty username and no review status error. Empty groups are valid; names retain their exact spelling and case. Review requests verify TLS, reject redirects and have a 10-second deadline subject to request cancellation. They leave audiences unset, using the reviewing API's audience semantics; there is no token exchange or trust inferred from decoding a token.

OpenShift caches successful results for `auth.openshift.tokenCacheTTLSeconds` (default 60 seconds). Unexpired entries remain usable during a reviewer outage; a failed review cannot extend an expired entry. Passthrough retains results only within one HTTP request. Its target-specific identity and operator membership never carry over to another target, even when the caller supplies the same token.

For OpenShift/passthrough backend-policy requests, missing/malformed bearer credentials, explicit token rejection, empty usernames and anonymous identities return 401 `UNAUTHORIZED` with `request is not authenticated`. Reviewer transport, timeout, TLS, malformed-response or status errors, including review API 401/403 responses, return 503 `INTERNAL_ERROR` with `identity verification is unavailable`. A review API 401/403 indicates failure of the backend review credential, not rejection of the caller token. Upstream diagnostics are not returned. LDAP retains its [authentication errors](../README.md#ldap).

Passthrough global cluster discovery verifies every configured target with at most four concurrent reviews before publishing:

| Review results | `GET /api/v1/clusters` |
|---|---|
| Some accept, others explicitly reject | Apply visibility rules only to accepted targets. |
| All explicitly reject | 401 `UNAUTHORIZED`. |
| Some accept, others are unavailable | Apply visibility rules only to accepted targets; omit unavailable targets. |
| None accept and any reviewer is unavailable | 503 `INTERNAL_ERROR`; no partial list. |
| Accepted identities have no visible targets | 200 with `items: []`. |

Successful discovery may be incomplete: omitted unavailable targets are not identified in the response. Reviews still wait for their deadlines; target health flags do not replace identity verification.

Cancellation stops outstanding reviews and produces no partial list. After authentication, missing/unsynchronized policy observations can still omit nonoperator targets; this is distinct from a reviewer outage. Scoped discovery and aggregates use only the selected target's identity. Missing bearer credentials are rejected before target lookup; an unknown scoped target returns `CLUSTER_NOT_FOUND` rather than using another reviewer. Migration has a separate [destination concealment contract](migration.md#authorization).

## Roles and API access

Assignments match the verified username or groups without normalization or directory lookups during requests. Grants are additive: removing a direct editor grant does not revoke a matching group editor grant. Application role names do not refer to Kubernetes ClusterRoles.

Here `C` is `/api/v1/clusters/:cluster` and `N` is `C/namespaces/:namespace`.

| API | Backend-policy authority |
|---|---|
| GET cluster/namespace discovery | Eligible viewer/editor namespaces, or operator administrative discovery |
| GET `N` | Eligible viewer/editor or configured operator |
| POST `C/namespaces`; PUT/PATCH/DELETE `N` | Configured operator only |
| GET `N/{capps,configmaps,secrets,cfunc/pools,cfunc/functions}` and object detail | viewer or editor |
| GET `N/{configmaps,secrets}/names` | viewer or editor |
| GET `N/{configmaps,secrets}/:name/bound` | viewer or editor |
| GET Capp revisions and revision detail | viewer or editor |
| GET `C/{capps,configmaps,secrets,cfunc/pools,cfunc/functions}` | viewer or editor separately in each included namespace |
| POST resource collections; PUT/DELETE resource objects | editor |
| Capp revision rollback, Git sync/unsync, including Git effects in update/delete | editor; feature settings still apply |
| Capp migration | editor at source **and** destination, even for copy-only or same-cluster requests |
| Capp metrics APIs (`.../capps/:name/metrics[/:metric]`) | Denied, even for operators/editors; no Prometheus calls |
| cfunc function test call (`.../cfunc/functions/:name/test`) | editor |
| cfunc function logs (`.../cfunc/functions/:name/logs`) | Denied, even for operators/editors; pod logs are read with the caller's own cluster credential, which this mode does not have |

Viewer reads include **Secret payloads** exposed by the existing API. There is no per-object ACL or field redaction. Disabling a resource route does not disable supporting operations of another enabled API: migration still copies managed Secrets and ConfigMaps. Review these compound operations before adoption.

Health/readiness, API docs/assets, size definitions, authentication endpoints and the configured monitoring metrics endpoint remain public as registered. Monitoring metrics are distinct from denied Capp metrics. Disabled features stay disabled; unclassified operations receive no grant. Transport-only CORS preflight can return 204 without executing a handler.

Nonoperators see only eligible namespaces and clusters with visible namespaces. After provider verification succeeds, global cluster discovery omits targets with missing or initially unsynchronized stores and returns 200 with an empty `items` array when none are visible. Scoped discovery still returns 503 when required observations are unavailable; a fully observed but invisible cluster detail returns 404. Eligible unhealthy clusters may appear with `healthy: false`, while resource routes retain target-health checks. Operators can discover their configured clusters without synchronized observations and managed, nonprotected namespaces inside the allowlist, including terminating namespaces for inspection.

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

Use the existing namespace routes with a bearer token that verifies as an operator on the requested target. No policy endpoint is added. Examples below use `C=/api/v1/clusters/production` and `N=$C/namespaces/team-a` as path shorthand; send JSON with `Content-Type: application/json` and `Authorization: Bearer <provider-token>`.

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

Never send both `users` and `bindings`, even as empty arrays; explicit null membership, duplicate input users/tuples, unknown fields, invalid names/roles, trailing JSON and bodies over 512 KiB are rejected.

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

Follow the chart's [identity review permissions](../helm/capp-backend/README.md#identity-review-permissions): LDAP needs no TokenReview grant; OpenShift needs `create` on `authentication.k8s.io/tokenreviews` for the workload account at home only; passthrough needs it for each target backend identity. OpenShift with only remote targets gets a home-only `<fullname>-tokenreviewer` role/binding. Local aliases share one binding. Do not copy a local OpenShift review grant to remote execution targets.

The Capp workload selects `serviceAccount.name`, including for passthrough. `serviceAccount.create: false` uses an existing account while retaining workload/binding references; configure OAuth redirect annotations separately for an existing OpenShift account. Created accounts receive OAuth annotations only for OpenShift. Standalone `deploy/` manifests require separately provisioned permissions.

On each remote target, install the equivalent [ClusterRole](../helm/capp-backend/templates/clusterrole.yaml) with the provider-appropriate review permissions, then create its binding:

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
| `fission.io` Environments (only when `resources.cfunc.enabled`) | get, list, create, update, delete |
| `fission.io` Functions (only when `resources.cfunc.enabled`) | list |
| `fission.io` Functions, Packages, HTTPTriggers, TimeTriggers (only when `resources.cfunc.functions.enabled`) | get, list, create, update, patch, delete |
| `fission.io` Packages status (only when `resources.cfunc.functions.enabled`) | patch |
| Core Namespaces (only when `resources.cfunc.onboardNamespaces`) | patch |

Retain API discovery and `/version` reachability. The ClusterRoleBinding grants these permissions cluster-wide, including protected storage. Application policy checks, namespace restrictions and operator checks control user access; there is no separate Kubernetes permission boundary between resource execution and policy storage. Empty operator lists deny administration in the application without changing infrastructure grants. Namespace creation and policy writes do not create Kubernetes grants.

This mode needs no user impersonation, RoleBinding management, `bind`, TokenRequest, ServiceAccount/ClusterRole observation, or Pod/Job writes. Helm renders only the consolidated backend role and any required home reviewer role. Legacy impersonator and `capp-user` roles and the `rbac.openShiftProjects` setting are removed; OpenShift authentication remains supported. Keep backend identities and credentials in protected namespaces and prevent user workloads from selecting or mounting them.

Deployment owns token issuance, expiry monitoring and rotation. Update the inline credential Secret and restart every backend replica before expiry; there is no credential hot reload. Validate authorized requests against each target and replica before retiring the old credential. The backend does not renew tokens, fall back to another credential or replay failed operations. LDAP signing-key rotation is separate; follow the [LDAP key procedure](../README.md#ldap).

## Readiness, errors and revocation

`/readyz` returns 200 when at least one target is healthy and both its namespace and policy collections have initially synchronized; otherwise 503. Empty policies and individual pending UID associations do not prevent readiness. Public/auth routes and operator bootstrap can serve during initial sync. Readiness does not prove reviewer trust/permissions, execution grants, every target's availability, or completed adoption. Local passthrough trust errors fail startup; remote TokenReview availability is not a startup prerequisite.

After initial sync, transient watch errors retain the last valid state and emit degraded/recovery logs; they do not independently remove readiness. Confirmed invalid/deleted state and authoritative relists withdraw grants. There is no staleness timeout or fixed revocation deadline. In-flight compound operations may finish under their initial decision; aggregate publication has additional rechecks. Namespace recreation is also subject to observation delay.

| Condition | HTTP / envelope |
|---|---|
| Missing/invalid/expired authentication | 401, existing authentication error |
| OpenShift/passthrough reviewer unavailable (except partial global discovery above) | 503 `INTERNAL_ERROR`, `identity verification is unavailable`; no `POLICY_UNAVAILABLE` reason |
| Role/target/allowlist denial | 403 `FORBIDDEN`, `details.reason: POLICY_DENIED`, `operation is not permitted` |
| Required initial observations incomplete, or assigned scoped target awaiting UID association | 503 `INTERNAL_ERROR`, `details.reason: POLICY_UNAVAILABLE`, `authorization policy is unavailable` |
| Namespace administration invalid/oversized input | 400 / 413 |
| Administration absent target/policy | 404 |
| Ownership, revision, immutable-data or namespace-UID conflict | 409; operator namespace lists instead include policy ownership, termination or namespace-UID conflicts as invalid, noneditable items |
| Administration infrastructure authentication/permission/availability failure | 503 `INTERNAL_ERROR`; not a claim that the caller lacks operator authority |

Migration destinations use a stricter disclosure contract: unknown clusters, rejected destination tokens, unavailable destination reviewers, denied or excluded namespaces, missing/unsynchronized observations, and pending UID association all return the same 403 `FORBIDDEN` / `POLICY_DENIED` envelope. Only after destination policy authorization and the namespace boundary pass may an unhealthy destination return 503 `CLUSTER_UNHEALTHY`. Source identity verification and policy unavailability retain their distinct 503 contracts above. See [migration](migration.md).

After initial synchronization, invalid/missing policy, unmanaged/terminating/absent target and UID mismatch deny ordinary scoped access. Before synchronization, otherwise valid scoped targets can return unavailable even when protected; allowlist rejection is independent of readiness. Discovery excludes known ineligible/pending namespaces and can return empty success. Ordinary execution failures retain existing Kubernetes error mapping without fallback.

LDAP account/group changes affect newly issued JWTs; existing JWT identities remain valid until expiry or signing-key withdrawal. OpenShift identity changes remain subject to its review cache lifetime; passthrough reviews each request.

Authorization and namespace-administration audits identify verified caller, operation, target, outcome and policy identity/revision, plus `authProvider` and `identityAuthority` (the trusted reviewer endpoint or LDAP issuer). The existing `authority` field still describes policy/operator authorization. Discovery and migration attribute each principal to its own target; failed verification supplies no invented caller. Authority URLs exclude credentials, query strings and fragments. Never log bearer headers, TokenReview bodies, raw reviewer diagnostics, policies or Secret payloads.

## Provider adoption and qualification

Before sharing assignments, establish that matching usernames **and groups** represent the same principals across the intended authorities. Equal strings alone are not evidence. Passthrough can return different identities for the same token at different targets; OpenShift requires the configured OAuth server and in-cluster reviewer to belong to the intended home authority. Incompatible authorities need separate deployments and policy storage; incompatible populations must remain on an earlier compatible release; unresolved gaps block retirement.

- **LDAP:** verify issuer/audience/key consistency and required directory groups. Confirm required automation can explicitly renew expiring JWTs and that its identities pass LDAP validation.
- **OpenShift:** verify home-cluster reviewer trust and the workload account's review permission independently of remote execution grants. Exercise browser/password/direct-token login and refresh as used by the population, including cache expiry and reviewer outage. Inventory home identities, service accounts and custom Kubernetes grants before replacing authorization.
- **Passthrough:** verify HTTPS/CA trust and backend TokenReview permission on every target, including remote targets. Record the username/groups returned for each required human and automation token and its accepted audiences. Exercise mixed acceptance/rejection, all rejection and reviewer outage in global discovery; test migration with each endpoint's actual identity.

For every provider, review `system:` automation exclusions: the recommended binding restriction excludes service-account User names as well as system groups, and `ServiceAccount` bindings are unsupported. Do not assume an operator entry grants resource access. Only relax prefixes after reviewing the affected identities and grants; otherwise keep incompatible automation on an earlier compatible release and block retirement. Custom/global grants are not automatically translated into viewer/editor roles.

Keep code evidence separate from a population qualification record. Record the deployed image/revision, provider, home/target authorities and trust, actual reviewer/execution permission results, reviewed users/groups/automation, required allows and denies, replica consistency, token rotation, direct-access cleanup and rollback after revocations. Required checks include viewer Secret reads, editor resource/Git operations, denied Capp metrics, operator-only administration, filtered discovery/aggregates and both migration endpoints. Verify revoked access remains revoked after rollback. Local tests and rendered charts establish neither live-cluster compatibility nor completed adoption; without operator-run evidence, the population remains unverified.

## Manual cutover and rollback

Legacy Kubernetes authorization, caller impersonation and caller-token resource execution are retired in this source tree. There is no same-release mode switch or automatic grant conversion. Existing backend-policy assignments need no conversion. Publishing or merging a build does not establish migration completion.

### Upgrade prerequisites and cutover

1. Inventory every affected provider and population, including custom/global grants, human users, service accounts and automation. Compare required allows and denies against the API map: viewer Secret reads, editor workload/Git effects, both migration endpoints, operator-only administration and excluded Capp metrics. Resolve identity-equivalence, automation restrictions and feature gaps before retirement; otherwise stay on the earlier compatible release.
2. Record the candidate image digest/revision, chart version and complete configuration, tested environments/authorities, population scope, reviewed allows/denies and evidence links in an operator-maintained qualification record. Include an earlier rollback image/chart/configuration and its tested recovery results. **No rollback release is qualified by this guide.** A proposed candidate remains unqualified until population-specific upgrade and rollback evidence, including intervening revocations, is recorded and reviewed.
3. Back up the current release configuration, policy data with namespace UID associations, and owned grant manifests securely. Inventory independently held credentials and grants separately. Provision protected storage, backend credentials and retained grants on every target; preserve home OpenShift or per-target passthrough TokenReview permission. Replace any review permission supplied by the old impersonator role **before** removing it.
4. Prepare explicit `authorization.mode: backend-policy` (`config.authorization.mode` in Helm), prefix restrictions and per-target policy/operator/allowlist settings. Retain provider credentials and trust. Label intended namespaces and create reviewed assignments when migrating legacy access; RoleBindings alone grant no API access. Remove obsolete `rbac.openShiftProjects` overrides.
5. Validate the candidate in the qualified environment, then pause incoming traffic for the upgrade. Roll out the selected image/chart/configuration and restart all replicas. Check readiness and actual reviewer/execution permissions on every target, allowed/denied operations, operator recovery, observation convergence, credential rotation and required Git/migration workflows before returning traffic. A successful Helm upgrade or readiness probe alone is insufficient.

### Obsolete-grant cleanup

Chart-owned removal and independent access cleanup have different owners:

- Compare the previous release manifest with the new render. Helm stops rendering the legacy impersonator ClusterRole/ClusterRoleBinding and `capp-user` ClusterRole; a normal upgrade removes chart-owned resources absent from the new manifest. Inspect the cluster afterward for retained/orphaned resources and their ownership before manual deletion. Keep the service account, consolidated `<fullname>-role` and its bindings, and any required `<fullname>-tokenreviewer` role/binding.
- Operators inventory and remove obsolete independently installed impersonation grants, `capp-user` bindings, direct user tokens and other bypass access only after reviewing their consumers and recovery needs. Remote grants and credentials are not cleaned up by the home chart. Do not delete a shared role/account merely because its name resembles a legacy resource. Replace required TokenReview grants first and preserve backend observation, administration and execution permissions.
- Verify required allows and denies after cleanup, including direct Kubernetes access and workload service-account access. Record removed/retained grants, credential revocations and their effects in the qualification record. Application policy changes neither revoke independent credentials nor restore deleted RBAC.

### Earlier-release recovery

1. Keep traffic paused. Select the **qualified earlier compatible image, chart and complete configuration** from the population record; if none has been qualified, recovery remains unverified and retirement must not proceed. The retired release rejects `kubernetes`, so changing its mode cannot restore legacy behavior.
2. Reconcile the saved grants and credentials against all intervening policy edits, group changes and revocations. Never restore a snapshot blindly: removed access must remain denied. If the earlier release will use Kubernetes authorization, restore only reviewed caller-token RBAC for passthrough or impersonation plus user/group RBAC for OpenShift/LDAP. Restore required authentication-review permissions and compatible client authentication. If it will use backend-policy, retain its compatible assignments and backend grants instead.
3. Deploy that earlier image/chart with its reviewed provider and authorization configuration. Recreate only the required removed grants, roll out all replicas and verify credentials, reviewer trust, readiness and actual operations. A Helm revision rollback alone does not reconcile revocations or independently managed grants.
4. Re-test the population's required allows **and denies**, especially access revoked since cutover, on every target before restoring traffic. Record the recovery revision/configuration, cleanup effects and revocation reconciliation evidence. Failed or unresolved compatibility checks keep traffic paused.
