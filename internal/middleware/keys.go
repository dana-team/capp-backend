// Package middleware provides the Gin middleware stack for capp-backend.
//
// Middleware is applied in this order (outermost first):
//
//  1. Recovery   — catches panics, returns 500
//  2. Logging    — records each request after completion
//  3. Metrics    — records Prometheus counters and histograms after completion
//  4. CORS       — sets Access-Control-* headers; short-circuits OPTIONS requests
//  5. RateLimit  — token-bucket rate limiter per client IP
//  6. Auth       — validates the Authorization header, attaches ClusterCredential
//  7. Cluster    — resolves :cluster path param, builds scoped K8s client
//
// Context keys are defined in keys.go so every middleware and handler can
// reference them without importing the specific middleware package.
package middleware

// Context key type prevents collisions with other packages' string keys.
type contextKey string

const (
	// CredentialKey is the gin.Context key for the auth.ClusterCredential
	// attached by the auth middleware. Handlers must not access credentials
	// directly — they should use the K8s client placed at K8sClientKey.
	CredentialKey contextKey = "credential"

	// K8sClientKey is the gin.Context key for the controller-runtime
	// client.Client attached by the cluster middleware. This is the scoped
	// client that every resource handler uses for cluster API calls.
	K8sClientKey contextKey = "k8sClient"

	// ClusterMetaKey is the gin.Context key for the cluster.ClusterMeta value
	// attached by the cluster middleware. Handlers can use it to include the
	// cluster name in responses.
	ClusterMetaKey contextKey = "clusterMeta"
)
