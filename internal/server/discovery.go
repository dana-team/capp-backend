package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/gin-gonic/gin"
)

// listClusters returns visible cluster metadata in the active authorization mode.
func listClusters(mgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		items := mgr.List()
		if policy.Enabled(c) {
			if _, _, ok := policy.RequireDiscovery(c, ""); !ok {
				return
			}
			visible := make([]cluster.ClusterMeta, 0, len(items))
			for _, item := range items {
				states, operator, ok := policy.RequireDiscovery(c, item.Name)
				if !ok {
					return
				}
				if operator || len(states) != 0 {
					visible = append(visible, item)
				}
			}
			slices.SortFunc(visible, func(a, b cluster.ClusterMeta) int { return strings.Compare(a.Name, b.Name) })
			items = visible
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

// getCluster returns metadata for a single cluster by name.
func getCluster(mgr cluster.ClusterManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("cluster")
		for _, m := range mgr.List() {
			if m.Name != name {
				continue
			}
			if policy.Enabled(c) {
				states, operator, ok := policy.RequireDiscovery(c, name)
				if !ok {
					return
				}
				if !operator && len(states) == 0 {
					break
				}
			}
			c.JSON(http.StatusOK, m)
			return
		}
		err := apierrors.NewClusterNotFound(name)
		c.JSON(err.Status, gin.H{"error": err})
	}
}
