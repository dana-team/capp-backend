package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/gin-gonic/gin"
)

// StatusClientClosedRequest records a canceled request without counting it as a server error.
const StatusClientClosedRequest = 499

// Authenticate validates the request, stores its credential, and reports errors.
// On failure it aborts the request and returns false.
func Authenticate(c *gin.Context, mgr auth.AuthManager) (auth.ClusterCredential, bool) {
	if AbortIfCanceled(c) {
		return auth.ClusterCredential{}, false
	}
	// Cluster-list routes omit the :cluster parameter.
	clusterName := c.Param("cluster")

	cred, err := mgr.Authenticate(c.Request.Context(), clusterName, c.Request)
	if AbortIfCanceled(c) {
		return auth.ClusterCredential{}, false
	}
	if err != nil {
		if diagnostic := auth.TokenReviewDiagnostic(err); diagnostic != "" {
			_ = c.Error(errors.New(diagnostic))
		}
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			apierrors.Respond(c, apierrors.NewUnauthorized(auth.ErrUnauthenticated.Error()))
		case errors.Is(err, auth.ErrTokenExpired):
			apierrors.Respond(c, apierrors.NewUnauthorized(auth.ErrTokenExpired.Error()))
		case errors.Is(err, auth.ErrDirectoryUnavailable):
			apierrors.Respond(c, &apierrors.APIError{
				Code: apierrors.CodeInternal, Message: auth.ErrDirectoryUnavailable.Error(), Status: http.StatusServiceUnavailable,
			})
		case errors.Is(err, auth.ErrIdentityUnavailable):
			apierrors.Respond(c, &apierrors.APIError{
				Code: apierrors.CodeInternal, Message: auth.ErrIdentityUnavailable.Error(), Status: http.StatusServiceUnavailable,
			})
		default:
			apierrors.Respond(c, apierrors.NewInternal(err))
		}
		return auth.ClusterCredential{}, false
	}

	c.Set(string(CredentialKey), cred)
	return cred, true
}

// AbortIfCanceled stops a canceled request before a response is written.
// It records status 499 without an error body; deadlines retain their existing handling.
func AbortIfCanceled(c *gin.Context) bool {
	if !errors.Is(c.Request.Context().Err(), context.Canceled) {
		return false
	}
	c.AbortWithStatus(StatusClientClosedRequest)
	return true
}
