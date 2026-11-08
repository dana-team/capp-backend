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

const (
	rolloutCappPath = "/api/v1/clusters/c1/namespaces/ns1/capps/my-app"
	rolloutPath     = rolloutCappPath + "/rollout"
)

func percent(v int64) *int64 { return &v }

// newRolloutCmd builds a minimal rollout cobra tree wired to a test HTTP server.
func newRolloutCmd(t *testing.T, serverURL string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	state := &root.State{
		Client:    client.New(serverURL, "test-token", false),
		Cluster:   "c1",
		Namespace: "ns1",
	}

	h := New(state)
	parent := &cobra.Command{Use: "rollout"}
	h.RegisterRolloutCommand(parent)

	buf := &bytes.Buffer{}
	parent.SetOut(buf)
	parent.SetErr(buf)

	return parent, buf
}

func writeCapp(w http.ResponseWriter, resp apitypes.CappResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func candidateCapp() apitypes.CappResponse {
	return apitypes.CappResponse{
		BareCappResponse: apitypes.BareCappResponse{Name: "my-app", Namespace: "ns1"},
		Status: apitypes.CappStatusResponse{
			Rollout: &apitypes.RolloutStatusResponse{CandidateRevision: "app-00002", Phase: "Holding"},
		},
	}
}

func TestParseRolloutStages(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    []apitypes.RolloutStage
		wantErr bool
	}{
		{
			name:   "canary",
			values: []string{"10:5m", "100"},
			want:   []apitypes.RolloutStage{{Percent: percent(10), Duration: "5m"}, {Percent: percent(100)}},
		},
		{name: "blue-green", values: []string{"0"}, want: []apitypes.RolloutStage{{Percent: percent(0)}}},
		{name: "not a number", values: []string{"x:5m"}, wantErr: true},
		{name: "above 100", values: []string{"101"}, wantErr: true},
		{name: "negative", values: []string{"-1"}, wantErr: true},
		{name: "bad duration", values: []string{"10:soon"}, wantErr: true},
		{name: "empty duration", values: []string{"10:"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRolloutStages(tt.values)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCreate_RolloutFlags(t *testing.T) {
	var got apitypes.CappRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		writeCapp(w, apitypes.CappResponse{BareCappResponse: apitypes.BareCappResponse{Name: "my-app"}})
	}))
	defer srv.Close()

	cmd, buf := newCreateCmd(t, srv.URL, "c1", "ns1")
	cmd.SetArgs([]string{"capps", "--name", "my-app", "--image", "nginx",
		"--rollout-stage", "10:5m", "--rollout-stage", "100",
		"--rollout-auto-rollback", "--rollout-progress-deadline", "10m"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "my-app")
	require.NotNil(t, got.RouteSpec)
	assert.Equal(t, &apitypes.RolloutStrategy{
		Stages:           []apitypes.RolloutStage{{Percent: percent(10), Duration: "5m"}, {Percent: percent(100)}},
		AutoRollback:     true,
		ProgressDeadline: "10m",
	}, got.RouteSpec.RolloutStrategy)
}

func TestCreate_RolloutAutoRollbackWithoutStages(t *testing.T) {
	cmd, _ := newCreateCmd(t, "http://127.0.0.1:0", "c1", "ns1")
	cmd.SetArgs([]string{"capps", "--name", "my-app", "--image", "nginx", "--rollout-auto-rollback"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--rollout-stage")
}

func TestUpdate_RolloutFlags(t *testing.T) {
	existing := apitypes.CappResponse{BareCappResponse: apitypes.BareCappResponse{
		Name:  "my-app",
		Image: "nginx",
		RouteSpec: &apitypes.RouteSpec{
			Hostname: "a.example.com",
			RolloutStrategy: &apitypes.RolloutStrategy{
				Stages:           []apitypes.RolloutStage{{Percent: percent(50)}},
				ProgressDeadline: "10m0s",
			},
		},
	}}

	t.Run("flag on existing strategy keeps its stages", func(t *testing.T) {
		var got apitypes.CappRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			}
			writeCapp(w, existing)
		}))
		defer srv.Close()

		cmd, _ := newUpdateCmd(t, srv.URL, "c1", "ns1")
		cmd.SetArgs([]string{"capps", "my-app", "--rollout-auto-rollback"})
		require.NoError(t, cmd.Execute())

		assert.Equal(t, "a.example.com", got.RouteSpec.Hostname)
		assert.True(t, got.RouteSpec.RolloutStrategy.AutoRollback)
		assert.Equal(t, "10m0s", got.RouteSpec.RolloutStrategy.ProgressDeadline)
		assert.Equal(t, []apitypes.RolloutStage{{Percent: percent(50)}}, got.RouteSpec.RolloutStrategy.Stages)
	})

	t.Run("stages replace all stages", func(t *testing.T) {
		var got apitypes.CappRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/clusters/east/namespaces/team-a/capps/my-app", r.URL.Path)
			if r.Method == http.MethodPut {
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			}
			writeCapp(w, existing)
		}))
		defer srv.Close()

		cmd, _ := newUpdateCmd(t, srv.URL, "east", "team-a")
		cmd.SetArgs([]string{"capps", "my-app", "--rollout-stage", "0"})
		require.NoError(t, cmd.Execute())

		assert.Equal(t, []apitypes.RolloutStage{{Percent: percent(0)}}, got.RouteSpec.RolloutStrategy.Stages)
	})
}

