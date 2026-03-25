package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORS returns a Gin middleware that adds the appropriate Access-Control-*
// headers to every response and short-circuits OPTIONS preflight requests with
// a 204.
//
// allowedOrigins is the list of origins that may make cross-origin requests.
// Pass ["*"] to allow all origins (not recommended in production — use the
// actual frontend URL).
func CORS(allowedOrigins []string) gin.HandlerFunc {
	// Build a lookup set for O(1) origin matching.
	originSet := make(map[string]struct{}, len(allowedOrigins))
	wildcard := false
	for _, o := range allowedOrigins {
		if o == "*" {
			wildcard = true
		}
		originSet[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		allowed := wildcard
		if !allowed {
			_, allowed = originSet[origin]
		}

		if allowed && origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		} else if wildcard {
			c.Header("Access-Control-Allow-Origin", "*")
		}

		c.Header("Access-Control-Allow-Methods", strings.Join([]string{
			http.MethodGet, http.MethodPost, http.MethodPut,
			http.MethodDelete, http.MethodOptions,
		}, ", "))
		c.Header("Access-Control-Allow-Headers",
			"Authorization, Content-Type, Accept, X-Request-ID")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
