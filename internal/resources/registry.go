// Package resources provides the extensible resource handler registry.
//
// Adding support for a new Kubernetes resource type (CRD or built-in) requires:
//
//  1. Create a new sub-package under internal/resources/<resource>/.
//  2. Implement the ResourceHandler interface.
//  3. Call registry.Register(myHandler) in cmd/server/main.go.
//
// No changes to the server, middleware, or cluster manager are necessary.
package resources

import (
	"github.com/gin-gonic/gin"
)

// ResourceHandler is implemented by every resource type the backend exposes.
// It is responsible for registering its own routes onto the provided RouterGroup.
//
// The RouterGroup passed to RegisterRoutes is already scoped to
// /api/v1/clusters/:cluster so handlers should register paths relative to
// that prefix (e.g. "/namespaces" not "/api/v1/clusters/:cluster/namespaces").
type ResourceHandler interface {
	// Name returns the unique identifier for this handler. It is used in log
	// messages, metric labels, and the resources config feature flags.
	// Must be lowercase, URL-safe, and stable (e.g. "capps", "namespaces").
	Name() string

	// RegisterRoutes registers all HTTP routes for this resource onto rg.
	// Called once at server startup.
	RegisterRoutes(rg *gin.RouterGroup)
}

// Registry holds all registered ResourceHandlers and mounts them onto a
// router group on request. Handlers are registered in the order they are
// added; route conflicts between handlers are detected by Gin at startup.
type Registry struct {
	handlers []ResourceHandler
	enabled  map[string]bool
}

// NewRegistry creates an empty Registry. enabled is a map from handler Name()
// to whether that handler should be registered. Handlers whose name is not
// present in the map are disabled by default; pass an empty map to enable all.
func NewRegistry(enabled map[string]bool) *Registry {
	return &Registry{enabled: enabled}
}

// Register adds a ResourceHandler to the registry. If the handler's name is
// disabled in the enabled map, the handler is silently dropped and its routes
// are never registered.
func (r *Registry) Register(h ResourceHandler) {
	if enabled, ok := r.enabled[h.Name()]; ok && !enabled {
		return // explicitly disabled by config
	}
	r.handlers = append(r.handlers, h)
}

// Mount calls RegisterRoutes on every registered handler, attaching their
// routes to the provided router group.
func (r *Registry) Mount(clusterGroup *gin.RouterGroup) {
	for _, h := range r.handlers {
		h.RegisterRoutes(clusterGroup)
	}
}
