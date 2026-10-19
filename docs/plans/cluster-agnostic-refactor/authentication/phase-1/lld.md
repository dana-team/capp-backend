# Phase 1 — LDAP backend and deployment

**Purpose:** Specify implementable deliverables, contracts, and tests within the stated scope; revisiting the architecture or implementing source code is outside this document's scope.

**Audience:** Backend maintainers, API consumers, and directory/platform integrators.

## Scope

- Implements authentication phase 1 of the [HLD](../hld.md): deliver LDAP authentication through the HTTPS API, deployment configuration, and impersonation permissions.
- LDAP CLI implementation, token import, CLI expiry behavior, and CLI documentation belong to [phase 2](../phase-2/lld.md). Phase 1 has no dependency on their completion. Frontend implementation remains outside this repository.
- Preserve all existing OpenShift/passthrough configuration, routes, CLI flows, cache/cleanup, default selection, and deployment resources. Exclude LDAP-mode refresh/session storage, ADFS SSO/MFA integration, authorization redesign, and other roadmap phases.

## Goals

- Authenticate through LDAP search-and-bind, issue backend access tokens, and verify them on any instance without a directory call per resource request.
- Reuse existing auth/API contracts; never authenticate a target user merely because the search account found their entry.

## Deliverables

### 1. Configuration and mode wiring

- Extend `AuthConfig`, `setDefaults`, `Load`, and `Validate` in [internal/config](../../../../../internal/config/config.go) with `AuthModeLDAP = "ldap"`, `LDAPConfig`, and `AuthConfig.LDAP`. Preserve existing modes/defaults and validate LDAP settings only when selected. Retain existing per-cluster inline-token requirements for OpenShift and extend them to LDAP impersonation.
- Define the following keys under `auth.ldap`; fields follow existing PascalCase/mapstructure conventions:

| Keys | Type / default | Contract |
| --- | --- | --- |
| `url`, `caCert` | string required; string empty | Host/port URL using `ldaps://` only; default port 636. Reject `ldap://` URLs; no URL credentials/query/fragment/path. Base64 PEM CA bundle or system roots. |
| `bindDN`, `bindPassword` | strings required | Search-account credentials; password from Secret/environment only. |
| `userBaseDN`, `userFilter` | strings required | Subtree search base and filter containing exactly one `{username}` placeholder. |
| `usernameAttribute` | string, `sAMAccountName` | Canonical username attribute, not the unverified login text. |
| `groupBaseDN`, `groupFilter` | base required; filter default `(&(objectClass=group)(member={userDN}))` | Group filter contains exactly one `{userDN}` placeholder; default is direct membership only. |
| `groupNameAttribute` | string, `sAMAccountName` | Canonical group attribute matching target RoleBindings. |
| `timeoutSeconds`, `tokenTTLSeconds` | ints, `10`, `900` | Positive total login deadline and access-token lifetime. |
| `issuer`, `audience` | strings required | Shared token trust identifiers; not inferred from the receiving instance. |
| `signingKeyID`, `signingPrivateKey` | strings required | Active key ID and PEM Ed25519 private key. |
| `verificationKeys` | map of key ID to PEM public key, empty | Additional trusted Ed25519 public keys for rotation; active public key is derived from the private key. |

- Validate DNs/filter syntax with LDAP library utilities, placeholder counts, attribute names, nonempty credentials, key formats/IDs, and active-key consistency at startup. Reject conflicting key IDs; restrict IDs to lowercase ASCII letters/digits, `-`, and `_` for predictable configuration mapping.
- Bind/register scalar keys for Viper environment-only loading; test `CAPP_AUTH_LDAP_BINDPASSWORD` and `CAPP_AUTH_LDAP_SIGNINGPRIVATEKEY`. Supply public-key maps through mounted configuration; do not assume Viper parses an arbitrary JSON map from an environment variable.

### 2. LDAP manager and search-and-bind

