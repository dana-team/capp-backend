// Package configmaps implements the config map resource handler.
//
// The frontend uses this handler to perform CRUD operations on  config maps in the selected namespace. The
// response is a simplified list it will only display config maps with the label dana.io/capp-managed=true.
// All config map operations are namespace-scoped and require the user to have permissions in the target namespace.
package configmaps

import (
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	managedConfigMapLabelKey = "dana.io/capp-managed"
)

// ConfigMapItem is the simplified config map representation returned to the frontend.
type ConfigMapItem struct {
	// Name is the Kubernetes config map name.
	Name string `json:"name"`

	// Data is the key-value data stored in the config map.
	Data map[string]string `json:"data"`
}

// ConfigMapListResponse is the response envelope for the list endpoint.
type ConfigMapListResponse struct {
	Items []ConfigMapItem `json:"items"`
}

// Handler implements resources.ResourceHandler for Kubernetes ConfigMaps.
type Handler struct{}

// New returns a ready-to-use config map Handler.
func New() *Handler { return &Handler{} }

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "configmaps" }

// RegisterRoutes attaches the config map routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Namespace-scoped operations.
	cm := rg.Group("/namespaces/:namespace/configmaps")
	cm.GET("", h.list)
	cm.POST("", h.create)
	cm.GET("/:name", h.get)
	cm.PUT("/:name", h.update)
	cm.DELETE("/:name", h.delete)
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
		managedConfigMapLabelKey: "true",
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, convertToResponseList(configMapList.Items))
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
				managedConfigMapLabelKey: "true",
			},
		},
		Data: req.Data,
	}

	if err := k8sClient.Create(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusCreated, ConfigMapItem{Name: configMap.Name, Data: configMap.Data})
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
	c.JSON(http.StatusOK, ConfigMapItem{Name: configMap.Name, Data: configMap.Data})
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
	configMap.Data = req.Data
	if err := k8s.Update(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, ConfigMapItem{Name: configMap.Name, Data: configMap.Data})
}

func (h *Handler) delete(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	namespace := c.Param("namespace")
	name := c.Param("name")

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := k8sClient.Delete(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// convertToResponseList transforms a list of corev1.ConfigMap objects into a ConfigMapListResponse for the API response.
func convertToResponseList(configMaps []corev1.ConfigMap) ConfigMapListResponse {
	items := make([]ConfigMapItem, len(configMaps))
	for i, cm := range configMaps {
		items[i] = ConfigMapItem{
			Name: cm.Name,
			Data: cm.Data,
		}
	}
	return ConfigMapListResponse{Items: items}
}
