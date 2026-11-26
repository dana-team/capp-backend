package namespaces

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/internal/resources/tracking"
	"github.com/dana-team/capp-backend/internal/resources/utils"
	"github.com/dana-team/capp-backend/pkg/api/common"
	"github.com/dana-team/capp-backend/pkg/api/drift"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WithTracker records the desired state of namespaces and reports their drift.
// A nil tracker leaves both off.
func (h *Handler) WithTracker(tracker *tracking.Tracker) *Handler {
	h.tracker = tracker
	return h
}

// errInvalidPolicy means that the membership cannot be compared or recorded.
var errInvalidPolicy = apierrors.NewConflict("invalid policy requires full bindings replacement")

// namespaceAccess reads and changes one namespace the way the active
// authorization mode stores it. The caller has already been authorized.
type namespaceAccess struct {
	// read returns nil when the namespace does not exist or is not managed.
	read func(context.Context) (*desiredstate.NamespaceState, error)
	// create and update make the namespace match a desired state.
	create, update func(context.Context, desiredstate.NamespaceState) error
}

func namespaceRef(c *gin.Context) desiredstate.Ref {
	return desiredstate.NamespaceRef(c.Param("cluster"), c.Param("namespace"))
}

// namespaceState projects what the API manages of a namespace.
func namespaceState(item NamespaceItem) desiredstate.NamespaceState {
	var state desiredstate.NamespaceState
	if item.Quota != nil {
		state.Quota = &desiredstate.QuotaState{CPU: item.Quota.CPU, Memory: item.Quota.Memory}
		if item.Quota.Pods != nil {
			state.Quota.Pods = *item.Quota.Pods
		}
	}
	// Backend-policy mode also fills Users, as a summary of Bindings.
	switch {
	case item.Bindings != nil:
		for _, binding := range *item.Bindings {
			state.Bindings = append(state.Bindings, desiredstate.BindingState(binding))
		}
	case item.Users != nil:
		state.Users = *item.Users
	}
	return state
}

func requestedQuota(state desiredstate.NamespaceState) *resourceQuota {
	if state.Quota == nil {
		return nil
	}
	return &resourceQuota{CPU: state.Quota.CPU, Memory: state.Quota.Memory, Pods: state.Quota.Pods}
}

func requestedBindings(state desiredstate.NamespaceState) *[]authorization.Binding {
	bindings := make([]authorization.Binding, 0, len(state.Bindings))
	for _, binding := range state.Bindings {
		bindings = append(bindings, authorization.Binding(binding))
	}
	return &bindings
}

// inspect returns the drift of an existing namespace.
func (h *Handler) inspect(c *gin.Context, ns *corev1.Namespace, item NamespaceItem) *drift.Drift {
	if !h.tracker.Enabled() || !ns.DeletionTimestamp.IsZero() || ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return nil
	}
	if item.PolicyState == policyStateInvalid {
		return &drift.Drift{State: drift.StateUnavailable}
	}
	return h.tracker.Inspect(c, desiredstate.NamespaceRef(c.Param("cluster"), ns.Name), namespaceState(item))
}

// missing lists recorded namespaces that reader does not find. Call it only
// for callers who may create namespaces.
func (h *Handler) missing(c *gin.Context, reader client.Reader, visible func(name string) bool) []drift.Missing {
	var missing []drift.Missing
	for _, candidate := range h.tracker.RecordedNamespaces(c) {
		if !visible(candidate.Name) {
			continue
		}
		err := reader.Get(c.Request.Context(), client.ObjectKey{Name: candidate.Name}, &corev1.Namespace{})
		if k8serrors.IsNotFound(err) {
			missing = append(missing, candidate)
		}
	}
	return missing
}

