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
		json.NewEncoder(w).Encode(apitypes.SyncResponse{ //nolint:errcheck
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
		json.NewEncoder(w).Encode(apitypes.SyncResponse{ //nolint:errcheck
			CommitSHA: "def456",
			Path:      "sites/test/prod/web.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "prod", "json")
	cmd.SetArgs([]string{"capps", "web"})
	require.NoError(t, cmd.Execute())

	var result apitypes.SyncResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Equal(t, "def456", result.CommitSHA)
	assert.Equal(t, "sites/test/prod/web.yaml", result.Path)
}

func TestSync_YAMLOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.SyncResponse{ //nolint:errcheck
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

func TestSync_Disable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/clusters/c1/namespaces/ns1/capps/my-app/sync", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.SyncResponse{ //nolint:errcheck
			Enabled:   false,
			CommitSHA: "def456",
			Path:      "sites/c1/ns1/my-app.yaml",
		})
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--disable", "--yes"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), `Git sync disabled for "my-app"`)
}

func TestSync_Disable_Aborted(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	cmd, buf := newSyncCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetIn(bytes.NewBufferString("n\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--disable"})
	require.NoError(t, cmd.Execute())

	assert.False(t, called, "no request should be sent when aborted")
	assert.Contains(t, buf.String(), "Aborted.")
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

// newMigrateCmd builds a minimal migrate cobra tree wired to a test HTTP server.
func newMigrateCmd(t *testing.T, serverURL, cluster, namespace, outputFmt string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
		OutputFmt: outputFmt,
	}

	h := New(state)
	parent := &cobra.Command{Use: "migrate"}
	h.RegisterMigrateCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

func TestMigrate(t *testing.T) {
	var received apitypes.MigrateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/clusters/east/namespaces/ns1/capps/my-app/migrate", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.MigrateResponse{ //nolint:errcheck
			Name:             "my-app",
			SourceCluster:    "east",
			SourceNamespace:  "ns1",
			TargetCluster:    received.TargetCluster,
			TargetNamespace:  received.TargetNamespace,
			SourceDeleted:    false,
			CopiedSecrets:    []string{"db-creds"},
			CopiedConfigMaps: []string{"app-config"},
		})
	}))
	defer srv.Close()

	cmd, buf := newMigrateCmd(t, srv.URL, "east", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west", "--target-namespace", "prod"})
	require.NoError(t, cmd.Execute())

	assert.Equal(t, "west", received.TargetCluster)
	assert.Equal(t, "prod", received.TargetNamespace)
	assert.False(t, received.DeleteSource)
	assert.Empty(t, received.TargetHostname)

	out := buf.String()
	assert.Contains(t, out, `Migrated "my-app" from east/ns1 to west/prod`)
	assert.Contains(t, out, "copied: 1 secrets, 1 configmaps")
}

func TestMigrateTargetHostname(t *testing.T) {
	var received apitypes.MigrateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.MigrateResponse{ //nolint:errcheck
			Name:            "my-app",
			SourceCluster:   "east",
			SourceNamespace: "ns1",
			TargetCluster:   "west",
			TargetNamespace: "prod",
		})
	}))
	defer srv.Close()

	cmd, _ := newMigrateCmd(t, srv.URL, "east", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west", "--target-namespace", "prod", "--target-hostname", "new.example.com"})
	require.NoError(t, cmd.Execute())

	assert.Equal(t, "new.example.com", received.TargetHostname)
}

func TestMigrateJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.MigrateResponse{ //nolint:errcheck
			Name:            "web",
			SourceCluster:   "east",
			SourceNamespace: "dev",
			TargetCluster:   "west",
			TargetNamespace: "prod",
			SourceDeleted:   true,
		})
	}))
	defer srv.Close()

	cmd, buf := newMigrateCmd(t, srv.URL, "east", "dev", "json")
	cmd.SetArgs([]string{"capps", "web", "--target-cluster", "west", "--target-namespace", "prod"})
	require.NoError(t, cmd.Execute())

	var result apitypes.MigrateResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Equal(t, "web", result.Name)
	assert.Equal(t, "west", result.TargetCluster)
	assert.Equal(t, "prod", result.TargetNamespace)
	assert.True(t, result.SourceDeleted)
}

func TestMigrateYAMLOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.MigrateResponse{ //nolint:errcheck
			Name:            "api",
			SourceCluster:   "c1",
			SourceNamespace: "ns1",
			TargetCluster:   "c2",
			TargetNamespace: "ns2",
		})
	}))
	defer srv.Close()

	cmd, buf := newMigrateCmd(t, srv.URL, "c1", "ns1", "yaml")
	cmd.SetArgs([]string{"capps", "api", "--target-cluster", "c2", "--target-namespace", "ns2"})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "name: api")
	assert.Contains(t, out, "targetCluster: c2")
	assert.Contains(t, out, "targetNamespace: ns2")
}

func TestMigrateMissingCluster(t *testing.T) {
	cmd, _ := newMigrateCmd(t, "http://unused", "", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west", "--target-namespace", "prod"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cluster is required")
}

func TestMigrateMissingNamespace(t *testing.T) {
	cmd, _ := newMigrateCmd(t, "http://unused", "c1", "", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west", "--target-namespace", "prod"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--namespace is required")
}

func TestMigrateMissingTargetCluster(t *testing.T) {
	cmd, _ := newMigrateCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-namespace", "prod"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--target-cluster is required")
}

func TestMigrateMissingTargetNamespace(t *testing.T) {
	cmd, _ := newMigrateCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--target-namespace is required")
}

func TestMigrateMissingName(t *testing.T) {
	cmd, _ := newMigrateCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps"})
	err := cmd.Execute()
	require.Error(t, err)
}

func TestMigrateAPIError(t *testing.T) {
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

	cmd, _ := newMigrateCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "gone", "--target-cluster", "west", "--target-namespace", "prod"})
	err := cmd.Execute()
	require.Error(t, err)

	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "CAPP_NOT_FOUND", apiErr.Code)
}

func TestMigrateDeleteSourceConfirmAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called when user aborts confirmation")
	}))
	defer srv.Close()

	cmd, buf := newMigrateCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetIn(bytes.NewBufferString("n\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--target-cluster", "west", "--target-namespace", "prod", "--delete-source"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "Aborted.")
}

func TestUpdate_PreservesAllFields(t *testing.T) {
	existing := apitypes.CappResponse{
		Name:      "my-app",
		Namespace: "ns1",
		Image:     "old:v1",
		SecretVolumes: []apitypes.SecretVolume{
			{Name: "db-creds", SecretName: "db-secret", MountPath: "/etc/db"},
		},
		ConfigMapVolumes: []apitypes.ConfigMapVolume{
			{Name: "app-cfg", ConfigMapName: "app-config", MountPath: "/etc/cfg"},
		},
		ImagePullSecrets: []string{"registry-creds"},
		EventSourcesSpec: &apitypes.EventSourcesSpec{
			Sources: []apitypes.SourceConfig{
				{Name: "ping", PingSourceConfig: &apitypes.PingSourceConfig{Schedule: "*/5 * * * *"}},
			},
		},
	}

	var received apitypes.CappRequest
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		callCount++
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(existing) //nolint:errcheck
			return
		}
		assert.Equal(t, http.MethodPut, r.Method)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		json.NewEncoder(w).Encode(apitypes.CappResponse{Name: "my-app", Image: "new:v2"}) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newUpdateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs([]string{"capps", "my-app", "--image", "new:v2"})
	require.NoError(t, cmd.Execute())

	assert.Equal(t, 2, callCount)
	assert.NotEmpty(t, buf.String())
	assert.Equal(t, "new:v2", received.Image)
	assert.Equal(t, existing.SecretVolumes, received.SecretVolumes)
	assert.Equal(t, existing.ConfigMapVolumes, received.ConfigMapVolumes)
	assert.Equal(t, existing.ImagePullSecrets, received.ImagePullSecrets)
	assert.Equal(t, existing.EventSourcesSpec, received.EventSourcesSpec)
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
