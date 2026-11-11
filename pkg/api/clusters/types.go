// Package clusters holds the cluster DTOs of the REST API.
//
// It imports nothing, so clients (cappctl, capp-mcp) can share these types
// without pulling in internal/cluster and its Kubernetes dependencies.
package clusters

// ClusterMeta holds identifying and health metadata for one managed cluster.
// It is a pure value type safe to copy and serialize. The Healthy field is
// populated from the atomic source of truth in ClusterClient when building
// snapshots (e.g. List).
type ClusterMeta struct {
	// Name is the unique identifier used in all API paths.
	Name string `json:"name"`

	// DisplayName is the human-readable label returned to the frontend.
	DisplayName string `json:"displayName"`

	// Healthy reflects the result of the most recent background health check.
	// False does NOT mean the cluster is permanently down — the next check may
	// restore it. Handlers should return 503 (not 404) for unhealthy clusters.
	Healthy bool `json:"healthy"`

	// IsOpenShift is true when the cluster exposes the OpenShift API groups.
	// Set at startup by probing the route.openshift.io API group.
	IsOpenShift bool `json:"isOpenShift"`

	// GitOpsPath is the directory name used in the GitOps repository path:
	// <pathPrefix>/<gitOpsPath>/<namespace>/<cappName>.yaml
	GitOpsPath string `json:"gitOpsPath"`

	// AllowedNamespaces is a copy of the config value. If non-empty, only the
	// listed namespaces are accessible through this cluster entry.
	AllowedNamespaces []string `json:"-"`
}
