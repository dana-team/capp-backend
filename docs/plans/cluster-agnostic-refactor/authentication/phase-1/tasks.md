# Phase 1 — LDAP backend and deployment tasks

**Purpose:** Break the backend scope into ordered tasks with files, dependencies, and completion criteria; architecture and rationale remain in the [HLD](../hld.md), and contracts in the [LLD](lld.md).

**Audience:** Backend implementers, deployment maintainers, and reviewers.

Task IDs are preserved across the authentication domain. [T07 — CLI login](../phase-2/tasks.md#t07--add-secure-unattended-cli-login) and [T08 — CLI expiry](../phase-2/tasks.md#t08--enforce-ldap-cli-token-expiry) move to phase 2; neither blocks this phase. T11 covers backend documentation, while phase 2's T12 covers CLI documentation.

**Mandatory completion gate — every task in this phase:** After each task's changes, run `make lint` and `make test` from the repository root and record both results. Both commands must pass before marking the task complete or starting the next task, including after Helm-only or documentation-only tasks. If either command fails or cannot run, stop and report the failure or blocker; after corrections, rerun both commands before proceeding. This gate is part of every task's completion criteria and does not replace its targeted tests or Helm checks.

### T01 — Reuse TLS configuration

- Concern: Extract the shared TLS configuration builder and keep the existing HTTP transport wired through it without changing OpenShift behavior.
- Files: `internal/auth/tls.go`, `internal/auth/tls_test.go` (TLS builders and their tests)
- Docs: [2. LDAP manager and search-and-bind](lld.md#2-ldap-manager-and-search-and-bind)
- Done when: The HTTP wrapper uses the shared builder; system-root/custom-CA behavior and TLS minimums match the LLD, and existing TLS regressions pass.

### T02 — Define LDAP configuration

- Concern: Add the LDAP configuration schema, defaults, environment loading, and mode-gated validation with the specified library dependencies.
- Files: `internal/config/config.go`, `internal/config/validation.go`, `internal/config/config_test.go`, `go.mod`, `go.sum` (LDAP/JWT dependency declarations)
- Docs: [1. Configuration and mode wiring](lld.md#1-configuration-and-mode-wiring)
- Done when: Every LLD configuration field loads and validates, including environment-only secrets, filters/DNs, key consistency, and per-cluster tokens; existing defaults and modes remain unchanged.

### T03 — Implement directory credential verification

- Concern: Implement isolated TLS-protected search-and-bind and canonical group resolution using the LDAP library behind a private three-method operations interface, returning sanitized failures.
- Files: `internal/auth/ldap_directory.go`, `internal/auth/ldap_directory_test.go` (new cohesive directory operations); `internal/auth/manager.go` (`ErrDirectoryUnavailable` declaration only)
- Docs: [2. LDAP manager and search-and-bind](lld.md#2-ldap-manager-and-search-and-bind), [6. Tests and acceptance](lld.md#6-tests-and-acceptance)
- Depends on: T01, T02. The interface, cleanup-contract, and coverage refinements are confined to T03; committed T01 TLS helpers and T02 configuration/dependencies need no changes.
- Done when: The private interface contains only `Bind`, `Search`, and `SearchWithPaging`, and test doubles implement it explicitly. Successful connection setup transfers ownership through a mandatory cleanup callback; failed setup cleans up internally. Tests cover TLS before credentials on both connections, certificate/hostname rejection, failed StartTLS without plaintext fallback, escaped unique lookup, separate target-user bind, direct/paged groups, identity restrictions, empty versus incomplete results, deadline/cancellation cleanup both before StartTLS acceptance and during the subsequent TLS handshake, referrals, and concurrent isolation. Keep all existing transport and failure combinations. Verify caller-context propagation to both connections in a focused unit test; cover concurrent isolation with overlapping real-transport logins with distinct credentials through one directory instance for both LDAPS and StartTLS, asserting cleanup. No password retry or retained credentials.

### T04 — Implement access-token issuance and verification

- Concern: Implement the specified EdDSA token contract and local verification with configured signing/trust keys.
- Files: `internal/auth/ldap_tokens.go`, `internal/auth/ldap_tokens_test.go` (new cohesive token operations)
- Docs: [3. Access-token issuance and verification](lld.md#3-access-token-issuance-and-verification), [6. Tests and acceptance](lld.md#6-tests-and-acceptance)
- Depends on: T02
- Done when: Cross-instance and overlapping-key rotation tests pass; malformed, tampered, expired, wrong-purpose, wrong-trust, and invalid-identity tokens fail with the specified errors; verification performs no directory calls.

### T05 — Wire the LDAP authentication provider

- Concern: Implement the LDAP AuthManager using directory verification and tokens, and select it through the existing factory.
- Files: `internal/auth/ldap.go`, `internal/auth/ldap_test.go` (new provider); `internal/auth/manager.go` (`New` factory and mode/interface documentation only)
- Docs: [2. LDAP manager and search-and-bind](lld.md#2-ldap-manager-and-search-and-bind), [3. Access-token issuance and verification](lld.md#3-access-token-issuance-and-verification)
- Depends on: T03, T04
- Done when: Factory-selected LDAP login issues a TokenPair only after complete authentication; bearer authentication returns impersonation credentials with no forwarded user token, raw-token Login/Refresh are unsupported, and existing factories/cleanup behavior remain unchanged.

### T06 — Expose LDAP API contracts

- Concern: Wire LDAP login, mode reporting, refresh rejection, and sanitized error responses into the existing routes and authentication middleware.
- Files: `internal/server/auth_routes.go`, `internal/server/auth_routes_test.go`, `internal/middleware/auth.go`, `internal/middleware/auth_test.go` (route registration and all refresh-handler call sites included)
- Docs: [4. API and error contracts](lld.md#4-api-and-error-contracts), [6. Tests and acceptance](lld.md#6-tests-and-acceptance)
- Depends on: T05
- Done when: MockAuthManager-based tests cover the LLD status/code/message matrix, no-store success, body-free LDAP refresh rejection, no LDAP OAuth routes, handler aborts and redaction; existing-mode API contracts and rate limiting remain unchanged.

### T09 — Deploy LDAP configuration and secrets

- Concern: Carry LDAP settings and Secret-backed credentials through Helm into both Deployment and Capp workloads.
- Files: `helm/capp-backend/values.yaml` (LDAP configuration/secret values), `helm/capp-backend/templates/configmap.yaml`, `helm/capp-backend/templates/secret.yaml`, `helm/capp-backend/templates/deployment.yaml`, `helm/capp-backend/templates/capp.yaml` (LDAP environment/configuration injection only)
- Docs: [5. Deployment and documentation](lld.md#5-deployment-and-documentation)
- Depends on: T02
- Done when: Both workload variants render all LDAP settings, environment secret references, and public verification-key maps without exposing private material in ConfigMaps; existing names/labels and other-mode resources are preserved.

### T10 — Enable LDAP impersonation permissions

- Concern: Extend service-account and impersonation RBAC gates to LDAP while keeping OAuth annotations and TokenReview permissions OpenShift-only.
- Files: `helm/capp-backend/templates/serviceaccount.yaml`, `helm/capp-backend/templates/clusterrole.yaml`, `helm/capp-backend/templates/clusterrolebinding.yaml`, `helm/capp-backend/templates/capp-user-cr.yaml`, `helm/capp-backend/templates/deployment.yaml` (serviceAccountName only), `helm/capp-backend/values.yaml` (service-account guidance only)
- Docs: [5. Deployment and documentation](lld.md#5-deployment-and-documentation), [6. Tests and acceptance](lld.md#6-tests-and-acceptance)
- Depends on: T09
- Done when: Helm lint/render checks cover all three modes and both workload variants; LDAP receives service-account/impersonation and required Projects access, no OAuth/TokenReview artifacts, and unchanged OpenShift/passthrough resources.

### T11 — Document LDAP backend usage and operations

- Concern: Update API, configuration, and operator documentation for the additive LDAP backend mode.
- Files: `README.md`, `CLAUDE.md`, `api/openapi.yaml`, `config/config.yaml` (backend/API/operator examples and affected existing auth references only)
- Docs: [5. Deployment and documentation](lld.md#5-deployment-and-documentation), [Assumptions and prerequisites](lld.md#assumptions-and-prerequisites)
- Depends on: T06, T10
- Done when: Documentation matches implemented mode/API/config behavior, web HTTPS integration, keys/rotation, RBAC/group coverage, token expiry/re-login, lockout ownership, and absent MFA/SSO/refresh. LDAP CLI support, including token import, is explicitly deferred to phase 2, and frontend implementation remains external. Existing-mode guidance is preserved; final make test/make lint plus Helm checks pass.

## Phase completion

T01–T06 and T09–T11 deliver a deployable LDAP backend with cross-instance token verification, Kubernetes impersonation, and API/operator documentation. No new LDAP CLI implementation or CLI documentation is required for completion.
