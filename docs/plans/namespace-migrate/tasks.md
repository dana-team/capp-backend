## Tasks — Namespace-level batch migration API (Phase 1)

### T01 — Extract `resolveTarget`
- Concern: Extract shared pre-flight validation (cluster meta, credential, same-cluster check, target cluster resolution, target namespace verification) from `migrate` into `resolveTarget` method + `migrateTarget` struct; refactor `migrate` to call it
- Files: `internal/resources/namespaced/capps/migrate.go`, `internal/resources/namespaced/capps/handler.go`
- Docs: [Extract shared migration helpers](lld-phase-1.md#0-extract-shared-migration-helpers-in-handlergo--migratego)
- Done when: `migrate` handler calls `resolveTarget`; existing `TestMigrate` passes unchanged

### T02 — Extract `deleteMigratedSource`
- Concern: Extract delete-source + remove-bypass-annotation block from `migrate` into `deleteMigratedSource` function; refactor `migrate` to call it
- Files: `internal/resources/namespaced/capps/migrate.go`, `internal/resources/namespaced/capps/handler.go`
- Docs: [Extract shared migration helpers](lld-phase-1.md#0-extract-shared-migration-helpers-in-handlergo--migratego)
- Done when: `migrate` handler calls `deleteMigratedSource`; existing `TestMigrate` passes unchanged

### T03 — Add request/response types
- Concern: Add `NamespaceMigrateRequest`, `CappMigrateResult`, `NamespaceMigrateResponse` structs to the migration types section
- Files: `internal/resources/namespaced/capps/types.go`
- Docs: [Request/response types](lld-phase-1.md#1-requestresponse-types-in-typesgo)
- Done when: types compile; JSON tags match HLD field names

### T04 — Add `validateHostnameMap`
- Concern: Add pre-flight hostname map validation function and unit tests
- Files: `internal/resources/namespaced/capps/migrate.go`, `internal/resources/namespaced/capps/migrate_test.go`
- Docs: [Pre-flight validation helper](lld-phase-1.md#2-pre-flight-validation-helper-in-migratego)
- Done when: `TestValidateHostnameMap` passes with all 7 cases

### T05 — Add `migrateNamespace` handler + route
- Concern: Implement the batch migration handler method and register the `/migrate` route on the `ns` group
- Files: `internal/resources/namespaced/capps/handler.go`
- Docs: [Handler method](lld-phase-1.md#3-handler-method-in-handlergo), [Route registration](lld-phase-1.md#4-route-registration-in-handlergo)
- Depends on: T01, T02, T03, T04
- Done when: route registered; handler compiles; `make build` succeeds

### T06 — Add `TestMigrateNamespace`
- Concern: Handler tests for the batch migration endpoint covering pre-flight and per-Capp loop cases
- Files: `internal/resources/namespaced/capps/handler_test.go`
- Docs: [Tests](lld-phase-1.md#tests)
- Depends on: T05
- Done when: all 17 test cases pass; `make test` green

### T07 — Update OpenAPI spec
- Concern: Add the new path, request/response schemas, and error responses
- Files: `api/openapi.yaml`
- Docs: [OpenAPI spec update](lld-phase-1.md#5-openapi-spec-update-in-apiopenapiYaml)
- Depends on: T03
- Done when: spec valid; new path, schemas, and error responses present

### T08 — Update docs
- Concern: Add new route to `CLAUDE.md` API Routes section; mark item 1 done in `docs/migration-next-steps.md`
- Files: `CLAUDE.md`, `docs/migration-next-steps.md`
- Docs: [Documentation updates](lld-phase-1.md#6-documentation-updates)
- Depends on: T05
- Done when: both files updated with the new endpoint reference
