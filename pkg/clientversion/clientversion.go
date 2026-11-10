// Package clientversion lets capp-backend warn clients that are older than it.
//
// A client sends "<name>/<version>" as the first User-Agent product token.
// When the name belongs to a known Client and the version is at least one
// minor release behind the backend, the backend sets WarningHeader on the
// response and the client shows that message to its user.
//
// Only clients released with the backend's version numbers belong in Known.
// Other software sends versions on unrelated numbering ("Go-http-client/1.1",
// "Mozilla/5.0"), and comparing those with the backend's would produce false
// warnings.
//
// The package has no Gin or Kubernetes imports so that the server and its
// clients can all use it.
package clientversion

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// WarningHeader is the response header the backend sets when the calling
// client is outdated. Its value is a human-readable message.
const WarningHeader = "X-Client-Warning"

// Client is a program that calls capp-backend and is released with the same
// version numbers as the backend.
type Client struct {
	// Name is the User-Agent product token the client sends, as in
	// "cappctl/v0.6.2".
	Name string
}

// Cappctl is the capp-backend CLI.
var Cappctl = Client{Name: "cappctl"}

// Known lists the clients the backend checks. To check another client,
// declare it like Cappctl and add it here; the backend middleware and the
// version comparison do not change.
var Known = []Client{Cappctl}

// UserAgent returns the User-Agent value c sends at version.
func (c Client) UserAgent(version string) string {
	return c.Name + "/" + version
}

// Warning returns the message sent to an outdated c. Callers must check
// Outdated first, so both values are valid semantic versions.
func (c Client) Warning(clientVersion, serverVersion string) string {
	s := canonical(serverVersion)
	return fmt.Sprintf(
		"%s %s is older than capp-backend %s; upgrade %s to %s or later to avoid unexpected errors",
		c.Name, canonical(clientVersion), s, c.Name, semver.MajorMinor(s),
	)
}

// FromUserAgent returns the client named by the first User-Agent product
// token and the version it sent. ok is false when that name is not one of
// clients or the version is empty.
func FromUserAgent(userAgent string, clients []Client) (client Client, version string, ok bool) {
	first, _, _ := strings.Cut(userAgent, " ")
	name, version, found := strings.Cut(first, "/")
	if !found || version == "" {
		return Client{}, "", false
	}
	for _, c := range clients {
		if c.Name == name {
			return c, version, true
		}
	}
	return Client{}, "", false
}

// Outdated reports whether client is at least one minor release behind
// server, for example v0.5.2 against v0.6.2. Patch differences are ignored.
// It returns false when either value is not a semantic version, such as the
// "dev" version of local builds.
func Outdated(client, server string) bool {
	c, s := canonical(client), canonical(server)
	if !semver.IsValid(c) || !semver.IsValid(s) {
		return false
	}
	return semver.Compare(semver.MajorMinor(c), semver.MajorMinor(s)) < 0
}

// MajorMinor returns the "vMAJOR.MINOR" prefix of version, for example "v0.5"
// for "0.5.2". It returns "" when version is not a semantic version.
func MajorMinor(version string) string {
	return semver.MajorMinor(canonical(version))
}

// canonical adds the "v" prefix that golang.org/x/mod/semver requires.
func canonical(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}
