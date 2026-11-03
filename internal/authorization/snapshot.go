package authorization

import (
	"slices"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Reason identifies an observed readiness condition without exposing policy data.
type Reason string

// Readiness reasons describe observed prerequisites, not Kubernetes status fields.
const (
	Ready                Reason = "Ready"
	CacheNotReady        Reason = "CacheNotReady"
	PolicyInvalid        Reason = "PolicyInvalid"
	NamespaceUnavailable Reason = "NamespaceUnavailable"
	NamespaceUnmanaged   Reason = "NamespaceUnmanaged"
	NamespaceUIDMismatch Reason = "NamespaceUIDMismatch"
	AssociationPending   Reason = "AssociationPending"
)

var namespaceUIDAnnotation = cappv1alpha1.GroupVersion.Group + "/namespace-uid"

// NamespaceState is a detached observation; callers may modify it safely.
// Assignment matching and readiness are separate so callers can order denials.
type NamespaceState struct {
	Namespace             *corev1.Namespace
	PolicyUID             types.UID
	PolicyResourceVersion string
	Reason                Reason
	assignment            *Assignment
}

// Allows checks assigned roles independently of namespace readiness.
func (s NamespaceState) Allows(username string, groups, allowedRoles []string) bool {
	return s.assignment.Allows(username, groups, allowedRoles)
}

// Ready reports whether all observed readiness prerequisites hold.
func (s NamespaceState) Ready() bool {
	return s.Reason == Ready
}

func (s NamespaceState) clone() NamespaceState {
	s.Namespace = s.Namespace.DeepCopy()
	if s.assignment != nil {
		a := *s.assignment
		a.bindings = slices.Clone(a.bindings)
		s.assignment = &a
	}
	return s
}

func (s NamespaceState) condition(reason Reason) NamespaceState {
	s.Reason = reason
	return s
}
