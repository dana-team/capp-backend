package server

import (
	"slices"

	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
)

// clusterVisible checks cluster permission before requiring namespace metadata.
func clusterVisible(c *gin.Context, meta cluster.ClusterMeta) (bool, error) {
	value, _ := c.Get(string(middleware.AuthorizationKey))
	state, ok := value.(*middleware.AuthorizationState)
	if !ok || state == nil {
		return false, authorization.ErrPolicyUnavailable
	}
	target := state.Targets[meta.Name]
	if target == nil || target.Snapshot == nil {
		return false, authorization.ErrPolicyUnavailable
	}
	allowed, err := target.Snapshot.CheckCluster(authorization.ResourceClusters, authorization.ActionGet)
	state.RecordDecision(meta.Name, "", authorization.ResourceClusters, authorization.ActionGet, allowed, err)
	if err != nil || allowed {
		return allowed, err
	}
	namespaces, err := target.Snapshot.Namespaces()
	if err != nil {
		return false, err
	}
	for i := range namespaces {
		namespace := &namespaces[i]
		if namespace.Name == target.Config.PolicyNamespace || slices.Contains(target.Config.ProtectedNamespaces, namespace.Name) ||
			(len(meta.AllowedNamespaces) > 0 && !slices.Contains(meta.AllowedNamespaces, namespace.Name)) {
			continue
		}
		granted, err := target.Snapshot.HasNamespaceAccess(namespace)
		if err != nil {
			return false, err
		}
		allowed = allowed || granted
	}
	// Empty and excluded collections still depend on both captured generations.
	if _, err := target.Snapshot.CheckCluster(authorization.ResourceClusters, authorization.ActionGet); err != nil {
		return false, err
	}
	if _, err := target.Snapshot.NamespaceResourceVersion(); err != nil {
		return false, err
	}
	return allowed, nil
}
