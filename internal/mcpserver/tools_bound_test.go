package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/boundcapps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundTools(t *testing.T) {
	tests := []struct {
		tool string
		set  ToolSet
		path string
	}{
		{"secret_bound", SecretToolSet{}, "/api/v1/clusters/c1/namespaces/ns1/secrets/res1/bound"},
		{"configmap_bound", ConfigMapToolSet{}, "/api/v1/clusters/c1/namespaces/ns1/configmaps/res1/bound"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			want := boundcapps.Response{BoundCapps: []string{"web", "worker"}, ToReload: []string{"web"}}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, tt.path, r.URL.Path)
				assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(want)
			}))
			defer backend.Close()

			result, err := connectTools(t, backend.URL, tt.set).CallTool(context.Background(), &mcp.CallToolParams{
				Name:      tt.tool,
				Arguments: map[string]any{"cluster": "c1", "namespace": "ns1", "name": "res1"},
			})
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)

			raw, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			var got boundcapps.Response
			require.NoError(t, json.Unmarshal(raw, &got))
			assert.Equal(t, want, got)
		})
	}
}
