// Package configmaps implements the config map resource handler.
//
// The frontend uses this handler to perform CRUD operations on  config maps in the selected namespace. The
// response is a simplified list it will only display config maps with the label dana.io/capp-managed=true.
// All config map operations are namespace-scoped and require the user to have permissions in the target namespace.
package configmaps

import (
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/boundcapps"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for Kubernetes ConfigMaps.
type Handler struct {
	functions boundcapps.FunctionGuard
	tracker   *tracking.Tracker
}

// New returns a ready-to-use config map Handler.
func New() *Handler { return &Handler{} }

// WithFunctionGuard turns on the check for cfunc functions that read a
// configmap. See boundcapps.FunctionGuard.
func (h *Handler) WithFunctionGuard(enabled bool) *Handler {
	h.functions.Enabled = enabled
	return h
}

// WithTracker records the desired state of ConfigMaps and reports their drift. A
// nil tracker leaves both off.
func (h *Handler) WithTracker(tracker *tracking.Tracker) *Handler {
	h.tracker = tracker
	return h
}

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
	cm.GET("/:name/bound", h.bound)

	tracking.Actions{
		Tracker:  h.tracker,
		Kind:     desiredstate.KindConfigMap,
		New:      func() client.Object { return &corev1.ConfigMap{} },
		Managed:  isManaged,
		Reverted: restartBoundCapps,
	}.Register(cm)
}

func isManaged(object client.Object) bool {
	return object.GetLabels()[consts.ManagedLabelKey] == consts.ManagedLabelValue
}

// restartBoundCapps restarts the opted-in Capps that have not yet been
// restarted for the object's resourceVersion.
func restartBoundCapps(c *gin.Context, k8sClient client.Client, object client.Object) error {
	ctx := c.Request.Context()
	bound, err := boundcapps.Find(ctx, k8sClient, object.GetNamespace(), boundcapps.KindConfigMap, object.GetName())
	if err != nil {
		return err
	}
	return boundcapps.TriggerRollouts(ctx, k8sClient, object.GetNamespace(), bound.ToReload, boundcapps.KindConfigMap, object.GetName(), object.GetResourceVersion())
}

func refOf(c *gin.Context) desiredstate.Ref {
	return tracking.Ref(c, desiredstate.KindConfigMap, c.Param("namespace"), c.Param("name"))
}

func (h *Handler) listAll(c *gin.Context) {
	configMapList := &corev1.ConfigMapList{}
	if !namespaced.ListAll(c, configMapList, client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}) {
		return
	}
	drifts, missing := h.tracker.InspectAll(c, desiredstate.KindConfigMap, tracking.Objects(configMapList.Items))
	c.JSON(http.StatusOK, convertToResponseList(configMapList.Items, drifts, missing))
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
	drifts, missing := h.tracker.InspectList(c, desiredstate.KindConfigMap, namespace, tracking.Objects(configMapList.Items))
	c.JSON(http.StatusOK, convertToResponseList(configMapList.Items, drifts, missing))
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

// bound handles GET /api/v1/clusters/:cluster/namespaces/:namespace/configmaps/:name/bound.
func (h *Handler) bound(c *gin.Context) {
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
	if !isManaged(configMap) {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}

	resp, err := boundcapps.Find(c.Request.Context(), k8sClient, namespace, boundcapps.KindConfigMap, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	resp.BoundFunctions, err = h.functions.Bound(c.Request.Context(), k8sClient, namespace, boundcapps.KindConfigMap, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
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

	write := h.tracker.Begin(c, tracking.Ref(c, desiredstate.KindConfigMap, namespace, configMap.Name))
	if err := k8sClient.Create(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := toResponse(configMap)
	resp.Warnings = write.Save(c, k8sClient, configMap)
	c.JSON(http.StatusCreated, resp)
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
		h.tracker.RespondReadError(c, refOf(c), nil, err)
		return
	}
	if !isManaged(configMap) {
		h.tracker.RespondReadError(c, refOf(c), configMap, apierrors.NewNotFound("configmap", name))
		return
	}
	resp := toResponse(configMap)
	resp.Drift = h.tracker.Inspect(c, refOf(c), configMap)
	c.JSON(http.StatusOK, resp)
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
	if !isManaged(configMap) {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}
	write := h.tracker.Begin(c, refOf(c))
	reverted, ok := write.ResolveDrift(c, configMap)
	if !ok {
		return
	}

	// Skip the write when nothing changed so the resourceVersion, which keys
	// Capp restarts, stays the same.
	if reverted || !maps.Equal(configMap.Data, req.Data) {
		configMap.Data = req.Data
		if err := k8s.Update(c.Request.Context(), configMap); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	warnings := write.Save(c, k8s, configMap)

	// Restart opted-in Capps that have not yet been restarted for this
	// resourceVersion. This is not gated on the data changing: an unchanged
	// PUT keeps the resourceVersion, so it restarts only Capps a previous
	// attempt failed to restart, which makes retrying a failed PUT safe.
	if err := restartBoundCapps(c, k8s, configMap); err != nil {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("configmap updated, but restarting bound Capps failed: %w", err)))
		return
	}
	resp := toResponse(configMap)
	resp.Warnings = warnings
	c.JSON(http.StatusOK, resp)
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
	if !isManaged(configMap) {
		apierrors.Respond(c, apierrors.NewNotFound("configmap", name))
		return
	}
	bound, err := boundcapps.Find(c.Request.Context(), k8sClient, namespace, boundcapps.KindConfigMap, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if len(bound.BoundCapps) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("configmap %s is still in use by Capps: %s", name, strings.Join(bound.BoundCapps, ", "))))
		return
	}
	functions, err := h.functions.Bound(c.Request.Context(), k8sClient, namespace, boundcapps.KindConfigMap, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if len(functions) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("configmap %s is still in use by functions: %s", name, strings.Join(functions, ", "))))
		return
	}
	write := h.tracker.Begin(c, refOf(c))
	if err := k8sClient.Delete(c.Request.Context(), configMap); err != nil {
		apierrors.Respond(c, err)
		return
	}
	tracking.RespondDeleted(c, write.Delete(c))
}

// toResponse converts a corev1.ConfigMap into its API response.
func toResponse(configMap *corev1.ConfigMap) ConfigMapResponse {
	return ConfigMapResponse{
		Name:            configMap.Name,
		Namespace:       configMap.Namespace,
		CreatedAt:       configMap.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(configMap.UID),
		ResourceVersion: configMap.ResourceVersion,
		Labels:          configMap.Labels,
		Annotations:     configMap.Annotations,
		Data:            configMap.Data,
	}
}

// convertToResponseList transforms a list of corev1.ConfigMap objects into a ConfigMapListResponse for the API response.
func convertToResponseList(configMaps []corev1.ConfigMap, drifts []*drift.Drift, missing []drift.Missing) ConfigMapListResponse {
	items := make([]ConfigMapResponse, len(configMaps))
	for i := range configMaps {
		items[i] = toResponse(&configMaps[i])
		items[i].Drift = drifts[i]
	}
	return ConfigMapListResponse{Items: items, Total: len(items), Missing: missing}
}