- Add private `ldapManager` in `internal/auth/ldap.go`, implementing existing [AuthManager](../../../../../internal/auth/manager.go); add its factory case without changing existing cases or public method signatures. Add `github.com/go-ldap/ldap/v3`; reuse its `Bind`, `Search`, `SearchWithPaging`, `EscapeFilter`, request/result types, and result-code constants. Define a private interface in `auth` containing only `Bind`, `Search`, and `SearchWithPaging`; `*ldap.Conn` implements it directly. Keep connection setup and cleanup outside this operations interface; do not introduce another LDAP protocol implementation.
- Extract private `buildTLSConfig(caCertB64 string) (*tls.Config, error)` from existing [buildTLSTransport](../../../../../internal/auth/tls.go) for both LDAP and the current HTTP wrapper, preserving OpenShift TLS behavior. Establish TLS immediately and verify certificates/hostnames before either connection sends credentials; no StartTLS upgrade or plaintext fallback. The directory constructor accepts only `ldaps://` endpoints and uses `ldap.DefaultLdapsPort` when no port is specified. Use standard dial/TLS deadlines and context cancellation to close connections, since synchronous LDAP binds do not take a context.
- `PasswordLogin`: reject empty username/password before network calls, preserving password bytes. Under one configured deadline, establish TLS, bind the search connection with configured credentials, and subtree-search for the escaped username. Require exactly one result, returning generic bad credentials for no/ambiguous matches. Never follow referrals with credentials.
- On a second, independently TLS-protected connection, bind using the returned user DN and submitted password; close it after verification. Never rebind a shared search connection as a user or reuse another login's bind state. Do not retry rejected credentials.
- After successful user bind, resolve groups on the search-account connection with the escaped verified DN and paged search (page size 500). Use only requested canonical attributes; reject missing/multivalued username, malformed group identities, partial results, referrals, or search errors. Deduplicate groups; preserve canonical case. Reject whitespace-only identities, surrounding whitespace, and externally supplied `system:` names.
- A successful group search with no matches yields an empty explicit group array; it does not erase user-based or implicit `system:authenticated` RBAC grants. Group filters determine membership coverage; do not invent nested/primary groups or implement client-side traversal.
- Close both connections on every path. A successful connector call returns an encrypted client and a cleanup callback owned by the caller, which must defer that callback immediately. Cleanup stops the cancellation callback and closes the underlying socket and LDAP client; calling the library client's `Close` alone is not the full lifecycle contract. Failed connection setup cleans up its own resources and transfers no client ownership. Retain no user password, mutable LDAP session, or authenticated-user cache in the manager. Return sanitized dependency categories rather than raw LDAP diagnostics containing account details.

### 3. Access-token issuance and verification

- Reuse `github.com/golang-jwt/jwt/v5` (v5.3.1 checksums already in [go.sum](../../../../../go.sum)); declare a direct dependency. Use library `MapClaims`, `NewWithClaims`, `SignedString`, PEM key parsers, and `ParseWithClaims` rather than custom JWT/crypto code.
- Issue only after complete authentication/group resolution: header `alg=EdDSA`, `typ=JWT`, required `kid`; claims `iss`, `aud`, `sub` (canonical RBAC username), `groups` (string array, including empty), `iat`, `exp`, and `token_use="access"`. Default lifetime is 15 minutes. Return existing `TokenPair` with `AccessToken`, matching `ExpiresAt`, and `RefreshToken=""`.
- `Authenticate`: read the Authorization bearer token and use `ParseWithClaims` with `WithValidMethods`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, and `WithIssuedAt`. Resolve only locally configured `kid` keys and pin EdDSA; additionally require numeric `iat`/`exp`, access-token purpose, nonblank subject, and valid groups. Enforce expiry, future-issued-at/not-before checks with zero leeway initially, and issuance identity restrictions. Never use `ParseUnverified`, token-provided remote keys, AD REST validation, or a live LDAP lookup here.
- Populate existing `ClusterCredential.ImpersonateUser/Groups`; keep `BearerToken` empty so only configured cluster credentials reach Kubernetes. Reuse [ClusterManager.ClientFor](../../../../../internal/cluster/manager.go).
- All instances receive consistent signing/trust configuration. Rotation: distribute the new public key first, activate its private key/ID, and keep retiring verification keys until their last issued tokens expire. Key removal is fleet-wide token invalidation after rollout, not immediate per-user logout.

