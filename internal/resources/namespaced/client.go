package namespaced

import (
	"errors"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/utils"
	"github.com/gin-gonic/gin"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ExtractClient retrieves the scoped K8s client from the Gin context.
// It responds with an internal error and returns nil if the client is absent
// (which indicates a middleware configuration error).
func ExtractClient(c *gin.Context) client.Client {
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
