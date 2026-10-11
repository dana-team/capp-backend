package capprevisions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/internal/cli/root"
	apitypes "github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
)

// newGetCmd builds a minimal get cobra tree wired to a test HTTP server.
func newGetCmd(t *testing.T, serverURL, cluster, namespace, outputFmt string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   cluster,
		Namespace: namespace,
		OutputFmt: outputFmt,
	}

	h := New(state)
	parent := &cobra.Command{Use: "get"}
	h.RegisterGetCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

func sampleList() apitypes.CappRevisionListResponse {
	return apitypes.CappRevisionListResponse{
		Items: []apitypes.CappRevisionResponse{
			{
				BareCappResponse: apitypes.BareCappResponse{Name: "rev-3", Image: "img:v3", Size: "medium", State: "enabled", ContainerName: "web"},
				RevisionNumber:   3,
			},
			{
				BareCappResponse: apitypes.BareCappResponse{Name: "rev-2", Image: "img:v2", Size: "small", State: "enabled", ContainerName: "web"},
				RevisionNumber:   2,
			},
		},
		Total: 2,
	}
}

func TestGetRevisions_List(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/clusters/c1/namespaces/ns1/capps/my-app/revisions", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sampleList()) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capprevisions", "--capp", "my-app"})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "REVISION")
	assert.Contains(t, out, "AGE")
	assert.NotContains(t, out, "CONTAINER")
	assert.Contains(t, out, "img:v3")
	assert.Contains(t, out, "img:v2")
	assert.Contains(t, out, "3")
	assert.Contains(t, out, "2")
}

func TestGetRevisions_Wide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sampleList()) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "c1", "ns1", "wide")
	cmd.SetArgs([]string{"capprevisions", "--capp", "my-app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "CONTAINER")
	assert.Contains(t, buf.String(), "web")
}

func TestGetRevisions_Single(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/clusters/c1/namespaces/ns1/capps/my-app/revisions/2", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sampleList().Items[1]) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capprevisions", "2", "--capp", "my-app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "img:v2")
	assert.NotContains(t, buf.String(), "img:v3")
}

func TestGetRevisions_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sampleList()) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "c1", "ns1", "json")
	cmd.SetArgs([]string{"capprevisions", "--capp", "my-app"})
	require.NoError(t, cmd.Execute())

	var result apitypes.CappRevisionListResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result.Items, 2)
	assert.Equal(t, 3, result.Items[0].RevisionNumber)
	assert.Equal(t, 2, result.Total)
}

func TestGetRevisions_YAMLOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sampleList().Items[0]) //nolint:errcheck
	}))
	defer srv.Close()

	cmd, buf := newGetCmd(t, srv.URL, "c1", "ns1", "yaml")
	cmd.SetArgs([]string{"capprevisions", "3", "--capp", "my-app"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "revisionNumber: 3")
}

func TestGetRevisions_MissingCapp(t *testing.T) {
	cmd, _ := newGetCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capprevisions"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capp")
}

func TestGetRevisions_MissingCluster(t *testing.T) {
	cmd, _ := newGetCmd(t, "http://unused", "", "ns1", "")
	cmd.SetArgs([]string{"capprevisions", "--capp", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cluster is required")
}

func TestGetRevisions_MissingNamespace(t *testing.T) {
	cmd, _ := newGetCmd(t, "http://unused", "c1", "", "")
	cmd.SetArgs([]string{"capprevisions", "--capp", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--namespace is required")
}

func TestGetRevisions_InvalidRevision(t *testing.T) {
	cmd, _ := newGetCmd(t, "http://unused", "c1", "ns1", "")
	cmd.SetArgs([]string{"capprevisions", "0", "--capp", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive integer")
}

func TestGetRevisions_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error": map[string]any{
				"code":    "NOT_FOUND",
				"message": `CappRevision "my-app revision 9" not found`,
				"status":  404,
			},
		})
	}))
	defer srv.Close()

	cmd, _ := newGetCmd(t, srv.URL, "c1", "ns1", "")
	cmd.SetArgs([]string{"capprevisions", "9", "--capp", "my-app"})
	err := cmd.Execute()
	require.Error(t, err)

	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "NOT_FOUND", apiErr.Code)
}