### 4. API and error contracts

- `GET /api/v1/auth/mode` reports `ldap` when selected. Extend existing `loginHandler` to call `PasswordLogin` for LDAP, preserving OpenShift/passthrough dispatch. LDAP request: `{"username":"...","password":"..."}`; raw `cluster`/`token` fields do not authenticate a user. Success uses the existing 200 `TokenPair` response and `Cache-Control: no-store`.
- In LDAP mode, `Refresh` and raw-token `Login` methods return existing `ErrNotSupported`; `POST /api/v1/auth/refresh` returns existing 501 `NOT_SUPPORTED` for `Refresh` without requiring a body. Existing OpenShift login/refresh/OAuth routes and cleanup remain unchanged. LDAP mode registers no OAuth callback or browser-redirection endpoints.
- Add only `auth.ErrDirectoryUnavailable` for sanitized directory/configuration failures; reuse current sentinels and `apierrors` envelope. Extend login/auth middleware handling for this sentinel without changing existing-mode responses:

| Condition | Result | HTTP / existing code / message |
| --- | --- | --- |
| Malformed login JSON or missing/empty fields | Existing bad-request response | 400 / `BAD_REQUEST` / `username and password are required` for missing fields |
| Unknown/ambiguous user; target bind rejects credentials | `ErrBadCredentials` | 401 / `UNAUTHORIZED` / `invalid credentials` |
| TLS/network/deadline failure, search-account failure, incomplete directory result | `ErrDirectoryUnavailable` | 503 / `INTERNAL_ERROR` / `directory service unavailable` |
| Missing/invalid bearer, signature/key/claim failure | `ErrUnauthenticated` | 401 / `UNAUTHORIZED` / `request is not authenticated` |
| Expired bearer | `ErrTokenExpired` | 401 / `UNAUTHORIZED` / `token has expired` |
| Token issuance/internal failure | Existing internal-error handling | 500 / `INTERNAL_ERROR` / `internal server error` |
| Resource denied by Kubernetes | Existing authorization error | Existing 403 behavior |

- Reuse `NewBadRequest`, `NewUnauthorized`, `NewNotSupported`, `APIError`, and `Respond`; do not introduce another error envelope. Reuse configured login rate limiting, which is optional, per client IP and backend instance, not per AD account or fleet-wide lockout protection. Do not log bodies, passwords, tokens, private keys, or raw authentication diagnostics.

- Web integration requirement: submit username/password over HTTPS to the same login endpoint, retain the returned token securely, send it in the Authorization header, and return to login on expiry. Frontend source implementation remains outside this backend repository.

### 5. Deployment and documentation

- Extend existing Helm ConfigMap/Secret/Deployment/Capp wiring for LDAP settings. Inject the search password and signing private key from Secrets, preserving existing naming/labels/ownership; ordinary directory settings and public verification keys are configuration. Ship no real credentials or generated keys in sample files.
- Enable service-account use and impersonator Role/Binding for both OpenShift and LDAP. Keep TokenReview permissions, OAuth secrets/annotations, and existing refresh/cache wiring OpenShift-only. Preserve existing Projects access and extend it to LDAP where unchanged namespace listing requires it; platform redesign remains deferred.
- Update OpenAPI, README, sample configuration, and contributor guidance for the third backend mode, HTTPS API usage, token expiry/rotation, group/RoleBinding prerequisites, and lack of LDAP-mode refresh or ADFS SSO/MFA. State that LDAP CLI support, including token import, arrives in phase 2; preserve existing-mode documentation. CLI help and unattended CLI examples belong to phase 2.
- Document the LDAPS-only directory transport in operator/contributor guidance and configuration examples. Require verified CA trust for directory connections and HTTPS for client login.
- Document operational prerequisites: directory operators own AD account-lockout policy/recovery; platform operators configure any gateway-level distributed login-abuse controls. Automation owners stop jobs on rejected credentials and rotate stale secrets rather than retrying bad passwords; per-IP limits do not prevent account lockout across clients/replicas. No new distributed limiter is part of this scope.

