package ui

import (
	"strings"
	"testing"
)

// TestDocsHTMLUsesRuntimeServerURL guards against the docs UI sending requests
// to the spec's localhost server when deployed: the server URL must be derived
// from the page URL (minus /docs), and the bundle loaded only after the config
// is set.
func TestDocsHTMLUsesRuntimeServerURL(t *testing.T) {
	html := string(DocsHTML)

	for _, want := range []string{
		"window.location.origin",
		"window.location.pathname",
		`servers: [{ url: base`,
		`base + "/openapi.yaml"`,
		`base + "/ui/scalar.js"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("docs HTML missing %q", want)
		}
	}
	if strings.Contains(html, `src="/ui/scalar.js"`) {
		t.Error("scalar.js must not load statically before the config is set")
	}
	if strings.Index(html, "dataset.configuration") > strings.Index(html, "appendChild(bundle)") {
		t.Error("configuration must be set before scalar.js is appended")
	}
}
