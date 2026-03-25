package server

import (
	"errors"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/gin-gonic/gin"
)

// loginRequest is the body for POST /api/v1/auth/login.
type loginRequest struct {
	// Cluster is the name of the cluster to authenticate against.
	Cluster string `json:"cluster" binding:"required"`

	// Token is the raw Kubernetes bearer token to validate.
	Token string `json:"token" binding:"required"`
}

// refreshRequest is the body for POST /api/v1/auth/refresh.
type refreshRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// registerAuthRoutes mounts the auth endpoints onto the provided group.
// These endpoints do not require the Auth middleware — they ARE the
// authentication entry points.
func registerAuthRoutes(rg *gin.RouterGroup, mgr auth.AuthManager) {
	rg.POST("/login", loginHandler(mgr))
	rg.POST("/refresh", refreshHandler(mgr))
}

// loginHandler handles POST /api/v1/auth/login.
// In jwt mode: validates the token against the cluster and returns a TokenPair.
// In passthrough / static mode: returns 501 Not Implemented.
func loginHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req loginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return
		}

		pair, err := mgr.Login(c.Request.Context(), req.Cluster, req.Token)
		if err != nil {
			if errors.Is(err, auth.ErrNotSupported) {
				apierrors.Respond(c, apierrors.NewNotSupported("Login"))
				return
			}
			if errors.Is(err, auth.ErrUnauthenticated) {
				apierrors.Respond(c, apierrors.NewUnauthorized("invalid cluster token"))
				return
			}
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, pair)
	}
}

// refreshHandler handles POST /api/v1/auth/refresh.
func refreshHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req refreshRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return
		}

		pair, err := mgr.Refresh(c.Request.Context(), req.RefreshToken)
		if err != nil {
			if errors.Is(err, auth.ErrNotSupported) {
				apierrors.Respond(c, apierrors.NewNotSupported("Refresh"))
				return
			}
			if errors.Is(err, auth.ErrTokenExpired) || errors.Is(err, auth.ErrInvalidToken) {
				apierrors.Respond(c, apierrors.NewUnauthorized("refresh token is invalid or expired"))
				return
			}
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, pair)
	}
}
