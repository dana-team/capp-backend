package mcpserver

import (
	"net/http"
	"strings"
)

// AuthMiddleware extracts the bearer token from the Authorization header
// and attaches it to the request context. Note: the HTTP transport is
// stateful, so the token from the "initialize" request applies to the
// entire session.
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerFromHeader(r.Header.Get("Authorization")); ok {
			r = r.WithContext(WithBearerToken(r.Context(), token))
		}
		next.ServeHTTP(w, r)
	})
}

func bearerFromHeader(h string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return token, token != ""
}
