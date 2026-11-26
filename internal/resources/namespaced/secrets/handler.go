// Package secrets implements the secret resource handler.
//
// The frontend uses this handler to perform CRUD operations on secrets in the selected namespace.
// The response is a simplified list; it will only display secrets with the label dana.io/capp-managed=true.
// All secret operations are namespace-scoped and require the user to have permissions in the target namespace.
package secrets

import (
	"bytes"
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

// Handler implements resources.ResourceHandler for Kubernetes Secrets.
type Handler struct {
	functions boundcapps.FunctionGuard
	tracker   *tracking.Tracker
}

// New returns a ready-to-use secret Handler.
func New() *Handler { return &Handler{} }

// WithFunctionGuard turns on the check for cfunc functions that read a
// secret. See boundcapps.FunctionGuard.
func (h *Handler) WithFunctionGuard(enabled bool) *Handler {
	h.functions.Enabled = enabled
	return h
}

// WithTracker records the desired state of Secrets and reports their drift. A
// nil tracker leaves both off.
func (h *Handler) WithTracker(tracker *tracking.Tracker) *Handler {
	h.tracker = tracker
	return h
}

// Name returns the handler's identifier.
func (h *Handler) Name() string { return "secrets" }

// RegisterRoutes attaches the secret routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// cluster-scoped list (all namespaces)
	rg.GET("/secrets", h.listAll)

	// Namespace-scoped operations.
	s := rg.Group("/namespaces/:namespace/secrets")
	s.GET("", h.list)
	s.POST("", h.create)
	s.GET("/names", h.listNames)
	s.GET("/:name", h.get)
	s.PUT("/:name", h.update)
	s.DELETE("/:name", h.delete)
	s.GET("/:name/bound", h.bound)

	tracking.Actions{
		Tracker:  h.tracker,
		Kind:     desiredstate.KindSecret,
		New:      func() client.Object { return &corev1.Secret{} },
		Managed:  isManaged,
		Reverted: restartBoundCapps,
	}.Register(s)
}

func isManaged(object client.Object) bool {
	return object.GetLabels()[consts.ManagedLabelKey] == consts.ManagedLabelValue
}

// restartBoundCapps restarts the opted-in Capps that have not yet been
// restarted for the object's resourceVersion.
func restartBoundCapps(c *gin.Context, k8sClient client.Client, object client.Object) error {
	ctx := c.Request.Context()
	bound, err := boundcapps.Find(ctx, k8sClient, object.GetNamespace(), boundcapps.KindSecret, object.GetName())
	if err != nil {
		return err
	}
	return boundcapps.TriggerRollouts(ctx, k8sClient, object.GetNamespace(), bound.ToReload, boundcapps.KindSecret, object.GetName(), object.GetResourceVersion())
}

func refOf(c *gin.Context) desiredstate.Ref {
	return tracking.Ref(c, desiredstate.KindSecret, c.Param("namespace"), c.Param("name"))
}

func (h *Handler) listAll(c *gin.Context) {
	secretList := &corev1.SecretList{}
	if !namespaced.ListAll(c, secretList, client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}) {
		return
	}

	drifts, missing := h.tracker.InspectAll(c, desiredstate.KindSecret, tracking.Objects(secretList.Items))
	c.JSON(http.StatusOK, convertToResponseList(secretList.Items, drifts, missing))
}

