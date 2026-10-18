# Phase 2 — LDAP CLI and automation tasks

**Purpose:** Break CLI delivery into ordered tasks against the phase 1 API; architecture remains in the [HLD](../hld.md), and CLI contracts in the [LLD](lld.md).

**Audience:** CLI implementers, automation owners, and reviewers.

Task IDs are preserved across the authentication domain: T07–T08 move from the original phase 1 task list, and T12 owns the CLI documentation split from T11. Backend/API/operator documentation stays in [phase 1's T11](../phase-1/tasks.md#t11--document-ldap-backend-usage-and-operations). Phase 2 consumes the phase 1 backend and does not block its completion.

**Mandatory completion gate — every task in this phase:** After each task's changes, run `make lint` and `make test` from the repository root and record both results. Both commands must pass before marking the task complete or starting the next task, including after documentation-only tasks. If either command fails or cannot run, stop and report the failure or blocker; after corrections, rerun both commands before proceeding. This gate is part of every task's completion criteria and does not replace its targeted CLI tests.

### T07 — Add secure unattended CLI login

- Concern: Wire LDAP mode and password-stdin into CLI login, including HTTPS validation, redirect rejection, token import, and safe context replacement.
- Files: `internal/cli/auth/login.go`, `internal/cli/auth/login_test.go`, `internal/cli/auth/helpers.go` (only if needed for password exchange), `internal/cli/client/client.go`, `internal/cli/client/client_test.go` (LDAP-specific redirect policy and every affected client/signature call site)
- Docs: [1. Secure unattended login](lld.md#1-secure-unattended-login), [2. Token persistence and expiry](lld.md#2-token-persistence-and-expiry), [4. Tests and acceptance](lld.md#4-tests-and-acceptance)
- Depends on: [Phase 1 T06 — API contracts](../phase-1/tasks.md#t06--expose-ldap-api-contracts)
- Done when: Non-TTY LDAP login handles bounded password bytes and all flag conflicts without prompts/browser; verified HTTPS and no redirects are enforced, failures do not save contexts, successful 0600 persistence contains no password/refresh metadata, and existing modes retain their behavior. CLI help lists all three modes and explains LDAP `--username`/`--password-stdin` usage and the verified-HTTPS requirement.

### T08 — Enforce LDAP CLI token expiry

- Concern: Stop expired LDAP contexts before resource or MCP-header commands without attempting refresh or hidden reauthentication.
- Files: `internal/cli/root/root.go`, `internal/cli/root/root_test.go` (context resolution and `maybeRefresh`); `internal/cli/auth/headers_test.go` (root-mounted MCP-header regression only)
- Docs: [1. Secure unattended login](lld.md#1-secure-unattended-login), [2. Token persistence and expiry](lld.md#2-token-persistence-and-expiry), [4. Tests and acceptance](lld.md#4-tests-and-acceptance)
- Depends on: T07
- Done when: Known-expired LDAP contexts return the exact LLD error before handler/network execution, unexpired contexts work, no LDAP refresh is attempted, and OpenShift refresh/passthrough regressions pass.

### T12 — Document LDAP CLI usage and automation

- Concern: Document the implemented LDAP CLI flow and unattended operation.
- Files: `docs/cappctl.md`, `README.md`, `CLAUDE.md` (CLI/automation content and phase 1 CLI-availability limitations only; command help is owned by T07)
- Docs: [3. CLI documentation and operations](lld.md#3-cli-documentation-and-operations), [4. Tests and acceptance](lld.md#4-tests-and-acceptance)
- Depends on: T07, T08
- Done when: Documentation matches password-stdin, verified HTTPS/no redirects, token import and persistence, expiry/re-login, and MCP-header behavior. No examples pass passwords as arguments or persist passwords/refresh credentials. Existing OpenShift/passthrough guidance is preserved, phase 1's CLI-availability limitation is updated, and make lint/make test pass.

## Phase completion

T07, T08, and T12 deliver secure unattended LDAP CLI login, token import/storage, expiry enforcement before resource and MCP-header commands, and matching documentation against the phase 1 API.
