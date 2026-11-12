package capps

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/root"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/boundcapps"
	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGetCmd(t *testing.T, serverURL, namespace, outputFmt string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   "c1",
		Namespace: namespace,
		OutputFmt: outputFmt,
	}
	parent := &cobra.Command{Use: "get"}
	New(state).RegisterGetCommand(parent)
	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)
	return parent, buf
}

func boundServer(t *testing.T, wantPath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, wantPath, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(boundcapps.Response{BoundCapps: []string{"web", "worker"}, ToReload: []string{"web"}})
	}))
}

func TestGetBound_Table(t *testing.T) {
	for _, tt := range []struct{ arg, path string }{
		{"secret/db", "/api/v1/clusters/c1/namespaces/ns1/secrets/db/bound"},
		{"configmap/cfg", "/api/v1/clusters/c1/namespaces/ns1/configmaps/cfg/bound"},
		{"cm/cfg", "/api/v1/clusters/c1/namespaces/ns1/configmaps/cfg/bound"},
	} {
		t.Run(tt.arg, func(t *testing.T) {
			srv := boundServer(t, tt.path)
			defer srv.Close()

			cmd, buf := newGetCmd(t, srv.URL, "ns1", "")
			cmd.SetArgs([]string{"capps", "--bound-to", tt.arg})
			require.NoError(t, cmd.Execute())

			out := buf.String()
			assert.Contains(t, out, "RELOADS")
			assert.Regexp(t, `web\s+true`, out)
			assert.Regexp(t, `worker\s+false`, out)
		})
	}
}

func TestGetBound_JSON(t *testing.T) {
	srv := boundServer(t, "/api/v1/clusters/c1/namespaces/ns1/secrets/db/bound")
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "ns1", outputJSON)
	cmd.SetArgs([]string{"capps", "--bound-to", "secret/db"})
	require.NoError(t, cmd.Execute())

	var got boundcapps.Response
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, []string{"web"}, got.ToReload)
}

func TestGetBound_InvalidUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no backend request expected")
	}))
	defer srv.Close()

	for _, tt := range []struct {
		name, ns string
		args     []string
	}{
		{"bad kind", "ns1", []string{"capps", "--bound-to", "pod/x"}},
		{"no slash", "ns1", []string{"capps", "--bound-to", "secret"}},
		{"empty name", "ns1", []string{"capps", "--bound-to", "secret/"}},
		{"with name arg", "ns1", []string{"capps", "app", "--bound-to", "secret/db"}},
		{"with -A", "ns1", []string{"capps", "-A", "--bound-to", "secret/db"}},
		{"no namespace", "", []string{"capps", "--bound-to", "secret/db"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := newGetCmd(t, srv.URL, tt.ns, "")
			cmd.SetArgs(tt.args)
			assert.Error(t, cmd.Execute())
		})
	}
}

func TestCreate_ReloadOnBoundChange(t *testing.T) {
	var received apitypes.CappRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/clusters/c2/namespaces/ns2/capps", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"my-app"}`))
	}))
	defer srv.Close()

	cmd, _ := newCreateCmd(t, srv.URL, "c2", "ns2")
	cmd.SetArgs([]string{"capps", "--name", "my-app", "--image", "img:latest", "--reload-on-bound-change"})
	require.NoError(t, cmd.Execute())
	assert.True(t, received.ReloadOnBoundResourceChange)
}

// updateReload runs `update capps my-app` against a Capp whose current
// reloadOnBoundResourceChange is current, and returns the value sent in the PUT.
func updateReload(t *testing.T, current bool, extraArgs ...string) bool {
	t.Helper()
	var received apitypes.CappRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			resp := apitypes.CappResponse{}
			resp.Name, resp.Image, resp.ReloadOnBoundResourceChange = "my-app", "img:v1", current
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		_, _ = w.Write([]byte(`{"name":"my-app"}`))
	}))
	defer srv.Close()

	cmd, _ := newUpdateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs(append([]string{"capps", "my-app"}, extraArgs...))
	require.NoError(t, cmd.Execute())
	return received.ReloadOnBoundResourceChange
}

func TestUpdate_ReloadOnBoundChange(t *testing.T) {
	assert.True(t, updateReload(t, true, "--image", "img:v2"), "unrelated update must keep the setting")
	assert.False(t, updateReload(t, true, "--reload-on-bound-change=false"))
	assert.True(t, updateReload(t, false, "--reload-on-bound-change"))
}
