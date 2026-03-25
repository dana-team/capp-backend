// Package ui embeds the Scalar API reference UI and the OpenAPI spec into
// the binary so that the docs endpoint works in disconnected (air-gapped)
// environments without any CDN access at runtime.
//
// To update the Scalar bundle run:
//
//	make fetch-ui
//
// The downloaded file (scalar.min.js) should be committed to the repository
// so that production builds never require internet access.
package ui

import _ "embed"

// ScalarJS is the self-contained Scalar API reference JavaScript bundle.
// It is baked into the binary at compile time via go:embed.
//
//go:embed scalar.min.js
var ScalarJS []byte

// DocsHTML is the minimal HTML shell that loads Scalar from the embedded JS
// bundle and points it at the embedded OpenAPI spec — both served locally.
var DocsHTML = []byte(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>capp-backend API Reference</title>
    <style>
      body { margin: 0; padding: 0; }
    </style>
  </head>
  <body>
    <!-- Scalar reads the data-url attribute and renders the spec. -->
    <script
      id="api-reference"
      data-url="/openapi.yaml"
      data-configuration='{"theme":"purple"}'></script>
    <!-- Served from the embedded binary — no CDN required. -->
    <script src="/ui/scalar.js"></script>
  </body>
</html>
`)
