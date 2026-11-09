package middleware

import (
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/mod/semver"
)

// ClientVersion returns a Gin middleware that warns users of a client that
// is at least one minor release behind this backend (for example cappctl
// v0.5.2 against capp-backend v0.6.2).
//
// A client identifies itself with "User-Agent: <name>/<version>". Only the
// given clients are checked (the server passes clientversion.Known). For an
// outdated client the middleware sets the clientversion.WarningHeader
// response header, which the client shows to its user, and increments
// capp_backend_outdated_client_requests_total. Both happen before the rest of
// the chain runs, so they also cover requests that a later middleware
// rejects, such as 401 and 429 responses.
//
// The "outdated client" warning is logged after the chain completes, so it
// can include the response status and the user that Auth resolved. The user
// is present only on authenticated routes in auth modes that identify users
// (openshift, ldap); passthrough forwards the token without knowing whose it
// is. Unlisted clients, patch-only differences, newer clients and unversioned
// builds ("dev" on either side) pass through unchanged.
func ClientVersion(serverVersion string, clients []clientversion.Client, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		client, version, ok := clientversion.FromUserAgent(c.Request.UserAgent(), clients)
		if !ok || !clientversion.Outdated(version, serverVersion) {
			c.Next()
			return
		}

		c.Header(clientversion.WarningHeader, client.Warning(version, serverVersion))
		outdatedClientRequests.WithLabelValues(client.Name, versionLabel(version, serverVersion)).Inc()

		c.Next()

		fields := []zap.Field{
			zap.String("client", client.Name),
			zap.String("clientVersion", version),
			zap.String("serverVersion", serverVersion),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.String("ip", c.ClientIP()),
		}
		if user := authenticatedUser(c); user != "" {
			fields = append(fields, zap.String("user", user))
		}
		logger.Warn("outdated client", fields...)
	}
}

// authenticatedUser returns the username Auth resolved for this request, or
// "" when the route has no authentication, authentication failed, or the
// auth mode does not identify users. It reads only the username, never the
// credential's token.
func authenticatedUser(c *gin.Context) string {
	value, ok := c.Get(string(CredentialKey))
	if !ok {
		return ""
	}
	cred, ok := value.(auth.ClusterCredential)
	if !ok {
		return ""
	}
	return cred.ImpersonateUser
}

// versionLabel returns the metric label for an outdated client version: its
// major.minor ("v0.5") when it has the backend's major version, otherwise
// only its major ("v0.x"). Any caller can put any version in the User-Agent,
// and this check runs before authentication, so the label must not take
// arbitrary values: older minors of the backend's major and older majors are
// both finite sets.
func versionLabel(clientVersion, serverVersion string) string {
	c := clientversion.MajorMinor(clientVersion)
	if major := semver.Major(c); major != semver.Major(clientversion.MajorMinor(serverVersion)) {
		return major + ".x"
	}
	return c
}
