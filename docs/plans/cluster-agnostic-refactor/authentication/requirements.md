**Purpose:** Define LDAP authentication outcomes, constraints, and success criteria; architecture remains in the HLD, contracts and tests in the LLDs, and delivery sequencing in the plan.

**Inherited requirements:** [Shared R01](../requirements.md#r01), [shared R02](../requirements.md#r02).

## Goal

Authenticate people and dedicated AD automation accounts securely, with credentials usable across interchangeable backend instances.

## Scope

- In scope: LDAP backend/API authentication, deployment and operator guidance, and CLI login/token handling.
- Non-goals: frontend implementation, OpenShift replacement, ADFS/AD REST integration, LDAP refresh tokens or session storage, long-lived-token management, immediate per-account revocation, authorization redesign, platform cleanup, and GitOps concurrency.

## Requirements

### R01

- Requirement: Authenticate AD credentials securely through LDAP and establish a canonical identity only after successful user authentication and complete group resolution.
- Success criterion: Verified LDAPS and HTTPS protect credentials; failed, ambiguous, or incomplete authentication issues no token. Directory failures are distinguishable from rejected credentials through sanitized responses. Concurrent logins cannot share authenticated bind state, and submitted passwords are neither persisted nor logged.
- Source: [Research — LDAP authentication](../research.md#group-1-authentication--openshift-oauth-is-cluster-bound); security criteria retained through the requirements review on 2026-10-25.

### R02

- Requirement: Issue short-lived access tokens that every consistently configured instance can verify locally.
- Success criterion: A token issued by one instance is accepted by another during its validity, including configured key rotation; invalid, expired, or wrong-purpose/trust tokens are rejected. Verification needs neither a directory call nor sticky sessions, so directory outages do not invalidate otherwise valid tokens.
- Source: [Research — cross-instance token verification](../research.md#group-1-authentication--openshift-oauth-is-cluster-bound); token-lifecycle criteria retained through the requirements review on 2026-10-25.

### R03

- Requirement: Support secure unattended LDAP CLI use throughout login, token import/storage, expiry, and explicit reauthentication.
- Success criterion: Automation can log in without prompts/browser or password arguments over verified HTTPS, without credential redirects. Stored contexts contain only tokens, expiry, and context metadata. Expired context tokens stop resource and MCP-header execution; explicit token overrides retain precedence. Token import reads expiry metadata without claiming authentication, and LDAP never performs refresh or hidden password retries. CLI guidance describes the supported flow.
- Source: [Research — CLI and automation](../research.md#group-5-cli-and-api-surface); CLI security and lifecycle criteria retained through the requirements review on 2026-10-25.

## Constraints and compatibility

- Preserve existing-mode defaults, API/CLI flows, and deployment support under [shared R02](../requirements.md#r02). Keep both Deployment and Capp workload configuration and API/operator guidance usable. Upgrades must explicitly retain required Projects permissions and replace earlier StartTLS configuration with verified LDAPS.
- LDAP authentication does not itself grant application permissions. Legacy Kubernetes authorization still needs configured cluster credentials and matching grants; the new mode follows [authorization requirements](../authorization/requirements.md). See [research authorization boundary](../research.md#group-2-authorization--k8s-rbac-assumes-cluster-local-identities).
- Account/group/password changes may not affect issued tokens until expiry; logout discards the client token. LDAP does not inherit ADFS SSO/MFA. Rejected passwords are not automatically retried, and per-instance/IP rate limits do not promise fleet-wide account-lockout protection.

## Assumptions

- Operators provide direct LDAPS access, CA trust, search credentials/filters, canonical attributes, required group coverage, signing keys, and synchronized configuration/clocks. Default direct-group lookup does not guarantee nested or primary groups.
- Automation accounts can obtain their own AD credentials from a secret store. Directory/platform operators own lockout policy and any distributed abuse controls; no new distributed limiter is required here.
