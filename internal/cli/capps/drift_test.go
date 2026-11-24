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
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driftBackend answers like a backend whose Capp "app" was changed to image
// "live" while its desired image is "desired".
type driftBackend struct {
	t        *testing.T
	reverted bool
	puts     []string
	body     apitypes.CappRequest
	actions  []string
}

func (b *driftBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	changed := &drift.Drift{State: drift.StateChanged, Generation: 7, LiveVersion: "v1", Fields: []drift.Field{{Path: "/spec/image", Desired: "old", Live: "new"}}}
	capp := apitypes.CappResponse{BareCappResponse: apitypes.BareCappResponse{Name: "app", Image: "live"}, Drift: changed}
	if b.reverted {
		capp.Image, capp.Drift = "desired", &drift.Drift{State: drift.StateInSync, Generation: 7}
	}
	var out any = capp
	switch {
	case strings.Contains(r.URL.Path, "/drift/"):
		var request drift.ActionRequest
		assert.NoError(b.t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(b.t, drift.ActionRequest{Generation: 7, LiveVersion: "v1"}, request)
		action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		b.actions = append(b.actions, action)
		b.reverted = action == drift.OnDriftRevert
		out = drift.ActionResponse{}
	case r.Method == http.MethodPut:
		b.puts = append(b.puts, r.URL.RawQuery)
		if capp.Drift.State == drift.StateChanged && r.URL.Query().Get("onDrift") == "" {
			w.WriteHeader(http.StatusConflict)
			out = map[string]any{"error": map[string]any{"code": "DRIFT_DETECTED", "message": "m", "status": 409, "details": map[string]any{"drift": changed}}}
			break
		}
		assert.NoError(b.t, json.NewDecoder(r.Body).Decode(&b.body))
	}
	assert.NoError(b.t, json.NewEncoder(w).Encode(out))
}

func runDrift(t *testing.T, b *driftBackend, args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	h := New(&root.State{Client: client.New(server.URL, "token", false), Cluster: "c1", Namespace: "ns1"})
	cmd := &cobra.Command{Use: "cappctl", SilenceUsage: true, SilenceErrors: true}
	update, revert, accept := &cobra.Command{Use: "update"}, &cobra.Command{Use: "revert"}, &cobra.Command{Use: "accept"}
	h.RegisterUpdateCommand(update)
	h.RegisterDriftCommand(revert, accept)
	cmd.AddCommand(update, revert, accept)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestUpdateExplainsDrift(t *testing.T) {
	_, err := runDrift(t, &driftBackend{t: t}, "update", "capps", "app", "--state", "disabled")
	require.ErrorContains(t, err, "/spec/image: desired old, live new")
	require.ErrorContains(t, err, "--on-drift=revert")
}

func TestUpdateAcceptsDrift(t *testing.T) {
	b := &driftBackend{t: t}
	_, err := runDrift(t, b, "update", "capps", "app", "--state", "disabled", "--on-drift", "accept")
	require.NoError(t, err)
	assert.Equal(t, []string{"onDrift=accept"}, b.puts)
	assert.Equal(t, "live", b.body.Image)
	assert.Empty(t, b.actions)
}

func TestUpdateRevertsBeforeSeedingTheRequest(t *testing.T) {
	b := &driftBackend{t: t}
	_, err := runDrift(t, b, "update", "capps", "app", "--state", "disabled", "--on-drift", "revert")
	require.NoError(t, err)
	assert.Equal(t, []string{"revert"}, b.actions)
	assert.Equal(t, "desired", b.body.Image, "the drifted value must not be sent back")
	assert.Equal(t, []string{""}, b.puts, "new drift after the revert must not be accepted silently")
}

func TestUpdateRejectsInvalidOnDrift(t *testing.T) {
	b := &driftBackend{t: t}
	_, err := runDrift(t, b, "update", "capps", "app", "--on-drift", "force")
	require.ErrorContains(t, err, "--on-drift must be")
	assert.Empty(t, b.puts)
}

func TestDriftCommands(t *testing.T) {
	for _, action := range []string{"revert", "accept"} {
		b := &driftBackend{t: t}
		out, err := runDrift(t, b, action, "capps", "app", "-y")
		require.NoError(t, err)
		assert.Equal(t, []string{action}, b.actions)
		assert.Contains(t, out, "/spec/image: desired old, live new")

		b = &driftBackend{t: t}
		out, err = runDrift(t, b, action, "capps", "app")
		require.NoError(t, err)
		assert.Contains(t, out, "Aborted.")
		assert.Empty(t, b.actions)
	}
}