// resolveDrift applies the onDrift decision of a namespace update. It answers
// the request itself when it returns false.
func (h *Handler) resolveDrift(c *gin.Context, write *tracking.Write, access namespaceAccess) bool {
	if !write.Recorded() {
		_, ok := write.CheckDrift(c, nil)
		return ok
	}
	ctx := c.Request.Context()
	state, err := access.read(ctx)
	if errors.Is(err, errInvalidPolicy) {
		return true // replacing the bindings is how an invalid policy is repaired
	}
	if err != nil {
		apierrors.Respond(c, err)
		return false
	}
	if state == nil {
		return true
	}
	revert, ok := write.CheckDrift(c, *state)
	if !ok || !revert {
		return ok
	}
	desired, err := desiredNamespace(write)
	if err == nil {
		err = access.update(ctx, desired)
	}
	if err != nil {
		apierrors.Respond(c, err)
		return false
	}
	return true
}

// record stores the namespace as it is after a successful change.
func record(c *gin.Context, write *tracking.Write, access namespaceAccess) []common.Warning {
	return write.SaveCurrent(c, func(ctx context.Context) (any, error) {
		state, err := access.read(ctx)
		if err != nil || state == nil {
			return nil, err
		}
		return *state, nil
	})
}

func desiredNamespace(write *tracking.Write) (desiredstate.NamespaceState, error) {
	state, err := write.Desired()
	if err != nil {
		return desiredstate.NamespaceState{}, err
	}
	return state.Namespace()
}

// revertDrift handles POST /api/v1/clusters/:cluster/namespaces/:namespace/drift/revert.
// It needs the permission that PUT needs.
func (h *Handler) revertDrift(c *gin.Context) {
	access, ok := h.revertAccess(c)
	if !ok {
		return
	}
	write, live, current, ok := h.confirm(c, access)
	if !ok {
		return
	}
	if !write.Recorded() {
		apierrors.Respond(c, apierrors.NewNotFound("desired state of namespace", c.Param("namespace")))
		return
	}
	if current.State == drift.StateInSync {
		c.JSON(http.StatusOK, drift.ActionResponse{Drift: current})
		return
	}
	ctx := c.Request.Context()
	desired, err := desiredNamespace(write)
	if err == nil {
		if live == nil {
			err = access.create(ctx, desired)
		} else {
			err = access.update(ctx, desired)
		}
	}
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	h.respondAction(c, access)
}

// acceptDrift handles POST /api/v1/clusters/:cluster/namespaces/:namespace/drift/accept.
// It changes only the store, so every caller who can read the namespace may do it.
func (h *Handler) acceptDrift(c *gin.Context) {
	access, ok := h.acceptAccess(c)
	if !ok {
		return
	}
	write, live, _, ok := h.confirm(c, access)
	if !ok {
		return
	}
	var state any
	if live != nil {
		state = *live
	}
	tracking.RespondAccepted(c, write.AcceptState(c, state), func() *drift.Drift {
		if !write.Recorded() {
			return nil
		}
		return h.tracker.Inspect(c, namespaceRef(c), state)
	})
}

func (h *Handler) confirm(c *gin.Context, access namespaceAccess) (*tracking.Write, *desiredstate.NamespaceState, *drift.Drift, bool) {
	var request drift.ActionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return nil, nil, nil, false
	}
	live, err := access.read(c.Request.Context())
	if err != nil {
		apierrors.Respond(c, err)
		return nil, nil, nil, false
	}
	write, ok := h.tracker.Require(c, namespaceRef(c))
	if !ok {
		return nil, nil, nil, false
	}
	var state any
	if live != nil {
		state = *live
	}
	current, ok := write.Confirm(c, state, request)
	return write, live, current, ok
}

func (h *Handler) respondAction(c *gin.Context, access namespaceAccess) {
	live, err := access.read(c.Request.Context())
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	var state any
	if live != nil {
		state = *live
	}
	c.JSON(http.StatusOK, drift.ActionResponse{Drift: h.tracker.Inspect(c, namespaceRef(c), state)})
}

