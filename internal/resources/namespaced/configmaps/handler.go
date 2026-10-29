// Package configmaps implements the config map resource handler.
//
// The frontend uses this handler to perform CRUD operations on  config maps in the selected namespace. The
// response is a simplified list it will only display config maps with the label dana.io/capp-managed=true.
// All config map operations are namespace-scoped and require the user to have permissions in the target namespace.
package configmaps

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for Kubernetes ConfigMaps.
type Handler struct{}

// New returns a ready-to-use config map Handler.
func New() *Handler { return &Handler{} }

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "configmaps" }

// RegisterRoutes attaches the config map routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// cluster-scoped list (all namespaces)
	rg.GET("/configmaps", h.listAll)

	// Namespace-scoped operations.
	cm := rg.Group("/namespaces/:namespace/configmaps")
	cm.GET("", h.list)
	cm.POST("", h.create)
	cm.GET("/names", h.listNames)
	cm.GET("/:name", h.get)
	cm.PUT("/:name", h.update)
	cm.DELETE("/:name", h.delete)
}

func (h *Handler) listAll(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	configMapList := &corev1.ConfigMapList{}
	if err := k8sClient.List(c.Request.Context(), configMapList, client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	value, exists := c.Get(string(middleware.AuthorizationKey))
	if !exists {
		c.JSON(http.StatusOK, convertToResponseList(configMapList.Items))
		return
	}
	state, ok := value.(*middleware.AuthorizationState)
	if !ok || state == nil {
		middleware.RespondAuthorization(c, authorization.ErrPolicyUnavailable)
		return
	}
	clusterName := c.Param("cluster")
	target := state.Targets[clusterName]
	value, _ = c.Get(string(middleware.ClusterMetaKey))
	meta, ok := value.(cluster.ClusterMeta)
	if target == nil || target.Snapshot == nil || !ok {
		middleware.RespondAuthorization(c, authorization.ErrPolicyUnavailable)
		return
	}
	// Validate both collections even when no item needs a namespace check.
	validate := func() error {
		if _, err := target.Snapshot.CheckAllNamespaces(authorization.ResourceConfigMaps, authorization.ActionList); err != nil {
			return err
		}
		_, err := target.Snapshot.NamespaceResourceVersion()
		return err
	}
	if err := validate(); err != nil {
		middleware.RespondAuthorization(c, err)
		return
	}
	items := configMapList.Items[:0]
	for _, item := range configMapList.Items {
		if item.Namespace == target.Config.PolicyNamespace || slices.Contains(target.Config.ProtectedNamespaces, item.Namespace) ||
			(len(meta.AllowedNamespaces) > 0 && !slices.Contains(meta.AllowedNamespaces, item.Namespace)) {
			continue
		}
		namespace, err := target.Snapshot.Namespace(item.Namespace)
		if k8serrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			middleware.RespondAuthorization(c, err)
			return
		}
		allowed, err := target.Snapshot.CheckNamespace(namespace, authorization.ResourceConfigMaps, authorization.ActionList)
		state.RecordDecision(clusterName, item.Namespace, authorization.ResourceConfigMaps, authorization.ActionList, allowed, err)
		if err != nil {
			middleware.RespondAuthorization(c, err)
			return
		}
		if allowed {
			items = append(items, item)
		}
	}
	if err := validate(); err != nil {
		middleware.RespondAuthorization(c, err)
		return
	}
	configMapList.Items = items

	c.JSON(http.StatusOK, convertToResponseList(configMapList.Items))
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/configmaps.
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	configMapList := &corev1.ConfigMapList{}
	if err := k8sClient.List(c.Request.Context(), configMapList, client.InNamespace(namespace), client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, convertToResponseList(configMapList.Items))
}

// listNames handles GET /api/v1/clusters/:cluster/namespaces/:namespace/configmaps/names.
func (h *Handler) listNames(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	configMapList := &corev1.ConfigMapList{}
	if err := k8sClient.List(c.Request.Context(), configMapList, client.InNamespace(namespace), client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	names := make([]string, 0, len(configMapList.Items))
	for _, cm := range configMapList.Items {
		names = append(names, cm.Name)
	}
	c.JSON(http.StatusOK, ConfigMapNameListResponse{Items: names, Total: len(names)})
}

func (h *Handler) create(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")

	var req ConfigMapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
			Labels: map[string]string{
				consts.ManagedLabelKey: consts.ManagedLabelValue,
			},
		},
		Data: req.Data,
	}

	if err := k8sClient.Create(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusCreated, ConfigMapResponse{
		Name:            configMap.Name,
		Namespace:       configMap.Namespace,
		Data:            configMap.Data,
		CreatedAt:       configMap.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(configMap.UID),
		ResourceVersion: configMap.ResourceVersion,
		Labels:          configMap.Labels,
		Annotations:     configMap.Annotations,
	})
}

func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	configMap := &corev1.ConfigMap{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	if value, ok := configMap.Labels[consts.ManagedLabelKey]; !ok || value != consts.ManagedLabelValue {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}
	c.JSON(http.StatusOK, ConfigMapResponse{
		Name:            configMap.Name,
		Namespace:       configMap.Namespace,
		Data:            configMap.Data,
		CreatedAt:       configMap.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(configMap.UID),
		ResourceVersion: configMap.ResourceVersion,
		Labels:          configMap.Labels,
		Annotations:     configMap.Annotations,
	})
}

func (h *Handler) update(c *gin.Context) {
	k8s := namespaced.ExtractClient(c)
	if k8s == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")
	var req ConfigMapUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	configMap := &corev1.ConfigMap{}
	if err := k8s.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	if value, ok := configMap.Labels[consts.ManagedLabelKey]; !ok || value != consts.ManagedLabelValue {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}
	configMap.Data = req.Data
	if err := k8s.Update(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, ConfigMapResponse{
		Name:            configMap.Name,
		Namespace:       configMap.Namespace,
		Data:            configMap.Data,
		CreatedAt:       configMap.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(configMap.UID),
		ResourceVersion: configMap.ResourceVersion,
		Labels:          configMap.Labels,
		Annotations:     configMap.Annotations,
	})
}

func (h *Handler) delete(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	configMap := &corev1.ConfigMap{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	if value, ok := configMap.Labels[consts.ManagedLabelKey]; !ok || value != consts.ManagedLabelValue {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}
	if err := k8sClient.Delete(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// convertToResponseList transforms a list of corev1.ConfigMap objects into a ConfigMapListResponse for the API response.
func convertToResponseList(configMaps []corev1.ConfigMap) ConfigMapListResponse {
	items := make([]ConfigMapResponse, len(configMaps))
	for i, cm := range configMaps {
		items[i] = ConfigMapResponse{
			Name:            cm.Name,
			Namespace:       cm.Namespace,
			CreatedAt:       cm.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
			UID:             string(cm.UID),
			ResourceVersion: cm.ResourceVersion,
			Labels:          cm.Labels,
			Annotations:     cm.Annotations,
			Data:            cm.Data,
		}
	}
	return ConfigMapListResponse{Items: items, Total: len(items)}
}
