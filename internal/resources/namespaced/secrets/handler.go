// Package secrets implements the secret resource handler.
//
// The frontend uses this handler to perform CRUD operations on secrets in the selected namespace.
// The response is a simplified list; it will only display secrets with the label dana.io/capp-managed=true.
// All secret operations are namespace-scoped and require the user to have permissions in the target namespace.
package secrets

import (
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Handler implements resources.ResourceHandler for Kubernetes Secrets.
type Handler struct{}

// New returns a ready-to-use secret Handler.
func New() *Handler { return &Handler{} }

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
	s.GET("/:name", h.get)
	s.PUT("/:name", h.update)
	s.DELETE("/:name", h.delete)
}

func (h *Handler) listAll(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	secretList := &corev1.SecretList{}
	if err := k8sClient.List(c.Request.Context(), secretList, client.MatchingLabels{
		consts.ManagedLabelKey: "true",
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, convertToResponseList(secretList.Items))
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
		consts.ManagedLabelKey: "true",
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, convertToResponseList(secretList.Items))
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
				consts.ManagedLabelKey: "true",
			},
		},
		Type: secretType,
		Data: data,
	}

	if err := k8sClient.Create(c.Request.Context(), secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusCreated, SecretResponse{
		Name:            secret.Name,
		Namespace:       secret.Namespace,
		Type:            string(secret.Type),
		CreatedAt:       secret.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(secret.UID),
		ResourceVersion: secret.ResourceVersion,
		Labels:          secret.Labels,
		Annotations:     secret.Annotations,
		Data:            bytesToStringMap(secret.Data),
	})
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
		apierrors.Respond(c, err)
		return
	}

	if value, ok := secret.Labels[consts.ManagedLabelKey]; !ok || value != "true" {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	c.JSON(http.StatusOK, SecretResponse{
		Name:            secret.Name,
		Namespace:       secret.Namespace,
		Type:            string(secret.Type),
		CreatedAt:       secret.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(secret.UID),
		ResourceVersion: secret.ResourceVersion,
		Labels:          secret.Labels,
		Annotations:     secret.Annotations,
		Data:            bytesToStringMap(secret.Data),
	})
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

	if value, ok := secret.Labels[consts.ManagedLabelKey]; !ok || value != "true" {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	data := make(map[string][]byte, len(req.Data))
	for k, v := range req.Data {
		data[k] = []byte(v)
	}
	secret.Data = data

	if err := k8s.Update(c.Request.Context(), secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, SecretResponse{
		Name:            secret.Name,
		Namespace:       secret.Namespace,
		Type:            string(secret.Type),
		CreatedAt:       secret.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
		UID:             string(secret.UID),
		ResourceVersion: secret.ResourceVersion,
		Labels:          secret.Labels,
		Annotations:     secret.Annotations,
		Data:            bytesToStringMap(secret.Data),
	})
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

	if value, ok := secret.Labels[consts.ManagedLabelKey]; !ok || value != "true" {
		apierrors.Respond(c, apierrors.NewNotFound("secret", name))
		return
	}

	if err := k8sClient.Delete(c.Request.Context(), secret); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.Status(http.StatusNoContent)
}

// convertToResponseList transforms a list of corev1.Secret objects into a SecretListResponse for the API response.
func convertToResponseList(secrets []corev1.Secret) SecretListResponse {
	items := make([]SecretResponse, len(secrets))
	for i, s := range secrets {
		items[i] = SecretResponse{
			Name:            s.Name,
			Namespace:       s.Namespace,
			Type:            string(s.Type),
			CreatedAt:       s.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
			UID:             string(s.UID),
			ResourceVersion: s.ResourceVersion,
			Labels:          s.Labels,
			Annotations:     s.Annotations,
			Data:            bytesToStringMap(s.Data),
		}
	}
	return SecretListResponse{Items: items, Total: len(items)}
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
