package clientversion_test

import (
	"testing"

	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/stretchr/testify/assert"
)

func TestOutdated(t *testing.T) {
	for _, tc := range []struct {
		name           string
		client, server string
		want           bool
	}{
		{name: "one minor behind", client: "v0.5.2", server: "v0.6.2", want: true},
		{name: "several minors behind", client: "v0.3.9", server: "v0.6.0", want: true},
		{name: "major behind", client: "v0.9.0", server: "v1.0.0", want: true},
		{name: "older patch only", client: "v0.6.0", server: "v0.6.2", want: false},
		{name: "same version", client: "v0.6.2", server: "v0.6.2", want: false},
		{name: "newer client", client: "v0.7.0", server: "v0.6.2", want: false},
		{name: "versions without v prefix", client: "0.5.2", server: "0.6.2", want: true},
		{name: "server pre-release counts as its minor", client: "v0.5.2", server: "v0.6.0-rc.1", want: true},
		{name: "client pre-release of same minor", client: "v0.6.0-rc.1", server: "v0.6.2", want: false},
		{name: "dev client", client: "dev", server: "v0.6.2", want: false},
		{name: "dev server", client: "v0.5.2", server: "dev", want: false},
		{name: "empty client", client: "", server: "v0.6.2", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, clientversion.Outdated(tc.client, tc.server))
		})
	}
}

func TestUserAgentRoundTrip(t *testing.T) {
	ua := clientversion.Cappctl.UserAgent("v0.5.2")
	assert.Equal(t, "cappctl/v0.5.2", ua)

	c, v, ok := clientversion.FromUserAgent(ua, clientversion.Known)
	assert.True(t, ok)
	assert.Equal(t, clientversion.Cappctl, c)
	assert.Equal(t, "v0.5.2", v)
}

func TestFromUserAgent(t *testing.T) {
	frontend := clientversion.Client{Name: "capp-frontend"}
	clients := []clientversion.Client{clientversion.Cappctl, frontend}

	for _, tc := range []struct {
		name, userAgent string
		wantClient      clientversion.Client
		wantVersion     string
		ok              bool
	}{
		{name: "cappctl with comment", userAgent: "cappctl/v0.5.2 (linux/amd64)", wantClient: clientversion.Cappctl, wantVersion: "v0.5.2", ok: true},
		{name: "another listed client", userAgent: "capp-frontend/v0.4.0", wantClient: frontend, wantVersion: "v0.4.0", ok: true},
		{name: "unlisted go client", userAgent: "Go-http-client/1.1"},
		{name: "unlisted browser", userAgent: "Mozilla/5.0 (X11; Linux x86_64)"},
		{name: "listed client not first", userAgent: "proxy/1.0 cappctl/v0.5.2"},
		{name: "missing version", userAgent: "cappctl/"},
		{name: "missing slash", userAgent: "cappctl"},
		{name: "empty", userAgent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, v, ok := clientversion.FromUserAgent(tc.userAgent, clients)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.wantClient, c)
			assert.Equal(t, tc.wantVersion, v)
		})
	}
}

func TestMajorMinor(t *testing.T) {
	for in, want := range map[string]string{
		"v0.5.2":      "v0.5",
		"0.5.2":       "v0.5",
		"v0.53":       "v0.53",
		"v1.0.0-rc.1": "v1.0",
		"dev":         "",
		"":            "",
	} {
		assert.Equal(t, want, clientversion.MajorMinor(in), "MajorMinor(%q)", in)
	}
}

func TestKnownChecksOnlyCappctl(t *testing.T) {
	assert.Equal(t, []clientversion.Client{clientversion.Cappctl}, clientversion.Known)
}

func TestWarning(t *testing.T) {
	assert.Equal(t,
		"cappctl v0.5.2 is older than capp-backend v0.6.2; upgrade cappctl to v0.6 or later to avoid unexpected errors",
		clientversion.Cappctl.Warning("0.5.2", "v0.6.2"),
	)
	assert.Equal(t,
		"capp-frontend v0.4.0 is older than capp-backend v0.6.2; upgrade capp-frontend to v0.6 or later to avoid unexpected errors",
		clientversion.Client{Name: "capp-frontend"}.Warning("v0.4.0", "v0.6.2"),
	)
}
