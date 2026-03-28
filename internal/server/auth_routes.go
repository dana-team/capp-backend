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
	// jwt mode fields
	Cluster string `json:"cluster"`
	Token   string `json:"token"`
	// dex mode fields
	Username string `json:"username"`
	Password string `json:"password"`
}

// refreshRequest is the body for POST /api/v1/auth/refresh.
type refreshRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// openshiftCallbackRequest is the body for POST /api/v1/auth/openshift/callback.
type openshiftCallbackRequest struct {
	Code        string `json:"code" binding:"required"`
	RedirectURI string `json:"redirectUri" binding:"required"`
}

// openshiftAuthorizeResponse is returned by GET /api/v1/auth/openshift/authorize.
type openshiftAuthorizeResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
}

// registerAuthRoutes mounts the auth endpoints onto the provided group.
// These endpoints do not require the Auth middleware — they ARE the
// authentication entry points.
func registerAuthRoutes(rg *gin.RouterGroup, mgr auth.AuthManager, mode string) {
	rg.POST("/login", loginHandler(mgr, mode))
	rg.POST("/refresh", refreshHandler(mgr))

	if mode == "openshift" {
		rg.GET("/openshift/authorize", openshiftAuthorizeHandler(mgr))
		rg.POST("/openshift/callback", openshiftCallbackHandler(mgr))
	}
}

// loginHandler handles POST /api/v1/auth/login.
// Dispatches to the appropriate login path based on mode.
func loginHandler(mgr auth.AuthManager, mode string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req loginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return
		}

		if mode == "dex" {
			if req.Username == "" || req.Password == "" {
				apierrors.Respond(c, apierrors.NewBadRequest("username and password are required"))
				return
			}
			pair, err := mgr.PasswordLogin(c.Request.Context(), req.Username, req.Password)
			if err != nil {
				switch {
				case errors.Is(err, auth.ErrNotSupported):
					apierrors.Respond(c, apierrors.NewNotSupported("Login"))
				case errors.Is(err, auth.ErrBadCredentials), errors.Is(err, auth.ErrUnauthenticated):
					apierrors.Respond(c, apierrors.NewUnauthorized("invalid credentials"))
				default:
					apierrors.Respond(c, apierrors.NewInternal(err))
				}
				return
			}
			c.JSON(http.StatusOK, pair)
			return
		}

		// jwt/passthrough/static mode path
		if req.Cluster == "" || req.Token == "" {
			apierrors.Respond(c, apierrors.NewBadRequest("cluster and token are required"))
			return
		}
		pair, err := mgr.Login(c.Request.Context(), req.Cluster, req.Token)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrNotSupported):
				apierrors.Respond(c, apierrors.NewNotSupported("Login"))
			case errors.Is(err, auth.ErrUnauthenticated):
				apierrors.Respond(c, apierrors.NewUnauthorized("invalid cluster token"))
			default:
				apierrors.Respond(c, apierrors.NewInternal(err))
			}
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
			if errors.Is(err, auth.ErrBadCredentials) {
				apierrors.Respond(c, apierrors.NewUnauthorized("refresh token rejected by identity provider"))
				return
			}
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, pair)
	}
}

// openshiftAuthorizeHandler handles GET /api/v1/auth/openshift/authorize.
// Returns the OAuth authorize URL for the frontend to redirect the browser to.
func openshiftAuthorizeHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		osMgr, ok := mgr.(auth.OAuthAuthorizer)
		if !ok {
			apierrors.Respond(c, apierrors.NewInternal(errors.New("auth manager does not support OAuth")))
			return
		}

		authorizeURL, err := osMgr.GetAuthorizeURL()
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, openshiftAuthorizeResponse{AuthorizeURL: authorizeURL})
	}
}

// openshiftCallbackHandler handles POST /api/v1/auth/openshift/callback.
// Exchanges an OAuth authorization code for tokens from the OpenShift OAuth server.
func openshiftCallbackHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openshiftCallbackRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return
		}

		osMgr, ok := mgr.(auth.OAuthAuthorizer)
		if !ok {
			apierrors.Respond(c, apierrors.NewInternal(errors.New("auth manager does not support OAuth")))
			return
		}

		pair, err := osMgr.OAuthExchange(c.Request.Context(), req.Code, req.RedirectURI)
		if err != nil {
			if errors.Is(err, auth.ErrBadCredentials) {
				apierrors.Respond(c, apierrors.NewUnauthorized("OAuth authentication failed"))
				return
			}
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, pair)
	}
}
