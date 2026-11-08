package namespaces

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const stepQuotaSaved = "quotaSaved"

// namespaceOperation records request outcomes and acknowledged writes, not cache
// convergence. Failed requests retain successful resources for explicit inspection
// and recovery.
type namespaceOperation struct {
	namespace     string
	policy        *corev1.ConfigMap
	completed     []string
	succeeded     bool
	failureReason metav1.StatusReason
}

func (h *Handler) createManagedNamespace(c *gin.Context) {
	progress := namespaceOperation{}
	defer func() { h.auditNamespaceOperation(c, "namespace lifecycle mutation", &progress) }()
	admin := h.requireAdministration(c)
	if admin == nil {
		return
	}
	var req CreateNamespaceRequest
	if err := decodeNamespaceRequest(c, &req); err != nil {
		progress.respondError(c, err)
		return
	}
	progress.namespace = req.Name
	if !middleware.RequireNamespaceAllowed(c, req.Name) {
		return
	}
	ns, quota, policy, err := admin.prepareNamespaceCreate(c.Request.Context(), req)
	if err != nil {
		progress.respondError(c, err)
		return
	}
	progress.policy = policy
	if err := admin.client.Create(c.Request.Context(), ns); err != nil {
		progress.respondError(c, err)
		return
	}
	progress.completed = append(progress.completed, "namespaceCreated")
	metav1.SetMetaDataAnnotation(&policy.ObjectMeta, authorization.NamespaceUIDAnnotation, string(ns.UID))
	if quota != nil {
		if err := admin.client.Create(c.Request.Context(), quota); err != nil {
			progress.respondError(c, err)
			return
		}
		progress.completed = append(progress.completed, stepQuotaSaved)
	}
	if err := admin.client.Create(c.Request.Context(), policy); err != nil {
		progress.respondError(c, err)
		return
	}
	progress.succeeded = true
	item := admin.policyNamespaceItem(ns, policy)
	if quota != nil {
		item.Quota = quotaInfoFromK8s(quota)
	}
	c.JSON(http.StatusCreated, item)
}

func (h *Handler) deleteManagedNamespace(c *gin.Context) {
	progress := namespaceOperation{namespace: c.Param("namespace")}
	defer func() { h.auditNamespaceOperation(c, "namespace lifecycle mutation", &progress) }()
	admin := h.requireAdministration(c)
	if admin == nil {
		return
	}
	ns, policy, err := admin.prepareNamespaceDelete(c.Request.Context(), progress.namespace)
	progress.policy = policy
	if err != nil {
		progress.respondError(c, err)
		return
	}
	// Repeating this UID-conditioned request also permits cleanup while the same
	// namespace is terminating. An absent namespace requires manual policy cleanup.
	if err := admin.client.Delete(c.Request.Context(), ns, client.Preconditions{UID: &ns.UID}); err != nil {
		progress.respondError(c, err)
		return
	}
	progress.completed = append(progress.completed, "namespaceDeletionAccepted")
	if policy != nil {
		err := admin.client.Delete(c.Request.Context(), policy, client.Preconditions{UID: &policy.UID, ResourceVersion: &policy.ResourceVersion})
		if err != nil && !k8serrors.IsNotFound(err) {
			progress.respondError(c, err)
			return
		}
	}
	progress.succeeded = true
	c.Status(http.StatusNoContent)
}

// requireAdministration verifies operator access and returns the cluster's
// administration component. On failure it writes an error response and returns
// nil; callers must stop processing.
func (h *Handler) requireAdministration(c *gin.Context) *Administration {
	operator, _, allowed := middleware.RequireNamespaceAccess(c)
	if !allowed {
		return nil
	}
	if !operator {
		apierrors.Respond(c, apierrors.NewForbidden(authorization.ErrDenied.Error()))
		return nil
	}
	admin := h.administration[c.Param("cluster")]
	if admin == nil {
		apierrors.Respond(c, administrationError(authorization.ErrUnavailable))
	}
	return admin
}

func (a *Administration) prepareNamespaceCreate(ctx context.Context, req CreateNamespaceRequest) (*corev1.Namespace, *corev1.ResourceQuota, *corev1.ConfigMap, error) {
	if err := a.checkNamespaceName(req.Name); err != nil {
		return nil, nil, nil, err
	}
	bindings, err := membershipBindings(req.Users, req.Bindings)
	if err != nil {
		return nil, nil, nil, err
	}
	policy, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: req.Name, Bindings: bindings}).ConfigMap(a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
	if err != nil {
		return nil, nil, nil, apierrors.NewBadRequest("invalid namespace membership")
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue}}}
	var quota *corev1.ResourceQuota
	if req.Quota != nil {
		quota, err = validatedQuota(*req.Quota, ns)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	// Both names are create-only. In particular, an orphan policy must never be
	// adopted by a newly created namespace, even when it is still unassociated.
	err = a.client.Get(ctx, client.ObjectKeyFromObject(ns), &corev1.Namespace{})
	if err == nil {
		return nil, nil, nil, apierrors.NewConflict("namespace already exists")
	}
	if !k8serrors.IsNotFound(err) {
		return nil, nil, nil, err
	}
	err = a.client.Get(ctx, client.ObjectKeyFromObject(policy), &corev1.ConfigMap{})
	if err == nil {
		return nil, nil, nil, apierrors.NewConflict("policy already exists; manual recovery required")
	}
	if !k8serrors.IsNotFound(err) {
		return nil, nil, nil, err
	}
	return ns, quota, policy, nil
}

