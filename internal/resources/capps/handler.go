package capps

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for the rcs.dana.io/v1alpha1
// Capp custom resource. It provides full CRUD over Capps through the
// per-request scoped Kubernetes client injected by the cluster middleware.
type Handler struct{}

// New returns a ready-to-use Capp Handler.
func New() *Handler { return &Handler{} }

// Name returns the handler's identifier, matching the resources.capps config key.
func (h *Handler) Name() string { return "capps" }

// RegisterRoutes attaches all Capp routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Cluster-scoped list (all namespaces).
	rg.GET("/capps", h.listAll)

	// Namespace-scoped operations.
	ns := rg.Group("/namespaces/:namespace/capps")
	ns.GET("", h.list)
	ns.POST("", h.create)
	ns.GET("/:name", h.get)
	ns.PUT("/:name", h.update)
	ns.DELETE("/:name", h.delete)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// listAll handles GET /api/v1/clusters/:cluster/capps
// Lists Capps across all namespaces visible to the caller.
func (h *Handler) listAll(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}

	var cappList cappv1alpha1.CappList
	if err := k8sClient.List(c.Request.Context(), &cappList); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, cappList)
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps
func (h *Handler) list(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var cappList cappv1alpha1.CappList
	if err := k8sClient.List(c.Request.Context(), &cappList, client.InNamespace(namespace)); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, cappList)
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
func (h *Handler) get(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, FromK8s(&capp))
}

// create handles POST /api/v1/clusters/:cluster/namespaces/:namespace/capps
func (h *Handler) create(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var req CappRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	// Override namespace from URL — the URL is authoritative.
	req.Namespace = namespace

	capp := ToK8s(req, namespace)
	if err := k8sClient.Create(c.Request.Context(), capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, FromK8s(capp))
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
// It performs a full replacement (not a patch). The resource version is read
// from the live object before writing so we don't clobber concurrent changes.
func (h *Handler) update(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var req CappRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	// Read the current live object to get its resourceVersion. This is
	// required by Kubernetes optimistic concurrency control.
	var existing cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	updated := ToK8s(req, namespace)
	// Preserve the resource version from the live object to satisfy the
	// Kubernetes API server's optimistic concurrency check.
	updated.ResourceVersion = existing.ResourceVersion
	updated.Name = name // URL param is authoritative

	if err := k8sClient.Update(c.Request.Context(), updated); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, FromK8s(updated))
}

// delete handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/capps/:name
func (h *Handler) delete(c *gin.Context) {
	k8sClient := extractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	if err := k8sClient.Delete(c.Request.Context(), &capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// respondList converts a CappList into a CappListResponse and writes it.
func (h *Handler) respondList(c *gin.Context, cappList cappv1alpha1.CappList) {
	items := make([]CappResponse, 0, len(cappList.Items))
	for i := range cappList.Items {
		items = append(items, FromK8s(&cappList.Items[i]))
	}
	c.JSON(http.StatusOK, CappListResponse{Items: items, Total: len(items)})
}

// extractClient retrieves the scoped K8s client from the Gin context.
// It responds with an internal error and returns nil if the client is absent
// (which indicates a middleware configuration error).
func extractClient(c *gin.Context) client.Client {
	val, exists := c.Get(string(middleware.K8sClientKey))
	if !exists {
		apierrors.Respond(c, apierrors.NewInternal(
			errors.New("K8sClientKey not set in context — cluster middleware must run before handler"),
		))
		return nil
	}
	k, ok := val.(client.Client)
	if !ok {
		apierrors.Respond(c, apierrors.NewInternal(
			errors.New("K8sClientKey has unexpected type in context"),
		))
		return nil
	}
	return k
}
