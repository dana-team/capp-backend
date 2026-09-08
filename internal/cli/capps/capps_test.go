package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/root"
	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCreateCmd builds a minimal create cobra tree wired to a test HTTP server.
func newCreateCmd(t *testing.T, serverURL, cluster, namespace string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
	}

	h := New(state)
	parent := &cobra.Command{Use: "create"}
	h.RegisterCreateCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

// newUpdateCmd builds a minimal update cobra tree wired to a test HTTP server.
func newUpdateCmd(t *testing.T, serverURL, cluster, namespace string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
	}

	h := New(state)
	parent := &cobra.Command{Use: "update"}
	h.RegisterUpdateCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

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
			Path:      "sites/site/ns1/my-app.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "test-cluster", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), `Synced "my-app" to git`)
	assert.Contains(t, buf.String(), "abc123")
	assert.Contains(t, buf.String(), "sites/site/ns1/my-app.yaml")
}

func TestSync_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(syncResult{ //nolint:errcheck
			CommitSHA: "def456",
			Path:      "sites/test/prod/web.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "prod", "json")
	cmd.SetArgs([]string{"capps", "web"})
	require.NoError(t, cmd.Execute())

	var result syncResult
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Equal(t, "def456", result.CommitSHA)
	assert.Equal(t, "sites/test/prod/web.yaml", result.Path)
}

func TestSync_YAMLOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(syncResult{ //nolint:errcheck
			CommitSHA: "aaa111",
			Path:      "sites/site/ns/app.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "ns", "yaml")
	cmd.SetArgs([]string{"capps", "app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "commitSha: aaa111")
	assert.Contains(t, buf.String(), "path: sites/site/ns/app.yaml")
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

func TestCreate_RouteSpec_AllFields(t *testing.T) {
	var received apitypes.CappRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.CappResponse{Name: received.Name, RouteSpec: received.RouteSpec}) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, _ := newCreateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs([]string{
		"capps", "--name", "my-app", "--image", "img:latest",
		"--hostname", "my-app.example.com",
		"--tls-enabled",
		"--timeout-seconds", "30",
	})
	require.NoError(t, cmd.Execute())

	require.NotNil(t, received.RouteSpec)
	assert.Equal(t, "my-app.example.com", received.RouteSpec.Hostname)
	assert.True(t, received.RouteSpec.TLSEnabled)
	require.NotNil(t, received.RouteSpec.RouteTimeoutSeconds)
	assert.Equal(t, int64(30), *received.RouteSpec.RouteTimeoutSeconds)
}

func TestUpdate_RouteSpec_TLSWithoutHostname(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.CappResponse{Name: "my-app"}) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, _ := newUpdateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs([]string{"capps", "my-app", "--tls-enabled"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--hostname is required")
}
