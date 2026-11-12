package authorization

import (
	"slices"

	"github.com/dana-team/capp-backend/internal/resources/consts"
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

// NamespaceUIDAnnotation binds a policy to one namespace instance.
const NamespaceUIDAnnotation = consts.CappAPIGroup + "/namespace-uid"

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

// Bindings returns a detached copy of the observed policy membership.
func (s NamespaceState) Bindings() []Binding {
	if s.assignment == nil {
		return nil
	}
	return s.assignment.Bindings()
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
