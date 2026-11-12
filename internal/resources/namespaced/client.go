package namespaced

import (
	"errors"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/utils"
	"github.com/gin-gonic/gin"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ListAll lists resources using the active authorization mode. Backend policy
// collects only permitted namespaces; Kubernetes-mode mode retains the caller's client.
// It writes an error response and returns false if the collection fails.
func ListAll(c *gin.Context, list client.ObjectList, options ...client.ListOption) bool {
	if policy.Enabled(c) {
		return policy.ListAggregate(c, list, options...)
	}
	k8sClient := ExtractClient(c)
	if k8sClient == nil {
		return false
	}
	if err := k8sClient.List(c.Request.Context(), list, options...); err != nil {
		apierrors.Respond(c, err)
		return false
	}
	return true
}

// ExtractClient retrieves the scoped K8s client from the Gin context.
// Backend-policy requests must carry a matching operation/target decision.
// It responds with an internal error and returns nil if the client is absent
// (which indicates a middleware configuration error).
func ExtractClient(c *gin.Context) client.Client {
	if !policy.RequireExecution(c) {
		return nil
	}
	val, exists := c.Get(string(middleware.K8sClientKey))
	if !exists {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
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
