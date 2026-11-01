// Package authorization validates namespace assignments for backend execution.
package authorization

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/json"
)

const (
	policyDataKey        = "policy.json"
	maxPolicyBytes       = 512 * 1024
	assignmentNamePrefix = "capp-assignment-"
	backendPolicy        = "backend-policy"
)

const (
	// RoleViewer grants access to APIs that allow the viewer role.
	RoleViewer = "viewer"
	// RoleEditor grants access to APIs that allow the editor role.
	RoleEditor = "editor"
)

var assignmentLabel = cappv1alpha1.GroupVersion.Group + "/authorization"

// Policy is the JSON assignment document. Decoding it does not validate it or
// grant access; ParseAssignment validates the document and its ConfigMap.
type Policy struct {
	Version   int       `json:"version"`
	Namespace string    `json:"namespace"`
	Bindings  []Binding `json:"bindings"`
}

// Binding assigns an application role to a named user or group.
// Names must be nonblank, without surrounding whitespace or control characters.
type Binding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Assignment is a validated namespace assignment. Its zero value grants nothing.
type Assignment struct {
	Namespace string
	bindings  []Binding // Sorted by kind, name, and role after validation.
}

// ParseAssignment validates an operator ConfigMap without mutating it or calling
// Kubernetes. policyNamespace is always protected in addition to protectedNamespaces.
// restrictedBindingNamePrefixes applies to User and Group names, case-sensitively.
// A nil or empty prefix list imposes no restrictions; empty-string entries are invalid.
// No defaults are supplied here.
// Errors deliberately omit policy contents, including unknown field names.
// Binding validation errors identify the zero-based index in the input array.
func ParseAssignment(cm *corev1.ConfigMap, policyNamespace string, protectedNamespaces, restrictedBindingNamePrefixes []string) (*Assignment, error) {
	if len(validation.ValidateNamespaceName(policyNamespace, false)) != 0 {
		return nil, errors.New("invalid policy namespace")
	}
	if cm == nil || cm.Namespace != policyNamespace || cm.Labels[assignmentLabel] != backendPolicy {
		return nil, errors.New("assignment must be selected in the policy namespace")
	}
	payload, ok := cm.Data[policyDataKey]
	if !ok || len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
		return nil, errors.New("assignment must contain only policy.json data")
	}
	if len(payload) > maxPolicyBytes {
		return nil, errors.New("assignment payload exceeds 512 KiB")
	}
	var policy Policy
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
	if err := validateBindings(policy.Bindings, restrictedBindingNamePrefixes); err != nil {
		return nil, err
	}
	slices.SortFunc(policy.Bindings, compareBindings)
	return &Assignment{Namespace: policy.Namespace, bindings: policy.Bindings}, nil
}

// Allows reports whether an assignment matching the verified username or groups
// carries one of allowedRoles. Names and roles are exact and case-sensitive;
// roles have no implicit inheritance. Nil assignments, empty usernames, and empty
// allowedRoles deny.
// Callers must separately authenticate the identity and check target eligibility.
func (a *Assignment) Allows(username string, groups, allowedRoles []string) bool {
	if a == nil || username == "" {
		return false
	}
	for _, role := range allowedRoles {
		if _, found := slices.BinarySearchFunc(a.bindings, Binding{Kind: rbacv1.UserKind, Name: username, Role: role}, compareBindings); found {
			return true
		}
		for _, group := range groups {
			if _, found := slices.BinarySearchFunc(a.bindings, Binding{Kind: rbacv1.GroupKind, Name: group, Role: role}, compareBindings); found {
				return true
			}
		}
	}
	return false
}

// ValidateBindingNamePrefixes rejects empty prefixes, which would match every name.
// Configuration loading must separately require an explicit list; nil and empty
// lists are unrestricted here, with no compiled-in defaults.
func ValidateBindingNamePrefixes(prefixes []string) error {
	if slices.Contains(prefixes, "") {
		return errors.New("restricted binding name prefixes must not contain an empty prefix")
	}
	return nil
}

func validateBindings(bindings []Binding, restrictedBindingNamePrefixes []string) error {
	if err := ValidateBindingNamePrefixes(restrictedBindingNamePrefixes); err != nil {
		return err
	}
	seen := make(map[Binding]struct{}, len(bindings))
	for i, b := range bindings {
		if (b.Kind != rbacv1.UserKind && b.Kind != rbacv1.GroupKind) || (b.Role != RoleViewer && b.Role != RoleEditor) || strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("bindings[%d]: binding requires User or Group, a nonblank name, and viewer or editor role", i)
		}
		if strings.TrimSpace(b.Name) != b.Name || strings.ContainsFunc(b.Name, unicode.IsControl) {
			return fmt.Errorf("bindings[%d]: binding name must not contain surrounding whitespace or control characters", i)
		}
		for _, prefix := range restrictedBindingNamePrefixes {
			if strings.HasPrefix(b.Name, prefix) {
				return fmt.Errorf("bindings[%d]: binding name uses a restricted prefix", i)
			}
		}
		if _, ok := seen[b]; ok {
			return fmt.Errorf("bindings[%d]: assignment contains duplicate bindings", i)
		}
		seen[b] = struct{}{}
	}
	return nil
}

func compareBindings(a, b Binding) int {
	if order := strings.Compare(a.Kind, b.Kind); order != 0 {
		return order
	}
	if order := strings.Compare(a.Name, b.Name); order != 0 {
		return order
	}
	return strings.Compare(a.Role, b.Role)
}
