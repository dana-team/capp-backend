package namespaces

import (
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getAuthorized consumes metadata whose visibility was checked by middleware.
func (h *Handler) getAuthorized(c *gin.Context, state *middleware.AuthorizationState) {
	target := state.Targets[c.Param("cluster")]
	if target == nil || target.Client == nil || state.Namespace == nil {
		middleware.RespondAuthorization(c, authorization.ErrPolicyUnavailable)
		return
	}
	item, err := authorizedNamespaceItem(target.Snapshot, state.Namespace)
	if err != nil {
		middleware.RespondAuthorization(c, err)
		return
	}
	var quota corev1.ResourceQuota
	err = target.Client.Get(c.Request.Context(), client.ObjectKey{
		Name: item.Name + "-quota", Namespace: item.Name,
	}, &quota)
	if err != nil && !k8serrors.IsNotFound(err) {
		apierrors.Respond(c, err)
		return
	}
	if err == nil {
		item.Quota = quotaInfoFromK8s(&quota)
	}
	c.JSON(http.StatusOK, item)
}

// authorizedNamespaceItem builds policy-derived fields after visibility checks.
func authorizedNamespaceItem(snapshot *authorization.Snapshot, namespace *corev1.Namespace) (NamespaceItem, error) {
	users, err := snapshot.DirectUsers(namespace)
	if err != nil {
		return NamespaceItem{}, err
	}
	update, err := snapshot.CheckNamespace(namespace, "namespaces", "update")
	if err != nil {
		return NamespaceItem{}, err
	}
	replace, err := snapshot.CheckNamespace(namespace, "roles/capp-user", "replace")
	if err != nil {
		return NamespaceItem{}, err
	}
	return NamespaceItem{
		Name: namespace.Name, Status: string(namespace.Status.Phase), Users: &users, CanEdit: update && replace,
	}, nil
}