// revertAccess authorizes a revert like an update.
func (h *Handler) revertAccess(c *gin.Context) (namespaceAccess, bool) {
	if !h.tracker.Enabled() {
		apierrors.Respond(c, apierrors.NewNotSupported("drift"))
		return namespaceAccess{}, false
	}
	if policy.Enabled(c) {
		admin := h.requireAdministration(c)
		if admin == nil {
			return namespaceAccess{}, false
		}
		return admin.access(c.Param("namespace")), true
	}
	userClient, adminClient, ok := kubernetesClients(c)
	if !ok {
		return namespaceAccess{}, false
	}
	allowed, err := canAccessNamespaces(c.Request.Context(), userClient, "create")
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden("not allowed to update namespaces"))
		return namespaceAccess{}, false
	}
	return kubernetesAccess(adminClient, c.Param("namespace")), true
}

// acceptAccess authorizes an accept like a read.
func (h *Handler) acceptAccess(c *gin.Context) (namespaceAccess, bool) {
	if !h.tracker.Enabled() {
		apierrors.Respond(c, apierrors.NewNotSupported("drift"))
		return namespaceAccess{}, false
	}
	namespace := c.Param("namespace")
	if policy.Enabled(c) {
		if _, _, allowed := policy.RequireNamespaceAccess(c); !allowed {
			return namespaceAccess{}, false
		}
		admin := h.administration[c.Param("cluster")]
		if admin == nil {
			apierrors.Respond(c, administrationError(authorization.ErrUnavailable))
			return namespaceAccess{}, false
		}
		return admin.access(namespace), true
	}
	userClient, adminClient, ok := kubernetesClients(c)
	if !ok {
		return namespaceAccess{}, false
	}
	allowed, err := canCreateCapps(c.Request.Context(), userClient, namespace)
	if err != nil || !allowed {
		apierrors.Respond(c, apierrors.NewForbidden(fmt.Sprintf("not allowed to view namespace %s", namespace)))
		return namespaceAccess{}, false
	}
	return kubernetesAccess(adminClient, namespace), true
}

// ── Kubernetes authorization mode ─────────────────────────────────────────────

func kubernetesClients(c *gin.Context) (userClient, adminClient client.Client, ok bool) {
	if userClient, ok = c.MustGet(string(middleware.K8sClientKey)).(client.Client); !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("K8sClientKey")))
		return nil, nil, false
	}
	if adminClient, ok = c.MustGet(string(middleware.AdminK8sClientKey)).(client.Client); !ok {
		apierrors.Respond(c, apierrors.NewInternal(utils.ErrContextMissing("AdminK8sClientKey")))
		return nil, nil, false
	}
	return userClient, adminClient, true
}

func kubernetesAccess(adminClient client.Client, name string) namespaceAccess {
	return namespaceAccess{
		read: func(ctx context.Context) (*desiredstate.NamespaceState, error) {
			if exists, err := managedNamespaceExists(ctx, adminClient, name); err != nil || !exists {
				return nil, err
			}
			item, err := readKubernetesNamespace(ctx, adminClient, name)
			if err != nil {
				return nil, err
			}
			state := namespaceState(item)
			return &state, nil
		},
		create: func(ctx context.Context, desired desiredstate.NamespaceState) error {
			return createNamespace(ctx, adminClient, CreateNamespaceRequest{Name: name, Users: &desired.Users, Quota: requestedQuota(desired)})
		},
		update: func(ctx context.Context, desired desiredstate.NamespaceState) error {
			ns := &corev1.Namespace{}
			ns.Name = name
			if quota := requestedQuota(desired); quota == nil {
				if err := deleteQuota(ctx, adminClient, name); err != nil {
					return err
				}
			} else if err := updateNamespaceQuota(ctx, adminClient, ns, *quota); err != nil {
				return err
			}
			return updateNamespaceRoleBinding(ctx, adminClient, ns, desired.Users)
		},
	}
}

