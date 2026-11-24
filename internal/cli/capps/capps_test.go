package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/root"
	apitypes "github.com/dana-team/capp-backend/pkg/api/capps"
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

func TestCreate_ContainerPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want *int32
	}{
		{"set", []string{"--container-port", "3000"}, int32Ptr(3000)},
		{"omitted", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received apitypes.CappRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(apitypes.CappResponse{Name: received.Name}) //nolint:errcheck
			}))
			defer srv.Close()

			cmd, _ := newCreateCmd(t, srv.URL, "c1", "ns1")
			cmd.SetArgs(append([]string{"capps", "--name", "my-app", "--image", "img:latest"}, tc.args...))
			require.NoError(t, cmd.Execute())
			assert.Equal(t, tc.want, received.ContainerPort)
		})
	}
}

func TestUpdate_ContainerPort(t *testing.T) {
	var received apitypes.CappRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(apitypes.CappResponse{Name: "my-app", Image: "img:v1", ContainerPort: int32Ptr(3000)}) //nolint:errcheck
			return
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		json.NewEncoder(w).Encode(apitypes.CappResponse{Name: "my-app"}) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, _ := newUpdateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs([]string{"capps", "my-app", "--container-port", "9000"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32Ptr(9000), received.ContainerPort)
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
		Name:          "my-app",
		Namespace:     "ns1",
		Image:         "old:v1",
		ContainerPort: int32Ptr(3000),
		SecretVolumes: []apitypes.SecretVolume{
			{Name: "db-creds", SecretName: "db-secret", MountPath: "/etc/db"},
		},
		ConfigMapVolumes: []apitypes.ConfigMapVolume{
			{Name: "app-cfg", ConfigMapName: "app-config", MountPath: "/etc/cfg"},
		},
		ImagePullSecrets: []string{"registry-creds"},
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
	assert.Equal(t, existing.ContainerPort, received.ContainerPort)
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

// newRollbackCmd builds a minimal rollback cobra tree wired to a test HTTP server.
func newRollbackCmd(t *testing.T, serverURL, cluster, namespace, outputFmt string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
		OutputFmt: outputFmt,
	}

	h := New(state)
	parent := &cobra.Command{Use: "rollback"}
	h.RegisterRollbackCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

func TestRollback_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/clusters/c1/namespaces/ns1/capps/my-app/revisions/4/rollback", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.CappResponse{ //nolint:errcheck
			BareCappResponse: apitypes.BareCappResponse{Name: "my-app", Namespace: "ns1", Image: "img:rollback"},
		})
	}))
	defer srv.Close()

	cmd, buf := newRollbackCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "4", "-y"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "my-app")
	assert.Contains(t, buf.String(), "img:rollback")
}

func TestRollback_ConfirmYes(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.CappResponse{ //nolint:errcheck
			BareCappResponse: apitypes.BareCappResponse{Name: "my-app", Image: "img:v1"},
		})
	}))
	defer srv.Close()

	cmd, buf := newRollbackCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "1"})
	require.NoError(t, cmd.Execute())

	assert.True(t, called)
	assert.Contains(t, buf.String(), "to revision 1? This overwrites its current spec. [y/N]")
	assert.Contains(t, buf.String(), "img:v1")
}

func TestRollback_ConfirmAborted(t *testing.T) {
	for _, input := range []string{"n\n", "\n", "yes\n"} {
		t.Run(strings.TrimSpace(input), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("server must not be called when rollback is aborted")
			}))
			defer srv.Close()

			cmd, buf := newRollbackCmd(t, srv.URL, "c1", "ns1", "")
			cmd.SetIn(bytes.NewBufferString(input))
			cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "1"})
			require.NoError(t, cmd.Execute())

			assert.Contains(t, buf.String(), "Aborted.")
		})
	}
}

func TestRollback_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apitypes.CappResponse{ //nolint:errcheck
			BareCappResponse: apitypes.BareCappResponse{Name: "my-app", Image: "img:v2"},
		})
	}))
	defer srv.Close()

	cmd, buf := newRollbackCmd(t, srv.URL, "c1", "ns1", "json")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "2", "-y"})
	require.NoError(t, cmd.Execute())

	var result apitypes.CappResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Equal(t, "img:v2", result.Image)
}

func TestRollback_MissingToRevision(t *testing.T) {
	cmd, _ := newRollbackCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to-revision is required")
}

func TestRollback_NonPositiveToRevision(t *testing.T) {
	cmd, _ := newRollbackCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "0"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to-revision is required")
}

func TestRollback_MissingCluster(t *testing.T) {
	cmd, _ := newRollbackCmd(t, "http://unused", "", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "1"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cluster is required")
}

func TestRollback_MissingNamespace(t *testing.T) {
	cmd, _ := newRollbackCmd(t, "http://unused", "c1", "", "")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "1"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--namespace is required")
}

func TestRollback_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error": map[string]any{
				"code":    "NOT_FOUND",
				"message": `CappRevision "my-app revision 7" not found`,
				"status":  404,
			},
		})
	}))
	defer srv.Close()

	cmd, _ := newRollbackCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capps", "my-app", "--to-revision", "7", "-y"})
	err := cmd.Execute()
	require.Error(t, err)

	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "NOT_FOUND", apiErr.Code)
}