### 6. Tests and acceptance

- **Directory/config:** mode-gated validation and environment precedence; reject `ldap://` URLs in configuration validation and the directory constructor; accept valid LDAPS URLs with default/custom ports and IPv6 hosts. Cover escaped search values; unique lookup; correct DN/password bind; no user bind after failed lookup; no group search/token issuance after failed bind; incomplete/paged group results fail closed; empty group results remain distinct from failure. Use the private three-method operations interface as the test seam, with independent clients per login. Test doubles implement those methods explicitly without embedding an uninitialized library interface.
- **Transport/concurrency:** TLS before credentials on both connections, certificate/hostname rejection without plaintext fallback, timeout/cancellation cleanup, and concurrent logins with no credential/bind-state crossover. Exercise both explicit cancellation and deadline expiry during the LDAPS handshake, search bind, user lookup, user bind, and group lookup. Verify caller-context propagation to both connections in a focused unit test. Cover concurrent isolation with overlapping real-transport LDAPS logins through the same directory instance, with distinct user credentials and connection cleanup assertions. Preserve the existing LDAPS transport and failure combinations. Keep existing OpenShift TLS tests.
- **Tokens/API:** cross-instance issuance/verification and overlapping-key rotation; tampering, unknown key, wrong algorithm/issuer/audience/purpose, missing/expired/time-invalid claims, reserved identities, and no LDAP calls during token authentication. Reuse existing `MockAuthManager` for route/status/envelope tests; assert no handler execution after auth failure and sanitized logs.
- **Deployment:** render both Deployment/Capp variants for all three modes; verify LDAP Secrets and impersonation-only RBAC, unchanged OpenShift OAuth/TokenReview artifacts, and unchanged passthrough/default behavior. Implementation acceptance uses targeted tests, repository `make test` (race detection), `make lint`, and Helm lint/template checks; outer Go test names follow PascalCase conventions.

Phase 1 is complete when the deployed backend authenticates LDAP credentials, issues tokens verifiable across consistently configured instances, and supports Kubernetes impersonation with the documented RBAC prerequisites. API/operator documentation and backend/deployment checks must pass; CLI implementation is not a completion condition.

## Assumptions and prerequisites

- Confirmed: direct LDAPS access, a configured search account, and a second bind using the target account's supplied credentials are supported. Required environments support LDAPS; StartTLS is not required. Assumption: dedicated automation accounts can obtain their own AD credentials from a secret store; an HTTP API key alone is not an LDAP credential.
- Operators provide CA trust, search bases/filters, unambiguous canonical names, appropriate group coverage, matching Kubernetes RoleBindings, signing keys, and synchronized replica configuration/clocks. Default group filtering covers direct membership, not guaranteed primary or nested membership; configure server-supported filters to match required policy.
- AD group/account/password changes affect newly issued tokens; existing tokens remain usable until expiry unless their signing key is withdrawn. Passwords are handled transiently by the backend, and this mode does not inherit ADFS MFA/SSO. These are deliberate first-version limits.

## Reuse checks

- Reuse [AuthManager/TokenPair/ClusterCredential](../../../../../internal/auth/manager.go), the [TLS helper](../../../../../internal/auth/tls.go), [API errors](../../../../../internal/apierrors/errors.go), configuration loading/validation, and [MockAuthManager](../../../../../internal/testutil/helpers.go). `LDAPConfig`, `AuthModeLDAP`, `ldapManager`, `buildTLSConfig`, and `ErrDirectoryUnavailable` extend these existing boundaries.
- Use [go-ldap connection/bind/search/filter utilities](https://pkg.go.dev/github.com/go-ldap/ldap/v3), [JWT signing/verification/PEM parsers](https://pkg.go.dev/github.com/golang-jwt/jwt/v5@v5.3.1), and [standard context cancellation](https://pkg.go.dev/context#AfterFunc). Do not introduce another LDAP/JWT framework; the extracted TLS builder has both HTTP and LDAP callers.