func TestRollout_DefaultsToCandidate(t *testing.T) {
	var got apitypes.RolloutActionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			assert.Equal(t, rolloutCappPath, r.URL.Path)
		case http.MethodPost:
			assert.Equal(t, rolloutPath, r.URL.Path)
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(candidateCapp()) //nolint:errcheck
			return
		}
		writeCapp(w, candidateCapp())
	}))
	defer srv.Close()

	cmd, buf := newRolloutCmd(t, srv.URL)
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--action", "promote"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "app-00002")
	assert.Equal(t, apitypes.RolloutActionRequest{Action: "promote", Revision: "app-00002"}, got)
}

func TestRollout_ExplicitRevisionWithYes(t *testing.T) {
	var got apitypes.RolloutActionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "must not need to read the candidate")
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		writeCapp(w, candidateCapp())
	}))
	defer srv.Close()

	cmd, _ := newRolloutCmd(t, srv.URL)
	cmd.SetArgs([]string{"capps", "my-app", "--action", "abort", "--revision", "app-00002", "-y"})
	require.NoError(t, cmd.Execute())

	assert.Equal(t, apitypes.RolloutActionRequest{Action: "abort", Revision: "app-00002"}, got)
}

func TestRollout_Declined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotEqual(t, http.MethodPost, r.Method)
		writeCapp(w, candidateCapp())
	}))
	defer srv.Close()

	cmd, buf := newRolloutCmd(t, srv.URL)
	cmd.SetIn(bytes.NewBufferString("n\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--action", "promote"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "Aborted.")
}

func TestRollout_NoCandidate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCapp(w, apitypes.CappResponse{})
	}))
	defer srv.Close()

	cmd, _ := newRolloutCmd(t, srv.URL)
	cmd.SetArgs([]string{"capps", "my-app", "--action", "promote"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidate")
}

func TestRollout_InvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"yes requires revision", []string{"--action", "promote", "-y"}, "--revision"},
		{"both", []string{"--action", "promote", "--remove-strategy"}, "exactly one"},
		{"neither", nil, "exactly one"},
		{"unknown action", []string{"--action", "pause"}, "--action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("no request expected")
			}))
			defer srv.Close()

			cmd, _ := newRolloutCmd(t, srv.URL)
			cmd.SetArgs(append([]string{"capps", "my-app"}, tt.args...))

			err := cmd.Execute()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRollout_RemoveStrategy_PromptWarns(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, rolloutPath+"/strategy", r.URL.Path)
		deleted = true
		writeCapp(w, apitypes.CappResponse{BareCappResponse: apitypes.BareCappResponse{Name: "my-app"}})
	}))
	defer srv.Close()

	cmd, buf := newRolloutCmd(t, srv.URL)
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetArgs([]string{"capps", "my-app", "--remove-strategy"})
	require.NoError(t, cmd.Execute())

	assert.True(t, deleted)
	assert.Contains(t, buf.String(), "promote")
	assert.Contains(t, buf.String(), "pin")
}

func TestRolloutPhaseColumnIsWide(t *testing.T) {
	var column *string
	for _, col := range tableCols {
		if col.Header == "ROLLOUT" {
			assert.True(t, col.Wide)
			value := col.Value(candidateCapp())
			column = &value
			assert.Empty(t, col.Value(apitypes.CappResponse{}))
		}
	}
	require.NotNil(t, column, "ROLLOUT column missing")
	assert.Equal(t, "Holding", *column)
}
