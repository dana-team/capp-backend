# Release notes

## Unreleased LDAP authentication

Direct LDAP is the chosen way to remove hosting-cluster authentication coupling while reusing existing AD. Preserve current platform-management behavior and effective permissions; this release adds no external identity service, authentication database, long-lived-token management, or revocation API. Existing OpenShift/passthrough modes remain supported.

### Required upgrade changes

**OpenShift permissions:** The chart no longer enables Projects API permissions
based on authentication mode. To preserve project access through `capp-user`,
add this top-level Helm value before upgrading:

```yaml
rbac:
  openShiftProjects: true
```

The default is `false`; omitting this setting can remove namespace listing
access for users relying on the role. Leave it disabled on non-OpenShift clusters.

**Earlier LDAP builds:** StartTLS is no longer supported. Replace `ldap://`
URLs with a working `ldaps://` endpoint in `config.auth.ldap.url` (Helm) or
`auth.ldap.url` (backend). Configure a trusted CA if needed; `ldap://` now
prevents startup.

### Before switching to LDAP

- Configure directory settings, token issuer/audience, and Ed25519 signing keys.
  Helm requires `secret.ldapBindPassword` and `secret.ldapSigningPrivateKey`.
- Provide an inline service-account token with user/group impersonation
  permissions and matching user/group role bindings on every managed cluster.
  Kubeconfig-only credentials are insufficient.
- Clients must use password login over HTTPS and sign in again after token
  expiry (15 minutes by default). Existing OpenShift/Kubernetes tokens, refresh,
  and OpenShift browser login are unsupported in LDAP mode.
- `cappctl login` does not yet support LDAP. Confirm client compatibility before
  switching modes.

### Staged cutover and rollback

1. Record the current mode, configuration, route, required clients, canonical
   usernames/groups, namespace visibility, and representative allowed/denied
   operations. Retain the prior deployment/configuration for rollback.
2. Qualify LDAP through a separate route using the same target clusters and
   equivalent permissions. Verify identities and direct/nested group coverage
   against actual RoleBindings; configure required Projects access explicitly.
3. Verify that a token issued by one LDAP instance works on another hosting
   cluster, with consistent issuer/audience/keys and clocks. Validate expiry,
   directory-failure behavior, and resource operations before moving traffic.
4. Move only compatible consumers, with an explicit fresh login. Do not mix
   OpenShift and LDAP replicas behind one undifferentiated endpoint: their
   credentials and login flows are incompatible. Consumers requiring OpenShift
   browser login/refresh stay on that mode until a migration is agreed. Frontend
   readiness must be confirmed outside this repository; CLI work is phase 2.
5. If acceptance fails, return affected traffic to the retained route and
   configuration and have clients log in to that mode again. Preserve previous
   secrets/RBAC for the agreed rollback window; do not add token exchange or
   automatic provider fallback.

See the [LDAP setup guide](../../../../README.md#ldap) for configuration and
key rotation. After upgrading, verify login and namespace access with a
non-admin user on each managed cluster.