// readKubernetesNamespace reads quota and users, failing when either cannot be
// read. Unlike buildNamespaceItem it never mistakes a failed read for an empty
// value, which would be recorded as desired state.
func readKubernetesNamespace(ctx context.Context, adminClient client.Client, name string) (NamespaceItem, error) {
	quota := &corev1.ResourceQuota{}
	if err := adminClient.Get(ctx, client.ObjectKey{Name: name + quotaNameSuffix, Namespace: name}, quota); client.IgnoreNotFound(err) != nil {
		return NamespaceItem{}, err
	}
	binding := &rbacv1.RoleBinding{}
	if err := adminClient.Get(ctx, client.ObjectKey{Name: name + roleBindingNameSuffix, Namespace: name}, binding); client.IgnoreNotFound(err) != nil {
		return NamespaceItem{}, err
	}
	users := []string{}
	for _, subject := range binding.Subjects {
		if subject.Kind == rbacv1.UserKind {
			users = append(users, subject.Name)
		}
	}
	return NamespaceItem{Name: name, Quota: quotaInfoFromK8s(quota), Users: &users}, nil
}

func managedNamespaceExists(ctx context.Context, reader client.Reader, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		return false, apierrors.NewConflict("namespace is terminating")
	}
	return ns.Labels[consts.ManagedNameSpaceLabelKey] == consts.ManagedLabelValue, nil
}

func deleteQuota(ctx context.Context, adminClient client.Client, namespace string) error {
	quota := &corev1.ResourceQuota{}
	quota.Name, quota.Namespace = namespace+quotaNameSuffix, namespace
	return client.IgnoreNotFound(adminClient.Delete(ctx, quota))
}

// ── Backend-policy authorization mode ─────────────────────────────────────────

func (a *Administration) access(name string) namespaceAccess {
	return namespaceAccess{
		read: func(ctx context.Context) (*desiredstate.NamespaceState, error) {
			if exists, err := managedNamespaceExists(ctx, a.client, name); err != nil || !exists {
				return nil, err
			}
			ns := &corev1.Namespace{}
			if err := a.client.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
				return nil, administrationError(err)
			}
			item, _, err := a.readNamespace(ctx, ns)
			if err != nil {
				return nil, administrationError(err)
			}
			if item.PolicyState == policyStateInvalid {
				return nil, errInvalidPolicy
			}
			state := namespaceState(item)
			return &state, nil
		},
		create: func(ctx context.Context, desired desiredstate.NamespaceState) error {
			ns, quota, policyMap, err := a.prepareNamespaceCreate(ctx, CreateNamespaceRequest{Name: name, Bindings: requestedBindings(desired), Quota: requestedQuota(desired)})
			if err == nil {
				err = a.createNamespace(ctx, &namespaceOperation{}, ns, quota, policyMap)
			}
			return wrapAdministration(err)
		},
		update: func(ctx context.Context, desired desiredstate.NamespaceState) error {
			return wrapAdministration(a.applyNamespace(ctx, name, desired))
		},
	}
}

func (a *Administration) applyNamespace(ctx context.Context, name string, desired desiredstate.NamespaceState) error {
	proposal, err := a.prepareMembership(ctx, name, UpdateNamespaceRequest{Bindings: requestedBindings(desired)}, false)
	if err != nil {
		return err
	}
	if quota := requestedQuota(desired); quota == nil {
		err = deleteQuota(ctx, a.client, name)
	} else {
		err = a.saveQuota(ctx, proposal.namespace, *quota)
	}
	if err != nil {
		return err
	}
	return a.saveMembership(ctx, proposal)
}

func (a *Administration) saveQuota(ctx context.Context, ns *corev1.Namespace, requested resourceQuota) error {
	quota, create, err := a.prepareQuota(ctx, ns, requested)
	if err != nil {
		return err
	}
	if create {
		return a.client.Create(ctx, quota)
	}
	return a.client.Update(ctx, quota)
}

func wrapAdministration(err error) error {
	if err == nil {
		return nil
	}
	return administrationError(err)
}
