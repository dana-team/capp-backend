package authorization

import (
	"errors"
	"slices"

	"k8s.io/apimachinery/pkg/api/validation"
)

// Authorization errors deliberately omit policy contents and target existence.
var (
	ErrDenied             = errors.New("operation is not permitted")
	ErrUnavailable        = errors.New("authorization policy is unavailable")
	ErrProtectedNamespace = errors.New("namespace is not accessible")
)

// Authorize checks an operation's allowed roles against one coherent observation.
// Identity must be authenticated and roles must come from the server's API rule.
// The returned state identifies the policy and namespace used for this decision.
func (s *Store) Authorize(namespace, username string, groups, allowedRoles []string) (NamespaceState, error) {
	if username == "" {
		return NamespaceState{}, ErrDenied
	}
	if err := s.CheckTarget(namespace); err != nil {
		return NamespaceState{}, err
	}
	state := s.Lookup(namespace)
	if state.Reason == CacheNotReady {
		return state, ErrUnavailable
	}
	if !state.Allows(username, groups, allowedRoles) {
		return state, ErrDenied
	}
	if state.Reason == AssociationPending {
		return state, ErrUnavailable
	}
	if !state.Ready() {
		return state, ErrDenied
	}
	return state, nil
}

// CheckTarget validates a namespace and its protected exclusions without requiring
// synchronized observations. It does not grant resource or operator authority.
func (s *Store) CheckTarget(namespace string) error {
	if len(validation.ValidateNamespaceName(namespace, false)) != 0 {
		return ErrDenied
	}
	if namespace == s.options.PolicyNamespace || slices.Contains(s.options.ProtectedNamespaces, namespace) {
		return ErrProtectedNamespace
	}
	return nil
}
