package namespaces

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/authorization"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Administration manages privileged namespace, quota, and policy operations for one cluster.
// Its client must use the system identity and must never be installed on requests.
type Administration struct {
	client                        client.Client
	policyNamespace               string
	protectedNamespaces           []string
	restrictedBindingNamePrefixes []string
}

// NewAdministration prepares namespace administration without storage I/O.
func NewAdministration(systemClient client.Client, policyNamespace string, protectedNamespaces, restrictedBindingNamePrefixes []string) (*Administration, error) {
	if systemClient == nil || len(validation.ValidateNamespaceName(policyNamespace, false)) != 0 {
		return nil, errors.New("administration requires a client and valid policy namespace")
	}
	if err := authorization.ValidateBindingNamePrefixes(restrictedBindingNamePrefixes); err != nil {
		return nil, err
	}
	return &Administration{
		client: systemClient, policyNamespace: policyNamespace,
		protectedNamespaces:           slices.Clone(protectedNamespaces),
		restrictedBindingNamePrefixes: slices.Clone(restrictedBindingNamePrefixes),
	}, nil
}

// readOwnedPolicy fetches the canonical policy without validating its membership.
// Missing policies return nil; ownership or identity errors retain the fetched
// policy for auditing. Each call reads storage afresh.
func (a *Administration) readOwnedPolicy(ctx context.Context, ns *corev1.Namespace) (*corev1.ConfigMap, error) {
	template, err := (authorization.Policy{Version: authorization.PolicySchemaVersion, Namespace: ns.Name, Bindings: []authorization.Binding{}}).ConfigMap(a.policyNamespace, a.protectedNamespaces, a.restrictedBindingNamePrefixes)
	if err != nil {
		return nil, err
	}
	policy := &corev1.ConfigMap{}
	if err := a.client.Get(ctx, client.ObjectKeyFromObject(template), policy); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return policy, a.checkPolicyOwnershipAndIdentity(policy, template, ns)
}

func (a *Administration) checkPolicyOwnershipAndIdentity(current, template *corev1.ConfigMap, ns *corev1.Namespace) error {
	for key, value := range template.Labels {
		if current.Labels[key] != value {
			return &policyConflictError{APIError: apierrors.NewConflict("canonical policy is not owned by backend policy")}
		}
	}
	if current.DeletionTimestamp != nil {
		return &policyConflictError{APIError: apierrors.NewConflict("policy is terminating")}
	}
	associated := current.Annotations[authorization.NamespaceUIDAnnotation]
	if associated != "" && associated != string(ns.UID) {
		return &policyConflictError{APIError: apierrors.NewConflict("policy belongs to another namespace instance; manual recovery required")}
	}
	return nil
}

// policyConflictError identifies ownership, termination, or namespace-UID conflicts,
// excluding malformed membership and storage errors.
type policyConflictError struct {
	*apierrors.APIError
}

func (e *policyConflictError) Unwrap() error { return e.APIError }

func administrationError(err error) *apierrors.APIError {
	var apiErr *apierrors.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	switch {
	case k8serrors.IsNotFound(err):
		return &apierrors.APIError{Code: apierrors.CodeNotFound, Status: http.StatusNotFound, Message: "namespace or policy not found"}
	case k8serrors.IsConflict(err), k8serrors.IsAlreadyExists(err):
		return apierrors.NewConflict("namespace policy changed; read current state before retrying")
	case errors.Is(err, authorization.ErrUnavailable), k8serrors.IsForbidden(err), k8serrors.IsUnauthorized(err), k8serrors.IsServiceUnavailable(err), k8serrors.IsTimeout(err), k8serrors.IsServerTimeout(err):
		return &apierrors.APIError{Code: apierrors.CodeInternal, Status: http.StatusServiceUnavailable, Message: "namespace administration is unavailable"}
	default:
		return &apierrors.APIError{Code: apierrors.CodeInternal, Status: http.StatusInternalServerError, Message: "namespace administration failed"}
	}
}

// administrationFailureReason accepts only known categories, never upstream text.
func administrationFailureReason(err error) metav1.StatusReason {
	if errors.Is(err, authorization.ErrUnavailable) {
		return metav1.StatusReasonServiceUnavailable
	}
	switch reason := k8serrors.ReasonForError(err); reason {
	case metav1.StatusReasonForbidden, metav1.StatusReasonUnauthorized,
		metav1.StatusReasonTimeout, metav1.StatusReasonServerTimeout,
		metav1.StatusReasonServiceUnavailable, metav1.StatusReasonTooManyRequests,
		metav1.StatusReasonConflict, metav1.StatusReasonAlreadyExists,
		metav1.StatusReasonNotFound, metav1.StatusReasonBadRequest, metav1.StatusReasonInvalid:
		return reason
	default:
		return metav1.StatusReasonUnknown
	}
}
