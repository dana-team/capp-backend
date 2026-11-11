package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/pkg/api/capps"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rolloutCappPath = "/api/v1/clusters/c1/namespaces/ns1/capps/app1"

// connectTools returns a client session for the given tool sets, backed by a
// capp-backend at backendURL.
func connectTools(t *testing.T, backendURL string, sets ...ToolSet) *mcp.ClientSession {
	t.Helper()
	ctx := WithBearerToken(context.Background(), "test-token")

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	registry := NewRegistry(nil)
	for _, set := range sets {
		registry.Register(set)
	}
	registry.Mount(server, &Backend{BaseURL: backendURL})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// toolDescription returns the description of the named tool.
func toolDescription(t *testing.T, session *mcp.ClientSession, name string) string {
	t.Helper()
	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	require.Failf(t, "tool not registered", "%s", name)
	return ""
}

func writeCapp(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(capps.CappResponse{BareCappResponse: capps.BareCappResponse{Name: "app1"}})
}

func TestCappRollout_PostsAction(t *testing.T) {
	var got capps.RolloutActionRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, rolloutCappPath+"/rollout", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		writeCapp(w, http.StatusAccepted)
	}))
	defer backend.Close()

	result, err := connectTools(t, backend.URL, CappToolSet{}).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "capp_rollout",
		Arguments: map[string]any{
			"cluster": "c1", "namespace": "ns1", "name": "app1",
			"action": "promote", "revision": "app-00002",
		},
	})

	require.NoError(t, err)
	assert.False(t, result.IsError, "%v", result.Content)
	assert.Equal(t, capps.RolloutActionRequest{Action: "promote", Revision: "app-00002"}, got)
}

func TestCappRollout_RequiresRevision(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no backend request expected")
	}))
	defer backend.Close()

	result, err := connectTools(t, backend.URL, CappToolSet{}).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "capp_rollout",
		Arguments: map[string]any{"cluster": "c1", "namespace": "ns1", "name": "app1", "action": "promote"},
	})

	if err == nil {
		assert.True(t, result.IsError)
	}
}

func TestCappRollout_SurfacesBackendRejection(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"CONFLICT","message":"revision \"app-00001\" is not the current candidate \"app-00002\""}}`))
	}))
	defer backend.Close()

	result, err := connectTools(t, backend.URL, CappToolSet{}).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "capp_rollout",
		Arguments: map[string]any{
			"cluster": "c1", "namespace": "ns1", "name": "app1",
			"action": "promote", "revision": "app-00001",
		},
	})

	require.NoError(t, err)
	require.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "app-00002")
}

func TestCappRolloutStrategyRemove_SendsDelete(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, rolloutCappPath+"/rollout/strategy", r.URL.Path)
		called = true
		writeCapp(w, http.StatusOK)
	}))
	defer backend.Close()

	result, err := connectTools(t, backend.URL, CappToolSet{}).CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "capp_rollout_strategy_remove",
		Arguments: map[string]any{"cluster": "c1", "namespace": "ns1", "name": "app1"},
	})

	require.NoError(t, err)
	assert.False(t, result.IsError, "%v", result.Content)
	assert.True(t, called)
}

// Agents decide what to call from these descriptions, so the rollout behavior
// that moves live traffic or does not match the tool's general rule must be stated.
func TestRolloutToolDescriptions(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	defer backend.Close()
	session := connectTools(t, backend.URL, CappToolSet{}, CappRevisionToolSet{})

	update := toolDescription(t, session, "capp_update")
	assert.Contains(t, update, "keeps the current strategy")
	assert.Contains(t, update, "capp_rollout_strategy_remove")

	action := toolDescription(t, session, "capp_rollout")
	assert.Contains(t, action, "candidateRevision")
	assert.Contains(t, action, "asynchronously")

	remove := toolDescription(t, session, "capp_rollout_strategy_remove")
	assert.Contains(t, remove, "live traffic")

	assert.Contains(t, toolDescription(t, session, "rollback_capp"), "rollout strategy")
}
