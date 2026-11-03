// Package authorization validates namespace assignments for backend execution.
package authorization

import (
	"errors"
	"slices"
	"strings"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/json"
)

const (
	policyDataKey        = "policy.json"
	maxPolicyBytes       = 512 * 1024
	cappUserRole         = "capp-user"
	assignmentNamePrefix = "capp-assignment-"
)

var assignmentLabel = cappv1alpha1.GroupVersion.Group + "/authorization"

// Assignment is a validated namespace assignment. Its zero value grants nothing.
type Assignment struct {
	Namespace string
	bindings  []binding
}

type binding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// ParseAssignment validates an operator ConfigMap without mutating it or calling
// Kubernetes. policyNamespace is always protected in addition to protectedNamespaces.
// Errors deliberately omit policy contents, including unknown field names.
func ParseAssignment(cm *corev1.ConfigMap, policyNamespace string, protectedNamespaces []string) (*Assignment, error) {
	if len(validation.ValidateNamespaceName(policyNamespace, false)) != 0 {
		return nil, errors.New("invalid policy namespace")
	}
	if cm == nil || cm.Namespace != policyNamespace || cm.Labels[assignmentLabel] != saRBAC {
		return nil, errors.New("assignment must be selected in the policy namespace")
	}
	payload, ok := cm.Data[policyDataKey]
	if !ok || len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
		return nil, errors.New("assignment must contain only policy.json data")
	}
	if len(payload) > maxPolicyBytes {
		return nil, errors.New("assignment payload exceeds 512 KiB")
	}
	var policy struct {
		Version   int       `json:"version"`
		Namespace string    `json:"namespace"`
		Bindings  []binding `json:"bindings"`
	}
	strictErrors, err := json.UnmarshalStrict([]byte(payload), &policy)
	if err != nil || len(strictErrors) != 0 {
		return nil, errors.New("assignment must contain strict JSON with only supported fields")
	}
	if policy.Version != 1 || policy.Bindings == nil {
		return nil, errors.New("assignment requires version 1 and a bindings array")
	}
	if len(validation.ValidateNamespaceName(policy.Namespace, false)) != 0 || cm.Name != assignmentNamePrefix+policy.Namespace {
		return nil, errors.New("assignment name must match a valid target namespace")
	}
	if policy.Namespace == policyNamespace || slices.Contains(protectedNamespaces, policy.Namespace) {
		return nil, errors.New("assignment targets a protected namespace")
	}
	if err := validateBindings(policy.Bindings); err != nil {
		return nil, err
	}
	return &Assignment{Namespace: policy.Namespace, bindings: policy.Bindings}, nil
}

// Matches checks exact, case-sensitive names from an already verified identity.
// It neither authenticates the caller nor expands group membership.
func (a *Assignment) Matches(username string, groups []string) bool {
	if a == nil {
		return false
	}
	for _, b := range a.bindings {
		if b.Kind == rbacv1.UserKind && b.Name == username {
			return true
		}
		if b.Kind == rbacv1.GroupKind && slices.Contains(groups, b.Name) {
			return true
		}
	}
	return false
}

func validateBindings(bindings []binding) error {
	seen := make(map[binding]struct{}, len(bindings))
	for _, b := range bindings {
		if (b.Kind != rbacv1.UserKind && b.Kind != rbacv1.GroupKind) || b.Role != cappUserRole || strings.TrimSpace(b.Name) == "" {
			return errors.New("binding requires User or Group, a nonblank name, and capp-user role")
		}
		if _, ok := seen[b]; ok {
			return errors.New("assignment contains duplicate bindings")
		}
		seen[b] = struct{}{}
	}
	return nil
}
