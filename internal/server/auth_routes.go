package server

import (
	"errors"
	"net/http"
	"net/url"

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
	Code  string `json:"code" binding:"required"`
	State string `json:"state" binding:"required"`
	// RedirectURI may be provided by the CLI to complete its local-server OAuth
	// flow. It is validated to be a localhost URI before use, preventing the
	// backend from acting as a generic code exchanger for arbitrary redirect URIs.
	// If empty, the server's configured auth.openshift.redirectUri is used.
	RedirectURI string `json:"redirectUri"`
}

// openshiftAuthorizeResponse is returned by GET /api/v1/auth/openshift/authorize.
type openshiftAuthorizeResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
	State        string `json:"state"`
}

// authModeResponse is returned by GET /api/v1/auth/mode.
type authModeResponse struct {
	Mode string `json:"mode"`
}

// registerAuthRoutes mounts the auth endpoints onto the provided group.
// These endpoints do not require the Auth middleware — they ARE the
// authentication entry points.
func registerAuthRoutes(rg *gin.RouterGroup, mgr auth.AuthManager, mode string) {
	rg.GET("/mode", func(c *gin.Context) {
		c.JSON(200, authModeResponse{Mode: mode})
	})
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

		// dex and openshift both support username/password login.
		if mode == "dex" || (mode == "openshift" && req.Username != "" && req.Password != "") {
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
// Returns the OAuth authorize URL for the frontend or CLI to redirect the browser to.
// An optional ?redirect_uri= query param may be provided by the CLI; it must be a
// localhost URI (validated here) so the browser callback lands on the CLI's local server.
func openshiftAuthorizeHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		osMgr, ok := mgr.(auth.OAuthAuthorizer)
		if !ok {
			apierrors.Respond(c, apierrors.NewInternal(errors.New("auth manager does not support OAuth")))
			return
		}

		redirectURI := c.Query("redirect_uri")
		if redirectURI != "" && !isLocalhostURI(redirectURI) {
			apierrors.Respond(c, apierrors.NewBadRequest("redirect_uri must be a localhost URI"))
			return
		}

		authorizeURL, state, err := osMgr.GetAuthorizeURL(redirectURI)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.JSON(http.StatusOK, openshiftAuthorizeResponse{AuthorizeURL: authorizeURL, State: state})
	}
}

// openshiftCallbackHandler handles POST /api/v1/auth/openshift/callback.
// Exchanges an OAuth authorization code for tokens from the OpenShift OAuth server.
// An optional redirectUri body field may be provided by the CLI; it must be a
// localhost URI and must match the one used to build the authorize URL.
func openshiftCallbackHandler(mgr auth.AuthManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openshiftCallbackRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
			return
		}

		if req.RedirectURI != "" && !isLocalhostURI(req.RedirectURI) {
			apierrors.Respond(c, apierrors.NewBadRequest("redirectUri must be a localhost URI"))
			return
		}

		osMgr, ok := mgr.(auth.OAuthAuthorizer)
		if !ok {
			apierrors.Respond(c, apierrors.NewInternal(errors.New("auth manager does not support OAuth")))
			return
		}

		if err := osMgr.ValidateState(req.State); err != nil {
			apierrors.Respond(c, apierrors.NewUnauthorized("invalid or expired OAuth state parameter"))
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

// isLocalhostURI reports whether s is a valid http://localhost (or 127.0.0.1/::1) URI.
// Used to validate client-supplied redirect URIs so the backend cannot be used as a
// generic OAuth code exchanger for arbitrary redirect targets.
func isLocalhostURI(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
