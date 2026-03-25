// Package namespaces implements the namespace listing resource handler.
//
// It exposes a single endpoint:
//
//	GET /api/v1/clusters/:cluster/namespaces
//
// The frontend uses this endpoint to populate the namespace dropdown when
// the user selects a cluster. The response is a simplified list — not the
// full Kubernetes Namespace object — so the frontend does not need to
// understand Kubernetes API conventions.
package namespaces

import (
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespaceItem is the simplified namespace representation returned to the frontend.
type NamespaceItem struct {
	// Name is the Kubernetes namespace name.
	Name string `json:"name"`

	// Status is the namespace phase: "Active" or "Terminating".
	Status string `json:"status"`
}

// NamespaceListResponse is the response envelope for the list endpoint.
type NamespaceListResponse struct {
	Items []NamespaceItem `json:"items"`
}

// Handler implements resources.ResourceHandler for Kubernetes Namespaces.
type Handler struct{}

// New returns a ready-to-use namespace Handler.
func New() *Handler { return &Handler{} }

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "namespaces" }

// RegisterRoutes attaches the namespace routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/namespaces", h.list)
}

// list handles GET /api/v1/clusters/:cluster/namespaces.
// It lists all Kubernetes Namespaces visible to the caller's credentials and
// returns them as a simplified JSON array.
func (h *Handler) list(c *gin.Context) {
	k8sClient, ok := c.MustGet(string(middleware.K8sClientKey)).(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(
			errContextMissing("K8sClientKey"),
		))
		return
	}

	var nsList corev1.NamespaceList
	if err := k8sClient.List(c.Request.Context(), &nsList); err != nil {
		apierrors.Respond(c, err)
		return
	}

	items := make([]NamespaceItem, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		items = append(items, NamespaceItem{
			Name:   ns.Name,
			Status: string(ns.Status.Phase),
		})
	}

	c.JSON(http.StatusOK, NamespaceListResponse{Items: items})
}

// errContextMissing is a helper for producing a consistent internal error when
// a required context value is absent (indicates middleware misconfiguration).
func errContextMissing(key string) error {
	return &contextError{key: key}
}

type contextError struct{ key string }

func (e *contextError) Error() string {
	return "required context value " + e.key + " not set — check middleware order"
}
