package middleware

import (
	"errors"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/gin-gonic/gin"
)

// Auth returns a Gin middleware that validates the incoming request using the
// provided AuthManager and attaches the resulting ClusterCredential to the
// Gin context under CredentialKey.
//
// The cluster name is extracted from the :cluster URL parameter. If the auth
// middleware is applied on routes without a :cluster parameter (e.g. the
// /api/v1/clusters listing endpoint), pass an empty string as the cluster name
// to the AuthManager — all implementations handle this gracefully.
//
// On failure the middleware calls Respond and aborts the chain; no downstream
// handler is invoked.
func Auth(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// The cluster name may or may not be in the path at this point.
		// Auth is applied globally so we extract it optimistically.
		clusterName := c.Param("cluster")

		cred, err := mgr.Authenticate(c.Request.Context(), clusterName, c.Request)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrTokenExpired) {
				apierrors.Respond(c, apierrors.NewUnauthorized(err.Error()))
				return
			}
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.Set(string(CredentialKey), cred)
		c.Next()
	}
}
