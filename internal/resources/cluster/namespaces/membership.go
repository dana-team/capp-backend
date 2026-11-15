package namespaces

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

func (h *Handler) updateManagedNamespace(c *gin.Context) {
	progress := namespaceOperation{namespace: c.Param("namespace")}
	defer func() { h.auditNamespaceOperation(c, "namespace membership mutation", &progress) }()
	admin := h.requireAdministration(c)
	if admin == nil {
		return
	}
	var req UpdateNamespaceRequest
	if err := decodeNamespaceRequest(c, &req); err != nil {
		progress.respondError(c, err)
		return
	}
	proposal, err := admin.prepareMembership(c.Request.Context(), c.Param("namespace"), req, c.Request.Method == http.MethodPatch)
	if proposal != nil {
		progress.policy = proposal.policy
	}
	if err != nil {
		progress.respondError(c, err)
		return
	}
	var quotaInfo *QuotaInfo
	if req.Quota == nil {
		quotaInfo, err = admin.readQuota(c.Request.Context(), proposal.namespace.Name)
		if err != nil {
			progress.respondError(c, err)
			return
		}
	}
	if req.Quota != nil {
		quota, create, err := admin.prepareQuota(c.Request.Context(), proposal.namespace, *req.Quota)
		if err != nil {
			progress.respondError(c, err)
			return
		}
		if create {
			err = admin.client.Create(c.Request.Context(), quota)
		} else {
			err = admin.client.Update(c.Request.Context(), quota)
		}
		if err != nil {
			progress.respondError(c, err)
			return
		}
		progress.completed = append(progress.completed, stepQuotaSaved)
		quotaInfo = quotaInfoFromK8s(quota)
	}
	if err := admin.saveMembership(c.Request.Context(), proposal); err != nil {
		progress.respondError(c, err)
		return
	}
	progress.policy = proposal.policy
	progress.succeeded = true
	policy := proposal.policy
	if proposal.create && !proposal.write {
		policy = nil
	}
	item := admin.policyNamespaceItem(proposal.namespace, policy)
	item.Quota = quotaInfo
	c.JSON(http.StatusOK, item)
}

func decodeNamespaceRequest(c *gin.Context, req any) error {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, authorization.MaxPolicyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &apierrors.APIError{Code: apierrors.CodeBadRequest, Status: http.StatusRequestEntityTooLarge, Message: "request exceeds 512 KiB"}
		}
		return apierrors.NewBadRequest("cannot read request")
	}
	strictErrors, err := strictjson.UnmarshalStrict(body, req)
	if err != nil || len(strictErrors) != 0 {
		return apierrors.NewBadRequest("invalid namespace membership request")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return apierrors.NewBadRequest("request must be an object")
	}
	users, hasUsers := fields["users"]
	bindings, hasBindings := fields["bindings"]
	if (hasUsers && hasBindings) || (hasUsers && string(users) == "null") || (hasBindings && string(bindings) == "null") {
		return apierrors.NewBadRequest("supply either users or bindings as an array")
	}
	if _, quota := fields["quota"]; quota && c.Request.Method == http.MethodPatch {
		return apierrors.NewBadRequest("PATCH cannot change quota")
	}
	return nil
}

// membershipProposal separates validation and fresh reads from persistence so
// namespace quota changes can be validated first and saved before membership.
type membershipProposal struct {
	namespace     *corev1.Namespace
	policy        *corev1.ConfigMap
	create, write bool
}

func (a *Administration) prepareMembership(ctx context.Context, namespace string, req UpdateNamespaceRequest, additive bool) (*membershipProposal, error) {
	// Validate the supplied set even when PATCH would deduplicate against storage.
	bindings, err := membershipBindings(req.Users, req.Bindings)
	if err != nil {
		return nil, err
	}
	template, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: namespace, Bindings: bindings}).ConfigMap(a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
	if err != nil {
		return nil, apierrors.NewBadRequest("invalid namespace membership")
	}
	ns := &corev1.Namespace{}
	if err := a.client.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return nil, err
	}
	if ns.Labels[consts.ManagedNameSpaceLabelKey] != consts.ManagedLabelValue || ns.DeletionTimestamp != nil {
		return nil, apierrors.NewConflict("namespace must be managed and nonterminating")
	}
	p := &membershipProposal{namespace: ns, policy: template}
	current := &corev1.ConfigMap{}
	err = a.client.Get(ctx, client.ObjectKeyFromObject(template), current)
	if err != nil && !k8serrors.IsNotFound(err) {
		return p, err
	}
	p.create = k8serrors.IsNotFound(err)
	if !p.create {
		p.policy = current
	}
	if req.PolicyResourceVersion != nil && (p.create || *req.PolicyResourceVersion != current.ResourceVersion) {
		return p, apierrors.NewConflict("policy revision has changed")
	}
	if p.create && additive {
		return p, apierrors.NewNotFound("policy", namespace)
	}
	if !p.create {
		if err := a.checkPolicyOwnershipAndIdentity(current, template, ns); err != nil {
			return p, err
		}
	}
	if req.Users == nil && req.Bindings == nil && !additive {
		return p, nil
	}
	if !p.create && (additive || req.Users != nil) {
		existing, err := authorization.ParseAssignment(current, a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
		if err != nil {
			return p, apierrors.NewConflict("invalid policy requires full bindings replacement")
		}
		bindings = mergeMembership(existing.Bindings(), bindings, additive)
		if additive && slices.Equal(bindings, existing.Bindings()) {
			return p, nil
		}
	}
	proposed, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: namespace, Bindings: bindings}).ConfigMap(a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
	if err != nil {
		return p, apierrors.NewBadRequest("invalid proposed policy or policy exceeds 512 KiB")
	}
	if ptr.Deref(current.Immutable, false) {
		if !maps.Equal(current.Data, proposed.Data) || len(current.BinaryData) != 0 {
			return p, apierrors.NewConflict("policy data is immutable; manual recovery required")
		}
		return p, nil
	}
	p.policy.Data, p.policy.BinaryData = proposed.Data, nil
	p.write = true
	return p, nil
}

func membershipBindings(users *[]string, bindings *[]authorization.Binding) ([]authorization.Binding, error) {
	if users != nil && bindings != nil {
		return nil, apierrors.NewBadRequest("supply either users or bindings")
	}
	if bindings != nil {
		return *bindings, nil
	}
	result := []authorization.Binding{}
	if users != nil {
		for _, user := range *users {
			result = append(result, authorization.Binding{Kind: rbacv1.UserKind, Name: user, Role: authorization.RoleEditor})
		}
	}
	return result, nil
}

func mergeMembership(existing, supplied []authorization.Binding, additive bool) []authorization.Binding {
	result := make([]authorization.Binding, 0, len(existing)+len(supplied))
	for _, binding := range existing {
		if additive || binding.Kind != rbacv1.UserKind || binding.Role != authorization.RoleEditor {
			result = append(result, binding)
		}
	}
	for _, binding := range supplied {
		if !slices.Contains(result, binding) {
			result = append(result, binding)
		}
	}
	return result
}

func (a *Administration) saveMembership(ctx context.Context, p *membershipProposal) error {
	if !p.write {
		return nil
	}
	if p.create {
		metav1.SetMetaDataAnnotation(&p.policy.ObjectMeta, authorization.NamespaceUIDAnnotation, string(p.namespace.UID))
		return a.client.Create(ctx, p.policy)
	}
	// Update carries both the UID and resourceVersion from the fresh read. No
	// retry can silently merge a concurrent operator edit or worker association.
	return a.client.Update(ctx, p.policy)
}
