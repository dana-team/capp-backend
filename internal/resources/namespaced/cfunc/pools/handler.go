package pools

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for cfunc pools.
type Handler struct {
	catalog *Catalog
	onboard bool
}

// New returns a ready-to-use pool Handler that resolves flavors and sizes
// from cfg. It fails when a configured size does not parse, which
// config.Validate already rejects, so this is a startup-time error.
func New(cfg config.CfuncConfig) (*Handler, error) {
	catalog, err := NewCatalog(cfg)
	if err != nil {
		return nil, err
	}
	return &Handler{catalog: catalog, onboard: cfg.OnboardNamespaces}, nil
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
	// Before the label, which stays when the create is refused.
	if err := h.checkTrustedCA(c.Request.Context(), k8sClient, namespace); err != nil {
		apierrors.Respond(c, err)
		return
	}
	// The label comes first: a pool in a namespace Fission does not serve
	// gets no pods, and a repeated request finds the label already there.
	if h.onboard {
		if err := onboardNamespace(c, k8sClient, namespace); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}
	if err := k8sClient.Create(c.Request.Context(), env); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, FromK8s(env, h.catalog))
}

// onboardNamespace sets the label that makes Fission, in its dynamic tenancy
// mode, serve the namespace. The caller's credential is tried first. A
// namespace's users normally cannot change the namespace object itself, so
// a refusal falls back to the backend's own cluster credential when the
// request has one (passthrough mode has none) and the caller may create
// pools in the namespace. The label is never removed
// here: removing it makes Fission tear down what it runs in the namespace.
func onboardNamespace(c *gin.Context, userClient client.Client, namespace string) error {
	err := labelNamespace(c, userClient, namespace)
	if !k8serrors.IsForbidden(err) {
		return err
	}
	// The backend credential acts only for a caller who may create the pool.
	// Without this check, a caller with no right in the namespace would get
	// it labelled, and the label stays, before its create is refused.
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Group:     fissionv1.SchemeGroupVersion.Group,
				Resource:  "environments",
			},
		},
	}
	if reviewErr := userClient.Create(c.Request.Context(), review); reviewErr != nil {
		return reviewErr
	}
	if !review.Status.Allowed {
		return apierrors.NewForbidden(fmt.Sprintf("you may not create pools in namespace %s", namespace))
	}
	admin, ok := c.Get(string(middleware.AdminK8sClientKey))
	if !ok {
		return err
	}
	adminClient, ok := admin.(client.Client)
	if !ok || adminClient == nil {
		return err
	}
	adminErr := labelNamespace(c, adminClient, namespace)
	if k8serrors.IsForbidden(adminErr) {
		return apierrors.NewForbidden(fmt.Sprintf(
			"namespace %s is not enabled for Fission, and neither the caller nor the backend may label it; ask a cluster administrator to set %s=%s on it",
			namespace, FissionEnabledLabel, FissionEnabledValue))
	}
	// Any other failure, such as a timeout or a conflict, is not a missing
	// right: it goes out as it is, so the cause is logged and the caller is
	// not sent to an administrator for nothing.
	return adminErr
}

// checkTrustedCA refuses a pool in a namespace that lacks the trusted CA
// ConfigMap or holds it with no data. Every pod of the pool mounts it: a
// missing one keeps the pods in ContainerCreating while the pool itself
// reports nothing, and an empty one leaves the variables that point into it
// naming no file, which breaks every TLS call of a runtime that reads one.
//
// A caller who may not read ConfigMaps is let through, because the kubelet
// mounts the ConfigMap with its own credential, not the caller's.
func (h *Handler) checkTrustedCA(ctx context.Context, k8sClient client.Client, namespace string) error {
	name := h.catalog.cfg.TrustedCA.ConfigMap
	if name == "" {
		return nil
	}
	var configMap corev1.ConfigMap
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &configMap)
	switch {
	case k8serrors.IsNotFound(err):
		return apierrors.NewConflict(fmt.Sprintf(
			"namespace %s has no ConfigMap %s, the trusted CA bundle that every pool pod mounts; ask a cluster administrator to provide it",
			namespace, name))
	case k8serrors.IsForbidden(err):
		return nil
	case err != nil:
		return err
	case len(configMap.Data) == 0 && len(configMap.BinaryData) == 0:
		return apierrors.NewConflict(fmt.Sprintf(
			"ConfigMap %s in namespace %s is empty; it must hold the trusted CA bundle that every pool pod mounts, so ask a cluster administrator to fill it",
			name, namespace))
	}
	return nil
}

func labelNamespace(c *gin.Context, k8sClient client.Client, namespace string) error {
	var ns corev1.Namespace
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Name: namespace}, &ns); err != nil {
		return err
	}
	if ns.Labels[FissionEnabledLabel] == FissionEnabledValue {
		return nil
	}
	patch := client.MergeFrom(ns.DeepCopy())
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels[FissionEnabledLabel] = FissionEnabledValue
	return k8sClient.Patch(c.Request.Context(), &ns, patch)
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
	if existing.Spec.Version < EnvironmentVersion {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf(
			"pool %s is a version %d Environment that was not created through this API; Fission ignores the pool size below version %d and does not let the version change, so delete it and create it again",
			name, existing.Spec.Version, EnvironmentVersion)))
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
	// Also for a pool that mounts the bundle already: an update can replace
	// its pods, and new pods cannot start without the ConfigMap.
	if err := h.checkTrustedCA(ctx, k8sClient, namespace); err != nil {
		apierrors.Respond(c, err)
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
