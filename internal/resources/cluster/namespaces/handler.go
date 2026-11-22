// Package namespaces implements namespace resource handlers.
//
// Endpoints:
//
//	GET    /api/v1/clusters/:cluster/namespaces          — list CAPP-managed namespaces
//	GET	   /api/v1/clusters/:cluster/namespaces/:namespace    — get namespace details
//	POST   /api/v1/clusters/:cluster/namespaces          — create namespace with optional quota and membership
//	PUT    /api/v1/clusters/:cluster/namespaces/:namespace    — update quota and membership
//	PATCH  /api/v1/clusters/:cluster/namespaces/:namespace    — add namespace membership
//	DELETE /api/v1/clusters/:cluster/namespaces/:namespace    — delete empty namespace
//
// Namespace discovery and membership use backend policy assignments.
package namespaces

import (
	"maps"

	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Handler implements resources.ResourceHandler for Kubernetes Namespaces.
type Handler struct {
	logger         *zap.Logger
	administration map[string]*Administration
}

// NewWithAdministration injects cluster-specific policy storage into the existing
// namespace handlers. The global policy chain must authorize every operation.
func NewWithAdministration(logger *zap.Logger, targets map[string]*Administration) *Handler {
	return &Handler{logger: logger, administration: maps.Clone(targets)}
}

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "namespaces" }

// RegisterRoutes attaches the namespace routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/namespaces", h.list)
	rg.GET("/namespaces/:namespace", h.getManagedNamespace)
	rg.POST("/namespaces", h.createManagedNamespace)
	rg.PUT("/namespaces/:namespace", h.updateManagedNamespace)
	rg.PATCH("/namespaces/:namespace", h.updateManagedNamespace)
	rg.DELETE("/namespaces/:namespace", h.deleteManagedNamespace)
}

// list handles GET /api/v1/clusters/:cluster/namespaces.
func (h *Handler) list(c *gin.Context) {
	operator, _, allowed := policy.RequireNamespaceAccess(c)
	if !allowed {
		return
	}
	if operator {
		h.listManagedNamespaces(c)
		return
	}
	h.listAuthorizedNamespaces(c)
}
