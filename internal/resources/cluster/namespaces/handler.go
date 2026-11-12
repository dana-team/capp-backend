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
// In Kubernetes authorization mode, OpenShift discovery lists project.openshift.io/v1
// Projects using the user-scoped client, returning only accessible projects.
// On vanilla Kubernetes, it uses the admin client to list
// all CAPP-managed namespaces and then filters them with a SelfSubjectAccessReview
// for each namespace to return only those the user can create Capps in.
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

// New returns a ready-to-use namespace Handler.
func New(logger *zap.Logger) *Handler { return &Handler{logger: logger} }

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
	rg.GET("/namespaces/:namespace", h.get)
	rg.POST("/namespaces", h.create)
	rg.PUT("/namespaces/:namespace", h.update)
	rg.PATCH("/namespaces/:namespace", h.patch)
	rg.DELETE("/namespaces/:namespace", h.delete)
}

// list handles GET /api/v1/clusters/:cluster/namespaces.
func (h *Handler) list(c *gin.Context) {
	if policy.Enabled(c) {
		operator, _, allowed := policy.RequireNamespaceAccess(c)
		if !allowed {
			return
		}
		if operator {
			h.listManagedNamespaces(c)
			return
		}
		h.listAuthorizedNamespaces(c)
		return
	}
	h.listKubernetes(c)
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace.
func (h *Handler) get(c *gin.Context) {
	if policy.Enabled(c) {
		h.getManagedNamespace(c)
		return
	}
	h.getKubernetes(c)
}

func (h *Handler) create(c *gin.Context) {
	if policy.Enabled(c) {
		h.createManagedNamespace(c)
		return
	}
	h.createKubernetes(c)
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:namespace.
// Applies optional quota and membership changes using the configured authorization mode.
func (h *Handler) update(c *gin.Context) {
	if policy.Enabled(c) {
		h.updateManagedNamespace(c)
		return
	}
	h.updateKubernetes(c)
}

// patch handles PATCH /api/v1/clusters/:cluster/namespaces/:namespace.
// Adds membership using the configured authorization mode.
// To update the resource quota, use the PUT endpoint with the full quota spec.
//
// Kubernetes mode permits Capp creators in the target namespace to add users;
// backend-policy mode requires a configured operator.
func (h *Handler) patch(c *gin.Context) {
	if policy.Enabled(c) {
		h.updateManagedNamespace(c)
		return
	}
	h.patchKubernetes(c)
}

// delete handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace.
// Rejects deletion when the namespace still contains Capps (409 Conflict).
func (h *Handler) delete(c *gin.Context) {
	if policy.Enabled(c) {
		h.deleteManagedNamespace(c)
		return
	}
	h.deleteKubernetes(c)
}
