package namespaces

import (
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (h *Handler) listAuthorized(c *gin.Context, state *middleware.AuthorizationState) {
	clusterName := c.Param("cluster")
	target := state.Targets[clusterName]
	value, _ := c.Get(string(middleware.ClusterMetaKey))
	meta, ok := value.(cluster.ClusterMeta)
	if target == nil || target.Client == nil || !ok {
		middleware.RespondAuthorization(c, authorization.ErrPolicyUnavailable)
		return
	}
	canCreate, err := target.Snapshot.CheckCluster(authorization.ResourceNamespaces, authorization.ActionCreate)
	if err != nil {
		middleware.RespondAuthorization(c, err)
		return
	}
	namespaces, err := target.Snapshot.Namespaces()
	if err != nil {
		middleware.RespondAuthorization(c, err)
		return
	}
	items := make([]NamespaceItem, 0, len(namespaces))
	for i := range namespaces {
		namespace := &namespaces[i]
		if namespace.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedNameSpaceLabelValue ||
			namespace.Name == target.Config.PolicyNamespace || slices.Contains(target.Config.ProtectedNamespaces, namespace.Name) ||
			(len(meta.AllowedNamespaces) > 0 && !slices.Contains(meta.AllowedNamespaces, namespace.Name)) {
			continue
		}
		allowed, err := target.Snapshot.CheckNamespace(namespace, authorization.ResourceNamespaces, authorization.ActionGet)
		state.RecordDecision(clusterName, namespace.Name, authorization.ResourceNamespaces, authorization.ActionGet, allowed, err)
		if err != nil {
			middleware.RespondAuthorization(c, err)
			return
		}
		if !allowed {
			continue
		}
		item, err := authorizedNamespaceItem(target.Snapshot, namespace)
		if err != nil {
			middleware.RespondAuthorization(c, err)
			return
		}
		items = append(items, item)
	}
	if len(items) > 0 {
		quotas := listQuotaInfoMap(c.Request.Context(), target.Client, h.logger)
		for i := range items {
			items[i].Quota = quotas[items[i].Name]
		}
	}
	c.JSON(http.StatusOK, NamespaceListResponse{Items: items, Total: len(items), CanCreate: canCreate})
}

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
	update, err := snapshot.CheckNamespace(namespace, authorization.ResourceNamespaces, authorization.ActionUpdate)
	if err != nil {
		return NamespaceItem{}, err
	}
	replace, err := snapshot.CheckNamespace(namespace, authorization.ResourceCappUserRole, authorization.ActionReplace)
	if err != nil {
		return NamespaceItem{}, err
	}
	return NamespaceItem{
		Name: namespace.Name, Status: string(namespace.Status.Phase), Users: &users, CanEdit: update && replace,
	}, nil
}
