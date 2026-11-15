package namespaces

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const policyStateInvalid = "invalid"

// Bound concurrent storage reads per discovery request.
const namespaceReadConcurrency = 4

func (h *Handler) getManagedNamespace(c *gin.Context) {
	progress := namespaceOperation{namespace: c.Param("namespace")}
	defer func() { h.auditNamespaceOperation(c, "namespace read", &progress) }()
	operator, state, allowed := requestpolicy.RequireNamespaceAccess(c)
	if !allowed {
		return
	}
	admin := h.administration[c.Param("cluster")]
	if admin == nil {
		progress.respondError(c, authorization.ErrUnavailable)
		return
	}
	var item NamespaceItem
	var err error
	if operator {
		ns := &corev1.Namespace{}
		if err = admin.client.Get(c.Request.Context(), client.ObjectKey{Name: progress.namespace}, ns); err == nil {
			item, progress.policy, err = admin.readNamespace(c.Request.Context(), ns)
		}
	} else {
		progress.policy = &corev1.ConfigMap{}
		progress.policy.UID, progress.policy.ResourceVersion = state.PolicyUID, state.PolicyResourceVersion
		item, err = admin.readObservedNamespace(c.Request.Context(), state)
	}
	if err != nil {
		progress.respondError(c, err)
		return
	}
	progress.succeeded = true
	c.JSON(http.StatusOK, item)
}

func (h *Handler) listAuthorizedNamespaces(c *gin.Context) {
	progress := namespaceOperation{}
	defer func() { h.auditNamespaceOperation(c, "namespace discovery", &progress) }()
	states, _, ok := requestpolicy.RequireDiscovery(c, c.Param("cluster"))
	if !ok {
		return
	}
	admin := h.administration[c.Param("cluster")]
	if len(states) != 0 && admin == nil {
		progress.respondError(c, authorization.ErrUnavailable)
		return
	}
	items, err := readNamespaceItems(c.Request.Context(), len(states), func(ctx context.Context, i int) (NamespaceItem, error) {
		state := states[i]
		policy := &corev1.ConfigMap{}
		policy.UID, policy.ResourceVersion = state.PolicyUID, state.PolicyResourceVersion
		item, err := admin.readObservedNamespace(ctx, state)
		if err != nil {
			return NamespaceItem{}, &namespaceReadError{namespace: state.Namespace.Name, policy: policy, err: err}
		}
		return item, nil
	})
	if err != nil {
		progress.respondError(c, err)
		return
	}
	progress.succeeded = true
	c.JSON(http.StatusOK, NamespaceListResponse{Items: items, Total: len(items)})
}

func (h *Handler) listManagedNamespaces(c *gin.Context) {
	progress := namespaceOperation{}
	defer func() { h.auditNamespaceOperation(c, "namespace discovery", &progress) }()
	admin := h.requireAdministration(c)
	if admin == nil {
		return
	}
	var namespaces corev1.NamespaceList
	if err := admin.client.List(c.Request.Context(), &namespaces, client.MatchingLabels{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}); err != nil {
		progress.respondError(c, err)
		return
	}
	slices.SortFunc(namespaces.Items, func(a, b corev1.Namespace) int { return strings.Compare(a.Name, b.Name) })
	allowed := make([]*corev1.Namespace, 0, len(namespaces.Items))
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		if requestpolicy.NamespaceAllowed(c, ns.Name) && admin.checkNamespaceName(ns.Name) == nil {
			allowed = append(allowed, ns)
		}
	}
	items, err := readNamespaceItems(c.Request.Context(), len(allowed), func(ctx context.Context, i int) (NamespaceItem, error) {
		ns := allowed[i]
		item, policy, err := admin.readNamespace(ctx, ns)
		var conflict *policyConflictError
		if errors.As(err, &conflict) {
			// Discovery can show the namespace without trusting its policy membership.
			item = NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase), PolicyState: policyStateInvalid}
			item.Quota, err = admin.readQuota(ctx, ns.Name)
		}
		if err != nil {
			return NamespaceItem{}, &namespaceReadError{namespace: ns.Name, policy: policy, err: err}
		}
		return item, nil
	})
	if err != nil {
		progress.respondError(c, err)
		return
	}
	progress.succeeded = true
	c.JSON(http.StatusOK, NamespaceListResponse{Items: items, Total: len(items), CanCreate: true})
}

