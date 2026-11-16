package pools

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for cfunc pools.
type Handler struct {
	catalog *Catalog
}

// New returns a ready-to-use pool Handler that resolves flavors and sizes
// from cfg. It fails when a configured size does not parse, which
// config.Validate already rejects, so this is a startup-time error.
func New(cfg config.CfuncConfig) (*Handler, error) {
	catalog, err := NewCatalog(cfg)
	if err != nil {
		return nil, err
	}
	return &Handler{catalog: catalog}, nil
}

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "cfuncpools" }

// RegisterRoutes attaches all pool routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Cluster-scoped list (all namespaces).
	rg.GET("/cfunc/pools", h.listAll)

	// Namespace-scoped operations.
	ns := rg.Group("/namespaces/:namespace/cfunc/pools")
	ns.GET("", h.list)
	ns.POST("", h.create)
	ns.GET("/:name", h.get)
	ns.PUT("/:name", h.update)
	ns.DELETE("/:name", h.delete)
}

// listAll handles GET /api/v1/clusters/:cluster/cfunc/pools
// Lists pools across all namespaces visible to the caller.
func (h *Handler) listAll(c *gin.Context) {
	var envList fissionv1.EnvironmentList
	if !namespaced.ListAll(c, &envList) {
		return
	}

	h.respondList(c, envList)
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var envList fissionv1.EnvironmentList
	if err := k8sClient.List(c.Request.Context(), &envList, client.InNamespace(namespace)); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.respondList(c, envList)
}

// get handles GET /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	var env fissionv1.Environment
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &env); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Pool", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, FromK8s(&env, h.catalog))
}

// create handles POST /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools
func (h *Handler) create(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var req PoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	env, err := ToK8s(req, nil, namespace, h.catalog)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}
	if err := k8sClient.Create(c.Request.Context(), env); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, FromK8s(env, h.catalog))
}

// update handles PUT /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
// It performs a full replacement of the request fields (not a patch). The
// resource version is read from the live object before writing so we don't
// clobber concurrent changes.
func (h *Handler) update(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var req PoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	// Read the current live object to get its resourceVersion. This is
	// required by Kubernetes optimistic concurrency control.
	var existing fissionv1.Environment
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Pool", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	// Fission ignores the pool size below this version and makes the version
	// immutable, so an update could only report values that have no effect.
	if existing.Spec.Version < environmentVersion {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf(
			"pool %s is a version %d Environment that was not created through this API; Fission ignores the pool size below version %d and does not let the version change, so delete it and create it again",
			name, existing.Spec.Version, environmentVersion)))
		return
	}

	// The URL param is authoritative. It is set before the conversion because
	// ToK8s names the runtime container after the pool.
	req.Name = name
	updated, err := ToK8s(req, &existing, namespace, h.catalog)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	if err := k8sClient.Update(ctx, updated); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, FromK8s(updated, h.catalog))
}

// delete handles DELETE /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/pools/:name
func (h *Handler) delete(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	ctx := c.Request.Context()

	var env fissionv1.Environment
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &env); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Pool", name))
			return
		}
		apierrors.Respond(c, err)
		return
	}

	// The Kubernetes API deletes an Environment that functions still use and
	// leaves them broken; only the fission CLI checks.
	//
	// The check is partial: a Function in another namespace can name this
	// Environment and is not seen, because the caller's client lists only
	// this namespace. A cluster-wide list would need a new permission and
	// would put the Function names of other namespaces in the error.
	var functions fissionv1.FunctionList
	if err := k8sClient.List(ctx, &functions, client.InNamespace(namespace)); err != nil {
		apierrors.Respond(c, err)
		return
	}
	var users []string
	for _, fn := range functions.Items {
		ref := fn.Spec.Environment
		if ref.Name == name && (ref.Namespace == "" || ref.Namespace == namespace) {
			users = append(users, fn.Name)
		}
	}
	if len(users) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("pool %s is still in use by functions: %s", name, strings.Join(users, ", "))))
		return
	}

	if err := k8sClient.Delete(ctx, &env); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// respondList converts an EnvironmentList into a PoolListResponse and writes it.
func (h *Handler) respondList(c *gin.Context, envList fissionv1.EnvironmentList) {
	items := make([]PoolResponse, 0, len(envList.Items))
	for i := range envList.Items {
		items = append(items, FromK8s(&envList.Items[i], h.catalog))
	}
	c.JSON(http.StatusOK, PoolListResponse{Items: items, Total: len(items)})
}