// list handles GET /api/v1/clusters/:cluster/namespaces/:namespace/secrets.
func (h *Handler) list(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")

	secretList := &corev1.SecretList{}
	if err := k8sClient.List(c.Request.Context(), secretList, client.InNamespace(namespace), client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	drifts, missing := h.tracker.InspectList(c, desiredstate.KindSecret, namespace, tracking.Objects(secretList.Items))
	c.JSON(http.StatusOK, convertToResponseList(secretList.Items, drifts, missing))
}

// listNames handles GET /api/v1/clusters/:cluster/namespaces/:namespace/secrets/names.
func (h *Handler) listNames(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")

	secretList := &corev1.SecretList{}
	if err := k8sClient.List(c.Request.Context(), secretList, client.InNamespace(namespace), client.MatchingLabels{
		consts.ManagedLabelKey: consts.ManagedLabelValue,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	names := make([]string, 0, len(secretList.Items))
	for _, s := range secretList.Items {
		names = append(names, s.Name)
	}

	c.JSON(http.StatusOK, SecretNameListResponse{Items: names, Total: len(names)})
}

// bound handles GET /api/v1/clusters/:cluster/namespaces/:namespace/secrets/:name/bound.
func (h *Handler) bound(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	name := c.Param("name")

	secret := &corev1.Secret{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		apierrors.Respond(c, err)
		return
	}
	if !isManaged(secret) {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	resp, err := boundcapps.Find(c.Request.Context(), k8sClient, namespace, boundcapps.KindSecret, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	resp.BoundFunctions, err = h.functions.Bound(c.Request.Context(), k8sClient, namespace, boundcapps.KindSecret, name)
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

	var req SecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	secretType := corev1.SecretType(req.Type)
	if secretType == "" {
		secretType = corev1.SecretTypeOpaque
	}

	data := make(map[string][]byte, len(req.Data))
	for k, v := range req.Data {
		data[k] = []byte(v)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
			Labels: map[string]string{
				consts.ManagedLabelKey: consts.ManagedLabelValue,
			},
		},
		Type: secretType,
		Data: data,
	}

	write := h.tracker.Begin(c, tracking.Ref(c, desiredstate.KindSecret, namespace, secret.Name))
	if err := k8sClient.Create(c.Request.Context(), secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := toResponse(secret)
	resp.Warnings = write.Save(c, k8sClient, secret)
	c.JSON(http.StatusCreated, resp)
}

func (h *Handler) get(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	name := c.Param("name")

	secret := &corev1.Secret{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		h.tracker.RespondReadError(c, refOf(c), nil, err)
		return
	}

	if !isManaged(secret) {
		h.tracker.RespondReadError(c, refOf(c), secret, apierrors.NewNotFound("secret", name))
		return
	}

	resp := toResponse(secret)
	resp.Drift = h.tracker.Inspect(c, refOf(c), secret)
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) update(c *gin.Context) {
	k8s := namespaced.ExtractClient(c)
	if k8s == nil {
		return
	}

	namespace := c.Param("namespace")
	name := c.Param("name")

	var req SecretUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	secret := &corev1.Secret{}
	if err := k8s.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if !isManaged(secret) {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	data := make(map[string][]byte, len(req.Data))
	for k, v := range req.Data {
		data[k] = []byte(v)
	}

	write := h.tracker.Begin(c, refOf(c))
	reverted, ok := write.ResolveDrift(c, secret)
	if !ok {
		return
	}

	// Skip the write when nothing changed so the resourceVersion, which keys
	// Capp restarts, stays the same.
	if reverted || !maps.EqualFunc(secret.Data, data, bytes.Equal) {
		secret.Data = data
		if err := k8s.Update(c.Request.Context(), secret); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	warnings := write.Save(c, k8s, secret)

	// Restart opted-in Capps that have not yet been restarted for this
	// resourceVersion. This is not gated on the data changing: an unchanged
	// PUT keeps the resourceVersion, so it restarts only Capps a previous
	// attempt failed to restart, which makes retrying a failed PUT safe.
	if err := restartBoundCapps(c, k8s, secret); err != nil {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("secret updated, but restarting bound Capps failed: %w", err)))
		return
	}

	resp := toResponse(secret)
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

	secret := &corev1.Secret{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	if !isManaged(secret) {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	bound, err := boundcapps.Find(c.Request.Context(), k8sClient, namespace, boundcapps.KindSecret, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if len(bound.BoundCapps) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("secret %s is still in use by Capps: %s", name, strings.Join(bound.BoundCapps, ", "))))
		return
	}
	functions, err := h.functions.Bound(c.Request.Context(), k8sClient, namespace, boundcapps.KindSecret, name)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if len(functions) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(fmt.Sprintf("secret %s is still in use by functions: %s", name, strings.Join(functions, ", "))))
		return
	}

	write := h.tracker.Begin(c, refOf(c))
	if err := k8sClient.Delete(c.Request.Context(), secret); err != nil {
		apierrors.Respond(c, err)
		return
	}
	tracking.RespondDeleted(c, write.Delete(c))
}

// toResponse converts a corev1.Secret into its API response.
func toResponse(secret *corev1.Secret) SecretResponse {
	return SecretResponse{
		Name:            secret.Name,
		Namespace:       secret.Namespace,
		Type:            string(secret.Type),
		CreatedAt:       secret.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(secret.UID),
		ResourceVersion: secret.ResourceVersion,
		Labels:          secret.Labels,
		Annotations:     secret.Annotations,
		Data:            bytesToStringMap(secret.Data),
	}
}

// convertToResponseList transforms a list of corev1.Secret objects into a SecretListResponse for the API response.
func convertToResponseList(secrets []corev1.Secret, drifts []*drift.Drift, missing []drift.Missing) SecretListResponse {
	items := make([]SecretResponse, len(secrets))
	for i := range secrets {
		items[i] = toResponse(&secrets[i])
		items[i].Drift = drifts[i]
	}
	return SecretListResponse{Items: items, Total: len(items), Missing: missing}
}

// bytesToStringMap decodes the base64-stored []byte values in a Secret's Data
// field into plain strings for the API response.
func bytesToStringMap(in map[string][]byte) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = string(v)
	}
	return out
}