// readNamespaceItems preserves input order and joins all reads before returning.
func readNamespaceItems(ctx context.Context, count int, read func(context.Context, int) (NamespaceItem, error)) ([]NamespaceItem, error) {
	group, readCtx := errgroup.WithContext(ctx)
	group.SetLimit(namespaceReadConcurrency)
	items := make([]NamespaceItem, count)
	for i := range count {
		group.Go(func() error {
			if err := readCtx.Err(); err != nil {
				return err
			}
			item, err := read(readCtx, i)
			if err != nil {
				return err
			}
			items[i] = item
			return nil
		})
		if readCtx.Err() != nil {
			break
		}
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// namespaceReadError keeps audit context attached to the failure selected by errgroup.
type namespaceReadError struct {
	namespace string
	policy    *corev1.ConfigMap
	err       error
}

func (e *namespaceReadError) Error() string { return e.err.Error() }

func (e *namespaceReadError) Unwrap() error { return e.err }

// readNamespace reads administrative data for an already authorized target.
// Invalid owned policies remain visible for explicit full-membership repair.
func (a *Administration) readNamespace(ctx context.Context, ns *corev1.Namespace) (NamespaceItem, *corev1.ConfigMap, error) {
	if err := a.checkNamespaceName(ns.Name); err != nil {
		return NamespaceItem{}, nil, err
	}
	if ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return NamespaceItem{}, nil, apierrors.NewConflict("namespace must be managed")
	}
	policy, err := a.readOwnedPolicy(ctx, ns)
	if err != nil {
		return NamespaceItem{}, policy, err
	}
	item := a.policyNamespaceItem(ns, policy)
	item.Quota, err = a.readQuota(ctx, ns.Name)
	return item, policy, err
}

func (a *Administration) readObservedNamespace(ctx context.Context, state authorization.NamespaceState) (NamespaceItem, error) {
	item := namespaceItem(state.Namespace, state.Bindings(), state.PolicyResourceVersion, false)
	var err error
	item.Quota, err = a.readQuota(ctx, state.Namespace.Name)
	return item, err
}

func (a *Administration) policyNamespaceItem(ns *corev1.Namespace, policy *corev1.ConfigMap) NamespaceItem {
	item := namespaceItem(ns, nil, "", true)
	if policy == nil {
		return item
	}
	item.PolicyResourceVersion = policy.ResourceVersion
	assignment, err := authorization.ParseAssignment(policy, a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
	if err != nil {
		item.PolicyState = policyStateInvalid
		item.Users, item.Bindings = nil, nil
		return item
	}
	return namespaceItem(ns, assignment.Bindings(), policy.ResourceVersion, true)
}

func namespaceItem(ns *corev1.Namespace, bindings []authorization.Binding, revision string, operator bool) NamespaceItem {
	users := []string{}
	if bindings == nil {
		bindings = []authorization.Binding{}
	}
	for _, binding := range bindings {
		if binding.Kind == rbacv1.UserKind && binding.Role == authorization.RoleEditor {
			users = append(users, binding.Name)
		}
	}
	return NamespaceItem{Name: ns.Name, Status: string(ns.Status.Phase), Users: &users, Bindings: &bindings, PolicyResourceVersion: revision, CanEdit: operator}
}

func (a *Administration) readQuota(ctx context.Context, namespace string) (*QuotaInfo, error) {
	quota := &corev1.ResourceQuota{}
	err := a.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: namespace + quotaNameSuffix}, quota)
	if k8serrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return quotaInfoFromK8s(quota), nil
}