func (a *Administration) prepareNamespaceDelete(ctx context.Context, name string) (*corev1.Namespace, *corev1.ConfigMap, error) {
	if err := a.checkNamespaceName(name); err != nil {
		return nil, nil, err
	}
	ns := &corev1.Namespace{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		return nil, nil, err
	}
	if ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue {
		return nil, nil, apierrors.NewConflict("namespace must be managed")
	}
	policy, err := a.readOwnedPolicy(ctx, ns)
	if err != nil {
		return nil, policy, err
	}
	var capps cappv1alpha1.CappList
	if err := a.client.List(ctx, &capps, client.InNamespace(name)); err != nil {
		return nil, policy, err
	}
	if len(capps.Items) != 0 {
		return nil, policy, apierrors.NewConflict("namespace contains Capps")
	}
	return ns, policy, nil
}

func (a *Administration) checkNamespaceName(name string) error {
	if len(validation.ValidateNamespaceName(name, false)) != 0 {
		return apierrors.NewBadRequest("invalid namespace name")
	}
	if name == a.policyNamespace || slices.Contains(a.protectedNamespaces, name) {
		denied := apierrors.NewForbidden(authorization.ErrDenied.Error())
		denied.Details = map[string]any{"reason": apierrors.ReasonPolicyDenied}
		return denied
	}
	return nil
}

func (a *Administration) prepareQuota(ctx context.Context, ns *corev1.Namespace, quota resourceQuota) (*corev1.ResourceQuota, bool, error) {
	proposed, err := validatedQuota(quota, ns)
	if err != nil {
		return nil, false, err
	}
	current := &corev1.ResourceQuota{}
	err = a.client.Get(ctx, client.ObjectKeyFromObject(proposed), current)
	if k8serrors.IsNotFound(err) {
		return proposed, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if current.DeletionTimestamp != nil {
		return nil, false, apierrors.NewConflict("quota is terminating")
	}
	if err := canUpdateQuota(*current, *proposed); err != nil {
		return nil, false, apierrors.NewBadRequest("quota cannot be reduced below current usage")
	}
	// Preserve identity and metadata while replacing the quota specification.
	current.Spec = proposed.Spec
	return current, false, nil
}

func validatedQuota(quota resourceQuota, ns *corev1.Namespace) (*corev1.ResourceQuota, error) {
	proposed, err := generateResourceQuota(quota, ns)
	if err != nil {
		return nil, apierrors.NewBadRequest("invalid namespace quota")
	}
	for _, quantity := range proposed.Spec.Hard {
		if quantity.Sign() < 0 {
			return nil, apierrors.NewBadRequest("quota must not be negative")
		}
	}
	return proposed, nil
}

func (op *namespaceOperation) respondError(c *gin.Context, err error) {
	var readError *namespaceReadError
	if errors.As(err, &readError) {
		op.namespace, op.policy = readError.namespace, readError.policy
	}
	op.failureReason = administrationFailureReason(err)
	sanitized := administrationError(err)
	if len(op.completed) != 0 {
		response := *sanitized
		response.Details = map[string]any{"completedSteps": slices.Clone(op.completed)}
		sanitized = &response
	}
	apierrors.Respond(c, sanitized)
}

func (h *Handler) auditNamespaceOperation(c *gin.Context, message string, op *namespaceOperation) {
	value, _ := c.Get(string(middleware.CredentialKey))
	cred, _ := value.(auth.ClusterCredential)
	uid, revision := "", ""
	if op.policy != nil {
		uid, revision = string(op.policy.UID), op.policy.ResourceVersion
	}
	outcome := "failed"
	if op.succeeded {
		outcome = "succeeded"
	} else if len(op.completed) != 0 {
		outcome = "partial"
	}
	h.logger.Info(message, zap.String("caller", cred.ImpersonateUser),
		zap.String("method", c.Request.Method), zap.String("route", c.FullPath()),
		zap.String("cluster", c.Param("cluster")), zap.String("namespace", op.namespace),
		zap.String("outcome", outcome), zap.String("policyUID", uid), zap.String("policyVersion", revision),
		zap.Strings("completedSteps", op.completed), zap.String("failureReason", string(op.failureReason)))
}
