package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/client"
	"github.com/dana-team/capp-backend/pkg/clientversion"
)

const upgradeWarning = "cappctl v0.5.2 is older than capp-backend v0.6.2; upgrade cappctl to v0.6 or later to avoid unexpected errors"

func TestWithClientVersionSendsUserAgentAndReportsWarning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "success response", status: http.StatusOK},
		{name: "error response", status: http.StatusBadRequest, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "cappctl/v0.5.2", r.UserAgent())
				w.Header().Set(clientversion.WarningHeader, upgradeWarning)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			var warnings []string
			c := client.New(srv.URL, "token", false).
				WithClientVersion(clientversion.Cappctl, "v0.5.2", func(msg string) { warnings = append(warnings, msg) })

			err := c.Get(t.Context(), "/api/v1/clusters", nil)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []string{upgradeWarning}, warnings)
		})
	}
}

func TestWithClientVersionSurvivesClientCopies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "cappctl/v0.5.2", r.UserAgent())
		w.Header().Set(clientversion.WarningHeader, upgradeWarning)
	}))
	defer srv.Close()

	var warnings int
	c := client.New(srv.URL, "", false).
		WithClientVersion(clientversion.Cappctl, "v0.5.2", func(string) { warnings++ }).
		WithToken("token").
		WithoutRedirects()

	require.NoError(t, c.Get(t.Context(), "/", nil))
	assert.Equal(t, 1, warnings)
}

func TestClientWithoutClientVersionKeepsDefaultUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.False(t, strings.HasPrefix(r.UserAgent(), clientversion.Cappctl.Name+"/"), "unexpected User-Agent %q", r.UserAgent())
		w.Header().Set(clientversion.WarningHeader, upgradeWarning)
	}))
	defer srv.Close()

	// capp-mcp uses the client without WithClientVersion; the header is ignored.
	require.NoError(t, client.New(srv.URL, "token", false).Get(t.Context(), "/", nil))
}

func TestWithClientVersionNilHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(clientversion.WarningHeader, upgradeWarning)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "token", false).WithClientVersion(clientversion.Cappctl, "v0.5.2", nil)
	require.NoError(t, c.Get(t.Context(), "/", nil))
}
