// Package api provides the embedded OpenAPI specification.
package api

import _ "embed"

// Spec is the OpenAPI 3.1 YAML specification baked into the binary at
// compile time. It is served at /openapi.yaml so that the Scalar UI and
// any other tooling can consume it without network access.
//
//go:embed openapi.yaml
var Spec []byte
