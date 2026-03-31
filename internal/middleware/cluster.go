package middleware

import (
	"errors"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
)

// Cluster returns a Gin middleware that:
//  1. Extracts the :cluster path parameter.
//  2. Looks up the ClusterClient in the ClusterManager.
//  3. Checks cluster health (returns 503 if unhealthy).
//  4. Builds a per-request scoped Kubernetes client using the credential
//     attached by the Auth middleware.
//  5. Optionally validates the :namespace path parameter against the cluster's
//     AllowedNamespaces list.
//  6. Attaches the scoped client and cluster metadata to the Gin context.
//
// This middleware must be applied after the Auth middleware on all routes that
// include a :cluster path segment.
func Cluster(mgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		clusterName := c.Param("cluster")
		if clusterName == "" {
			// Routes without :cluster don't need this middleware — skip silently.
			c.Next()
			return
		}

		// Resolve the cluster.
		cc, err := mgr.Get(clusterName)
		if err != nil {
			if errors.Is(err, cluster.ErrClusterNotFound) {
				apierrors.Respond(c, apierrors.NewClusterNotFound(clusterName))
			} else {
				apierrors.Respond(c, apierrors.NewInternal(err))
			}
			return
		}

		// Reject requests to unhealthy clusters with 503.
		if !cc.Meta.Healthy {
			apierrors.Respond(c, apierrors.NewClusterUnhealthy(clusterName))
			return
		}

		// Validate namespace restriction (when applicable).
		if ns := c.Param("namespace"); ns != "" {
			if !mgr.IsNamespaceAllowed(cc, ns) {
				apierrors.Respond(c, apierrors.NewNamespaceDenied(ns, clusterName))
				return
			}
		}

		// Retrieve the credential attached by the Auth middleware.
		credVal, exists := c.Get(string(CredentialKey))
		if !exists {
			// Auth middleware must run before Cluster middleware.
			apierrors.Respond(c, apierrors.NewInternal(
				errors.New("cluster middleware: credential not found in context — auth middleware must run first"),
			))
			return
		}
		cred := credVal.(auth.ClusterCredential)

		// Build a per-request scoped Kubernetes client.
		k8sClient, err := mgr.ClientFor(cc, cred)
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		// Build an admin client using the cluster's own SA credentials (no user override).
		// Used for administrative listing endpoints (e.g. namespace discovery) where the
		// user's credentials may lack cluster-wide list permissions.
		adminClient, err := mgr.ClientFor(cc, auth.ClusterCredential{})
		if err != nil {
			apierrors.Respond(c, apierrors.NewInternal(err))
			return
		}

		c.Set(string(K8sClientKey), k8sClient)
		c.Set(string(AdminK8sClientKey), adminClient)
		c.Set(string(ClusterMetaKey), cc.Meta)
		c.Next()
	}
}
