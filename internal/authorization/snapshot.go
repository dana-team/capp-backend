package authorization

import (
	"slices"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Reason identifies an observed readiness condition without exposing policy data.
type Reason string

const (
	saRBAC          = "sa-rbac"
	executionSAName = "capp-exec"
)

// Readiness reasons describe observed prerequisites, not Kubernetes status fields.
const (
	Ready                Reason = "Ready"
	CacheNotReady        Reason = "CacheNotReady"
	PolicyInvalid        Reason = "PolicyInvalid"
	NamespaceUnavailable Reason = "NamespaceUnavailable"
	NamespaceUnmanaged   Reason = "NamespaceUnmanaged"
	NamespaceUIDMismatch Reason = "NamespaceUIDMismatch"
	ProvisioningPending  Reason = "ProvisioningPending"
	OwnershipConflict    Reason = "OwnershipConflict"
	ManagedObjectDrift   Reason = "ManagedObjectDrift"
)

var (
	managedByLabel            = cappv1alpha1.GroupVersion.Group + "/managed-by"
	policyUIDAnnotation       = cappv1alpha1.GroupVersion.Group + "/policy-uid"
	namespaceUIDAnnotation    = cappv1alpha1.GroupVersion.Group + "/namespace-uid"
	targetNamespaceAnnotation = cappv1alpha1.GroupVersion.Group + "/target-namespace"
)

// NamespaceState is a detached observation; callers may modify it safely.
// Assignment matching and readiness are separate so callers can order denials.
type NamespaceState struct {
	Namespace     *corev1.Namespace
	PolicyUID     types.UID
	PolicyVersion string
	Reason        Reason
	assignment    *Assignment
}

// Matches checks the assignment independently of readiness.
func (s NamespaceState) Matches(username string, groups []string) bool {
	return s.assignment.Matches(username, groups)
}

// Ready reports whether all observed readiness prerequisites hold.
func (s NamespaceState) Ready() bool {
	return s.Reason == Ready
}

// Message describes the observed reason without exposing policy data.
func (s NamespaceState) Message() string {
	messages := map[Reason]string{
		Ready:                "Required assignment, namespace, and binding state verified",
		CacheNotReady:        "Initial authorization observation incomplete",
		PolicyInvalid:        "Assignment is invalid or absent",
		NamespaceUnavailable: "Target namespace is absent or terminating",
		NamespaceUnmanaged:   "Target namespace is not labeled as managed",
		NamespaceUIDMismatch: "Policy is associated with another namespace instance",
		ProvisioningPending:  "Managed namespace binding is not established",
		OwnershipConflict:    "Expected managed object name is occupied by another owner",
		ManagedObjectDrift:   "Observed namespace binding differs from the intended state",
	}
	return messages[s.Reason]
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

func owned(object metav1.Object, policy *corev1.ConfigMap, ns *corev1.Namespace) bool {
	annotations := object.GetAnnotations()
	return object.GetLabels()[managedByLabel] == saRBAC &&
		annotations[policyUIDAnnotation] == string(policy.UID) &&
		annotations[namespaceUIDAnnotation] == string(ns.UID) &&
		annotations[targetNamespaceAnnotation] == ns.Name
}

func managedState(policy *corev1.ConfigMap, ns *corev1.Namespace, rb *rbacv1.RoleBinding) Reason {
	if rb == nil {
		return ProvisioningPending
	}
	if rb.Namespace != ns.Name || rb.Name != executionSAName+"-"+string(policy.UID) || !owned(rb, policy, ns) {
		return OwnershipConflict
	}
	if rb.DeletionTimestamp != nil || rb.UID == "" {
		return ProvisioningPending
	}
	expectedRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cappUserRole}
	expectedSubject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: policy.Namespace, Name: executionSAName}
	if rb.RoleRef != expectedRef || len(rb.Subjects) != 1 || rb.Subjects[0] != expectedSubject {
		return ManagedObjectDrift
	}
	return Ready
}
