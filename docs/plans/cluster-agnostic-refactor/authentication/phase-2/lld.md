# Phase 2 — LDAP CLI and automation

**Purpose:** Specify CLI deliverables, contracts, and tests for authentication phase 2 without duplicating the backend authentication design.

**Audience:** CLI maintainers and automation owners.

## Scope and dependencies

- Implements phase 2 of the [authentication HLD](../hld.md), following [phase 1](../phase-1/lld.md). The CLI consumes phase 1's [API and error contracts](../phase-1/lld.md#4-api-and-error-contracts) and [token contract](../phase-1/lld.md#3-access-token-issuance-and-verification); these remain the authoritative backend specifications.
- Phase 2 T01 depends on phase 1's T06 API contract; phase 2 T02 depends on phase 2 T01. Delivery requires the phase 1 backend. Backend implementation does not depend on this phase.
- Preserve existing OpenShift/passthrough CLI behavior. Exclude backend authentication changes, new refresh/session mechanisms, ADFS integration, and frontend implementation.

## Deliverables

### 1. Secure unattended login

- Extend existing CLI login with `ldap` mode and new boolean `--password-stdin`; reuse `--username`, `loginWithPassword`, mode detection, and context persistence. LDAP unattended login requires a username and password from `cmd.InOrStdin()`; it never prompts or opens a browser. Read bounded input (16 KiB maximum), strip at most one terminal LF/CRLF, and preserve all other password bytes.
- In LDAP mode, require a certificate-verified HTTPS backend; reject `--insecure`, missing stdin/username, empty passwords, password-in-argument usage, and combining `--token` with password-login flags. Preserve existing flag behavior for other modes. Permit existing `--token` import as an alternative; the backend validates it on use.
- For LDAP credential login, configure the existing CLI HTTP client through `http.Client.CheckRedirect` to return an error on every redirect, including same-origin redirects; never replay credentials to a redirected URL or save a context after a redirect failure. Keep this policy isolated to LDAP credential requests so existing OpenShift/passthrough behavior remains unchanged.

### 2. Token persistence and expiry

- Store only mode, access token, expiry, and existing context metadata using current 0600 persistence; never store the AD password or refresh credentials. Clear previous token/refresh metadata when replacing a context. For LDAP contexts, known expiry produces `LDAP access token expired; run cappctl login again`; no automatic refresh, browser fallback, or hidden password retry. Automation explicitly repeats login from its secret store as needed.
- Apply the expiry check before both resource commands and MCP-header commands. Unexpired contexts remain usable; LDAP contexts never enter the OpenShift refresh flow.

### 3. CLI documentation and operations

- Update CLI help, `docs/cappctl.md`, and CLI-related README/contributor guidance for LDAP mode, `--username`, `--password-stdin`, verified HTTPS, redirect rejection, token import, 0600 token persistence, expiry, and explicit re-login. Replace the phase 1 CLI-availability limitation when this implementation ships. Preserve existing-mode guidance.
- Document unattended use with dedicated AD accounts and secret-store input, without password arguments or persisted passwords/refresh credentials. Automation owners stop on rejected credentials and rotate stale secrets instead of retrying bad passwords. Refer to phase 1's [operator prerequisites](../phase-1/lld.md#5-deployment-and-documentation) and the HLD's [security requirements](../hld.md#security) for lockout ownership and the absence of ADFS SSO/MFA.

### 4. Tests and acceptance

- **CLI:** non-TTY stdin login without prompts/browser, HTTPS/verification requirements, exact secret-byte handling, invalid flag/input cases, no saved password/refresh token, context replacement, and deterministic expired-token error. Cover rejected redirects, including 307/308 cross-origin and HTTPS-to-HTTP targets and same-origin redirects; assert no redirected request and no context save. Retain OpenShift browser/password/direct-token/refresh and passthrough regressions.
- Verify root-mounted MCP-header expiry handling, no LDAP refresh or hidden reauthentication, and existing OpenShift/passthrough command regressions. Reuse existing CLI helpers and fixtures; outer Go test names follow PascalCase conventions.
- Run targeted CLI tests plus repository `make lint` and `make test` (race detection). Both commands must pass before marking each task complete, including documentation-only tasks; failures or unavailable checks stop progression until corrected and rerun.
- Phase 2 is complete when secure unattended login, token import/storage, expiry handling, MCP-header behavior, and matching CLI documentation are delivered against the phase 1 API. Backend/deployment changes are not required by this phase.

## Reuse checks

- Reuse [CLI password helpers](../../../../../internal/cli/auth/helpers.go), the [CLI HTTP client](../../../../../internal/cli/client/client.go), and [context persistence](../../../../../internal/cli/config/config.go).
- Use [Cobra InOrStdin](https://pkg.go.dev/github.com/spf13/cobra#Command.InOrStdin) and the [standard HTTP redirect policy](https://pkg.go.dev/net/http#Client); do not introduce a new CLI framework. Keep redirect enforcement isolated to LDAP credential requests.
