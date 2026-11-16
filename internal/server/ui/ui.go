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
    <!-- Scalar reads data-url / data-configuration from this element on load. -->
    <script id="api-reference"></script>
    <!--
      The docs are served at <server-url>/docs, where <server-url> may carry a
      path prefix (e.g. behind an ingress). Derive <server-url> from the page
      URL at runtime and use it for the spec, the bundle, and "Send request",
      instead of the spec's localhost server.
    -->
    <script>
      (function () {
        var base = window.location.origin +
          window.location.pathname.replace(/\/docs\/?$/, "").replace(/\/$/, "");
        var ref = document.getElementById("api-reference");
        ref.dataset.url = base + "/openapi.yaml";
        ref.dataset.configuration = JSON.stringify({
          theme: "purple",
          servers: [{ url: base, description: "This server" }],
        });
        // Load the bundle only after the config is set; it is served from the
        // embedded binary, so no CDN is required.
        var bundle = document.createElement("script");
        bundle.src = base + "/ui/scalar.js";
        document.body.appendChild(bundle);
      })();
    </script>
  </body>
</html>
`)
