package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/root"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSyncCmd builds a minimal sync cobra tree wired to a test HTTP server.
func newSyncCmd(t *testing.T, serverURL, cluster, namespace, outputFmt string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
		OutputFmt: outputFmt,
	}

	h := New(state)
	parent := &cobra.Command{Use: "sync"}
	h.RegisterSyncCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

func TestSync_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/clusters/test-cluster/namespaces/ns1/capps/my-app/sync", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(syncResult{ //nolint:errcheck
			CommitSHA: "abc123",
			Path:      "sites/nova/ns1/my-app.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "test-cluster", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), `Synced "my-app" to git`)
	assert.Contains(t, buf.String(), "abc123")
	assert.Contains(t, buf.String(), "sites/nova/ns1/my-app.yaml")
}

func TestSync_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(syncResult{ //nolint:errcheck
			CommitSHA: "def456",
			Path:      "sites/five/prod/web.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "prod", "json")
	cmd.SetArgs([]string{"capps", "web"})
	require.NoError(t, cmd.Execute())

	var result syncResult
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Equal(t, "def456", result.CommitSHA)
	assert.Equal(t, "sites/five/prod/web.yaml", result.Path)
}

func TestSync_YAMLOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(syncResult{ //nolint:errcheck
			CommitSHA: "aaa111",
			Path:      "sites/nova/ns/app.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "ns", "yaml")
	cmd.SetArgs([]string{"capps", "app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "commitSha: aaa111")
	assert.Contains(t, buf.String(), "path: sites/nova/ns/app.yaml")
}

func TestSync_MissingCluster(t *testing.T) {
	cmd, _ := newSyncCmd(t, "http://unused", "", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cluster is required")
}

func TestSync_MissingNamespace(t *testing.T) {
	cmd, _ := newSyncCmd(t, "http://unused", "c1", "", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--namespace is required")
}

func TestSync_MissingName(t *testing.T) {
	cmd, _ := newSyncCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps"})
	err := cmd.Execute()
	require.Error(t, err)
}

func TestSync_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error": map[string]any{
				"code":    "CAPP_NOT_FOUND",
				"message": `Capp "gone" not found`,
				"status":  404,
			},
		})
	}))
	defer srv.Close()

	cmd, _ := newSyncCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "gone"})
	err := cmd.Execute()
	require.Error(t, err)

	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "CAPP_NOT_FOUND", apiErr.Code)
}

func TestSync_GitOpsDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error": map[string]any{
				"code":    "NOT_SUPPORTED",
				"message": "sync is not supported",
				"status":  501,
			},
		})
	}))
	defer srv.Close()

	cmd, _ := newSyncCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}
