# Authentication — LDAP architecture

**Purpose:** Describe the architecture, components, flows, and design rationale; detailed API contracts, implementation steps, and per-phase tests are outside this document's scope.

**Audience:** Backend/frontend maintainers, automation owners, and directory/platform operators.

## Summary

- This authentication-domain design covers the LDAP work in the [roadmap](../plan.md), based on the [research](../research.md). Capp-backend authenticates AD credentials through direct LDAP search-and-bind and issues short-lived access tokens. Authorization redesign, platform decoupling, and GitOps will have separate domain designs.
- Existing `openshift` and `passthrough` modes remain supported.

## Delivery phases

Phase numbers are local to the authentication domain. Both phases use the same LDAP and token contracts.

| Phase | Delivery | Specification | Tasks |
| --- | --- | --- | --- |
| 1 — LDAP backend and deployment | Authentication, tokens, API contracts, Helm configuration, impersonation permissions, and API/operator documentation | [LLD](phase-1/lld.md) | [Tasks](phase-1/tasks.md) |
| 2 — LDAP CLI and automation | Secure unattended login, token import/storage, expiry handling, and CLI documentation | [LLD](phase-2/lld.md) | [Tasks](phase-2/tasks.md) |

Phase 1 is independently usable through the HTTPS API. LDAP support in `cappctl`, including token import, arrives in phase 2. Frontend implementation is outside this repository; phase 1 supplies the web integration API, not a completed web login UI. Phase 2 consumes phase 1's API without changing its authentication contract. Future ADFS integration remains separate roadmap work.

## Goals / Non-goals

**Cluster independence means capp-backend is portable and its behavior does not depend on which cluster receives the request. It does not require every dependency to survive a cluster failure.**

capp-backend can be deployed on any supported OCP cluster, with consistent request handling across instances that have equivalent configuration and access to the same dependencies. Instances must accept the same tokens and apply the same authorization rules. Global DNS and routing configuration are infrastructure responsibilities outside this work's scope. Cross-cluster failover is optional, not an acceptance requirement for this phase; shared authentication and other services are allowed.

- **Goals:** one login path for people and dedicated AD automation accounts, interchangeable backend instances, and existing Kubernetes impersonation/RBAC.
- **Non-goals:** OpenShift replacement, AD REST/ADFS broker integration in the new mode, LDAP-mode refresh tokens, session storage, immediate per-account revocation, authorization redesign, platform cleanup, and GitOps concurrency. Existing OpenShift login/refresh remain unchanged.
- **Open questions:** broader authorization, automation-credential alternatives, and ADFS integration remain in [research](../research.md#open-questions); they do not change this design's scope.

## Architecture

- **Mode selection:** add `ldap` alongside existing modes, selected at startup without automatic provider fallback. The following describes LDAP mode.
- **LDAP adapter:** reuses `AuthManager.PasswordLogin`. A configured search account finds the target user's entry; a separate TLS-protected connection binds as that user using the submitted password. Only after success does the backend resolve groups and derive the canonical RBAC identity. Each login owns isolated connections and their cleanup; a successful search never proves the user's identity. Verification depends on a private operations interface owned by the auth package, while the LDAP library implements the protocol and a connector owns transport setup.
- **Token issuer/verifier:** backend-signed tokens carry the canonical username/groups. Replicas share issuer/audience, signing/trust configuration, and token policy, allowing local verification without LDAP calls or sticky sessions. Private keys remain deployment secrets; rotation retains retiring public keys until their tokens expire.
- **Clients/authorization:** phase 1 exposes HTTPS login for API and web consumers; phase 2 adds the CLI integration. Automation submits its own AD credentials non-interactively from a secret store; the CLI persists only token/expiry and context metadata. Verified tokens populate existing `ClusterCredential` impersonation fields; per-cluster credentials, impersonation permissions, and matching RoleBindings remain necessary. Empty external groups do not remove user-based or implicit Kubernetes authenticated-group permissions.
- **Assumptions:** direct LDAP access and search-and-bind are confirmed. Operators supply CA trust, search bases/filters, canonical attributes, required group coverage, automation account credentials, and synchronized replica configuration/clocks.

## Workflow / Sequence

- **Login:** client posts credentials; backend searches, performs a separate target-user bind, resolves groups, and returns a signed token plus expiry. Failed or partial authentication issues no token.
- **Resource access:** any instance verifies signature, token purpose, issuer, audience, expiry, and identity before existing Kubernetes authorization; no directory lookup is needed.
- **Expiry:** clients log in again; automation repeats login using its secret store. No user password or LDAP refresh token is persisted.

## Security

- Require verified LDAPS before binds, and HTTPS for client login. Escape search values, reject empty passwords, isolate bind state, and never log credentials/tokens or persist submitted passwords.
- Use established LDAP/JWT libraries, pinned signing algorithms/configured keys, and Secret-backed credentials. Existing optional rate limits apply per client IP and backend instance, not per AD account; they do not provide fleet-wide account-lockout protection. Directory operators own AD lockout policy, and platform operators configure any gateway-level distributed login-abuse controls. Do not automatically retry bad passwords.
- LDAP password login does not automatically inherit ADFS SSO/MFA. Automation acts as its own AD account; directory search privileges confer no capp permissions. App tokens do not constitute a new OAuth authorization-server protocol.

## Failure modes

- **Directory/configuration failure:** new logins fail closed with sanitized dependency errors; existing unexpired tokens remain usable through local verification. Failed group resolution must not become an empty identity.
- **Credentials:** reject unsuccessful/ambiguous logins with generic messages and no partial token issuance.
- **Account lockout:** stale automation secrets or repeated attempts across clients/replicas can lock AD accounts despite per-IP limits. Automation owners stop failing jobs and rotate stale secrets; directory/platform operators handle lockout recovery and review directory/gateway policy.
- **Stale identity/revocation:** logout discards the client token; password/account/group changes may not affect issued tokens until expiry. No per-token revocation store exists; withdrawing a signing key invalidates its tokens after fleet configuration updates.
- **Fleet inconsistency:** mismatched trust/configuration/clocks can reject valid tokens; report sanitized directory, token, and configuration failures.

## Resolved Questions

1. **StartTLS requirement** — StartTLS is not required; the required environments
   support LDAPS. This integration uses LDAPS.
