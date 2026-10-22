# Phase 2 — LDAP CLI and automation

**Purpose:** Specify CLI deliverables, contracts, and tests for authentication phase 2 without duplicating the backend authentication design.

**Audience:** CLI maintainers and automation owners.

## Scope and dependencies

- Implements phase 2 of the [authentication HLD](../hld.md), following [phase 1](../phase-1/lld.md). The CLI consumes phase 1's [API and error contracts](../phase-1/lld.md#4-api-and-error-contracts) and [token contract](../phase-1/lld.md#3-access-token-issuance-and-verification); these remain the authoritative backend specifications.
- Phase 2 T01 depends on phase 1's T06 API contract; phase 2 T02 depends on phase 2 T01. Delivery requires the phase 1 backend. Backend implementation does not depend on this phase.
- Preserve existing OpenShift/passthrough CLI behavior. Exclude backend authentication changes, new refresh/session mechanisms, ADFS integration, and frontend implementation.

## Deliverables

### 1. Secure unattended login

- Extend existing CLI login with `ldap` mode and new boolean `--password-stdin`; reuse `--username`, `loginWithPassword`, mode detection, and context persistence. LDAP credential login requires `--username` and `--password-stdin`, with only the password read from `cmd.InOrStdin()`; it never prompts or opens a browser. Limit raw input to 16 KiB, reject overflow, strip at most one terminal LF/CRLF, and preserve all other password bytes. Reject invalid UTF-8 before sending credentials because the existing JSON client would otherwise replace invalid bytes.
- Reject use of `--password-stdin` when the explicit or detected mode is not `ldap`, before reading stdin or entering any interactive/browser flow. Preserve existing OpenShift/passthrough flags and login behavior.
- In LDAP mode, require a certificate-verified HTTPS backend and reject `--insecure`. For credential login, reject missing stdin/username, empty passwords, and use of the `--password` argument, including an explicitly empty value. Reject combining `--token` with password-login flags. Permit `--token` import as an alternative without a password exchange; record its expiry as specified below, while the backend authenticates the token on use.
- For LDAP credential login, configure the existing CLI HTTP client through `http.Client.CheckRedirect` to return an error on every redirect, including same-origin redirects; never replay credentials to a redirected URL or save a context after a redirect failure. Keep this policy isolated to LDAP credential requests so existing OpenShift/passthrough behavior remains unchanged.

### 2. Token persistence and expiry

- For LDAP contexts, store only mode, access token, expiry, and existing context metadata using current 0600 persistence; never store the AD password or refresh credentials. Clear previous token/refresh metadata when replacing a context. Password login records the phase 1 response's `expiresAt` in `Context.TokenExpiresAt`.
- For LDAP `--token` import, use the existing JWT library's parser and claims helpers to read `exp` without verifying the signature, solely as an untrusted local expiry hint. Require a well-formed JWT with a usable positive numeric `exp`; reject malformed tokens and missing, nonnumeric, or nonpositive expiry before saving a context. Reject an already-expired import with `LDAP access token expired; run cappctl login again`. Do not infer validity or authorization from parsed claims: signature, issuer, audience, purpose, and identity verification remain the backend's responsibility. This CLI metadata parsing does not change phase 1's requirement to verify tokens before authentication.
- Apply the known-expiry check before both resource commands and MCP-header commands when the selected token comes from an LDAP context. At or after expiry, return `LDAP access token expired; run cappctl login again` before handler/network execution; unexpired contexts remain usable, including within the OpenShift refresh window of 30 seconds. LDAP contexts never attempt refresh, browser fallback, or hidden password retries; automation explicitly repeats login from its secret store as needed.
- Preserve flag > environment > context resolution. A token supplied through `--token` or `CAPP_TOKEN` must not inherit an active/named context's expiry, even if the server comes from that context; those overrides remain subject to backend verification on use.

### 3. CLI documentation and operations

- Update CLI help, `docs/cappctl.md`, and CLI-related README/contributor guidance for LDAP mode, `--username`, `--password-stdin`, verified HTTPS, redirect rejection, token import, 0600 token persistence, expiry, and explicit re-login. Replace the phase 1 CLI-availability limitation when this implementation ships. Preserve existing-mode guidance.
- Document installing a private backend CA into the trust store used by the CLI runtime so LDAP login can verify HTTPS without `--insecure`. The backend's `auth.ldap.caCert` configures directory trust, not CLI trust; no new CLI CA flag is required by this phase.
- Document unattended LDAP use with dedicated AD accounts and secret-store input, without password arguments or persisted passwords/refresh credentials in LDAP examples. Preserve existing OpenShift password/refresh guidance. Automation owners stop on rejected credentials and rotate stale secrets instead of retrying bad passwords. Refer to phase 1's [operator prerequisites](../phase-1/lld.md#5-deployment-and-documentation) and the HLD's [security requirements](../hld.md#security) for lockout ownership and the absence of ADFS SSO/MFA.

### 4. Tests and acceptance

- **CLI login (T01):** non-TTY stdin login without prompts/browser, HTTPS/verification requirements, exact valid UTF-8 secret-byte handling, raw-input size boundaries, invalid UTF-8 rejection before credential requests, invalid flag/input cases, no saved LDAP password/refresh token, and context replacement. Test `--password-stdin` rejection for both explicit and detected non-LDAP modes without reading stdin or entering interactive/browser flows. Cover rejected redirects, including 307/308 cross-origin and HTTPS-to-HTTP targets and same-origin redirects; assert no redirected request and no context save.
- **Token import (T01):** persist the imported token's numeric `exp`; reject malformed, missing/nonnumeric/nonpositive-expiry, and already-expired inputs without saving a context. Verify expiry extraction is metadata parsing only and does not authenticate the token or perform a password exchange.
- **Expiry (T02):** test the exact expiry boundary, unexpired tokens within 30 seconds of expiry, imported-token expiry, and flag/environment overrides of expired active/named contexts before resource and root-mounted MCP-header handlers. Assert the deterministic LDAP expiry error, no LDAP refresh or hidden reauthentication, and no headers emitted for an expired context token.
- Add OpenShift browser/password/direct-token and passthrough login compatibility tests in T01, and OpenShift refresh/passthrough command compatibility tests in T02; current CLI login/root coverage must not be assumed. Reuse existing CLI helpers and fixtures; outer Go test names follow PascalCase conventions.
- Run targeted CLI tests plus repository `make lint` and `make test` (race detection). Both commands must pass before marking each task complete, including documentation-only tasks; failures or unavailable checks stop progression until corrected and rerun.
- Phase 2 is complete when secure unattended login, token import/storage, expiry handling, MCP-header behavior, and matching CLI documentation are delivered against the phase 1 API. Backend/deployment changes are not required by this phase.

## Reuse checks

- Reuse [CLI password helpers](../../../../../internal/cli/auth/helpers.go), the [CLI HTTP client](../../../../../internal/cli/client/client.go), and [context persistence](../../../../../internal/cli/config/config.go).
- Use [Cobra InOrStdin](https://pkg.go.dev/github.com/spf13/cobra#Command.InOrStdin) and the [standard HTTP redirect policy](https://pkg.go.dev/net/http#Client); do not introduce a new CLI framework. Keep redirect enforcement isolated to LDAP credential requests.
- Use `io.LimitReader` with one extra byte to detect input overflow, `utf8.Valid` to reject invalid UTF-8, and pflag's `Changed` to distinguish omitted flags from explicit empty values. Reuse `github.com/golang-jwt/jwt/v5` parser/claims helpers for imported expiry metadata; do not add custom JWT decoding or CLI token-authentication logic.
