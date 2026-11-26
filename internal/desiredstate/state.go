package desiredstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/resources/consts"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// droppedAnnotations are written by the operator or by the backend as a side
// effect. They are not desired state, and a revert leaves them alone.
// The reload opt-in is also listed: it is projected as its own field.
var droppedAnnotations = map[string]struct{}{
	consts.CappAPIGroup + "/last-updated-by": {},
	consts.CappAPIGroup + "/skip-dns-check":  {},
	k8s.AnnotationRollout:                    {},
	consts.BoundResourceVersionsAnnotation:   {},
	consts.RestartedAtAnnotation:             {},
	consts.RestartReasonAnnotation:           {},
	consts.BoundResourceRolloutAnnotation:    {},
}

// kubectlAnnotationPrefix covers last-applied-configuration, which would also
// copy Secret values in plain text.
const kubectlAnnotationPrefix = "kubectl.kubernetes.io/"

type metadataState struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type cappState struct {
	Metadata                    metadataState         `json:"metadata"`
	ReloadOnBoundResourceChange bool                  `json:"reloadOnBoundResourceChange"`
	Spec                        cappv1alpha1.CappSpec `json:"spec"`
}

type configMapState struct {
	Metadata   metadataState     `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
	BinaryData map[string][]byte `json:"binaryData,omitempty"`
	Immutable  *bool             `json:"immutable,omitempty"`
}

// secretState has no data: Secret values are kept apart and stored encrypted.
type secretState struct {
	Metadata  metadataState     `json:"metadata"`
	Type      corev1.SecretType `json:"type,omitempty"`
	Immutable *bool             `json:"immutable,omitempty"`
}

// NamespaceState is what the API manages of a namespace. Users is used in
// Kubernetes authorization mode and Bindings in backend-policy mode.
type NamespaceState struct {
	Quota    *QuotaState    `json:"quota,omitempty"`
	Users    []string       `json:"users,omitempty"`
	Bindings []BindingState `json:"bindings,omitempty"`
}

// QuotaState holds the limits of a namespace's ResourceQuota.
type QuotaState struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   int    `json:"pods,omitempty"`
}

// BindingState is one backend-policy grant.
type BindingState struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// State is the projection of one object.
type State struct {
	json   string
	tree   map[string]any
	secret map[string][]byte
}

// StateOf projects a Capp, ConfigMap, Secret or NamespaceState.
func StateOf(obj any) (State, error) {
	var projected any
	var secret map[string][]byte
	switch o := obj.(type) {
	case *cappv1alpha1.Capp:
		projected = cappState{
			Metadata:                    metadataOf(o.Labels, o.Annotations),
			ReloadOnBoundResourceChange: o.Annotations[consts.BoundResourceRolloutAnnotation] == consts.BoundResourceRolloutEnabled,
			Spec:                        o.Spec,
		}
	case *corev1.ConfigMap:
		projected = configMapState{Metadata: metadataOf(o.Labels, o.Annotations), Data: o.Data, BinaryData: o.BinaryData, Immutable: o.Immutable}
	case *corev1.Secret:
		projected = secretState{Metadata: metadataOf(o.Labels, o.Annotations), Type: o.Type, Immutable: o.Immutable}
		secret = o.Data
	case NamespaceState:
		projected = o.normalized()
	default:
		return State{}, fmt.Errorf("objects of type %T have no desired state", obj)
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return State{}, fmt.Errorf("encoding desired state: %w", err)
	}
	return decodeState(string(encoded), secret)
}

// Open returns the desired state of a record.
func Open(record *Record, sealer *Sealer) (State, error) {
	secret, err := sealer.Open(record.Ref(), record.Desired.SecretData)
	if err != nil {
		return State{}, err
	}
	return decodeState(record.Desired.JSON, secret)
}

// Seal returns the stored form of s.
func (s State) Seal(ref Ref, sealer *Sealer) (Desired, error) {
	sealed, err := sealer.Seal(ref, s.secret)
	if err != nil {
		return Desired{}, err
	}
	return Desired{ProjectorVersion: projectorVersion, JSON: s.json, SecretData: sealed}, nil
}

// Equal reports whether two states store the same desired state.
func (s State) Equal(other State) bool {
	return s.json == other.json && maps.EqualFunc(s.secret, other.secret, bytes.Equal)
}

// Version returns an opaque token that changes whenever s does.
func (s State) Version(sealer *Sealer) string {
	payload := []byte(s.json)
	for _, key := range slices.Sorted(maps.Keys(s.secret)) {
		// Lengths keep neighbouring keys and values from running together.
		payload = fmt.Appendf(payload, "\x00%d:%s\x00%d:", len(key), key, len(s.secret[key]))
		payload = append(payload, s.secret[key]...)
	}
	return sealer.version(payload)
}

// Namespace returns the projected namespace.
func (s State) Namespace() (NamespaceState, error) {
	var ns NamespaceState
	if err := json.Unmarshal([]byte(s.json), &ns); err != nil {
		return NamespaceState{}, fmt.Errorf("decoding desired state: %w", err)
	}
	return ns, nil
}

// Apply makes the projected fields of obj equal to s. Fields outside the
// projection, and labels and annotations s does not name, are left as they
// are.
func (s State) Apply(obj any) error {
	var err error
	switch o := obj.(type) {
	case *cappv1alpha1.Capp:
		var desired cappState
		if err = json.Unmarshal([]byte(s.json), &desired); err == nil {
			o.Labels = overlay(o.Labels, desired.Metadata.Labels)
			o.Annotations = overlay(o.Annotations, desired.Metadata.Annotations)
			if desired.ReloadOnBoundResourceChange {
				o.Annotations = overlay(o.Annotations, map[string]string{consts.BoundResourceRolloutAnnotation: consts.BoundResourceRolloutEnabled})
			} else {
				delete(o.Annotations, consts.BoundResourceRolloutAnnotation)
			}
			o.Spec = desired.Spec
		}
	case *corev1.ConfigMap:
		var desired configMapState
		if err = json.Unmarshal([]byte(s.json), &desired); err == nil {
			o.Labels = overlay(o.Labels, desired.Metadata.Labels)
			o.Annotations = overlay(o.Annotations, desired.Metadata.Annotations)
			o.Data, o.BinaryData, o.Immutable = desired.Data, desired.BinaryData, desired.Immutable
		}
	case *corev1.Secret:
		var desired secretState
		if err = json.Unmarshal([]byte(s.json), &desired); err == nil {
			o.Labels = overlay(o.Labels, desired.Metadata.Labels)
			o.Annotations = overlay(o.Annotations, desired.Metadata.Annotations)
			o.Type, o.Immutable = desired.Type, desired.Immutable
			o.Data, o.StringData = maps.Clone(s.secret), nil
		}
	default:
		return fmt.Errorf("objects of type %T have no desired state", obj)
	}
	if err != nil {
		return fmt.Errorf("decoding desired state: %w", err)
	}
	return nil
}

func decodeState(encoded string, secret map[string][]byte) (State, error) {
	// Numbers stay text, so comparing them is exact.
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var tree map[string]any
	if err := decoder.Decode(&tree); err != nil {
		return State{}, fmt.Errorf("decoding desired state: %w", err)
	}
	return State{json: encoded, tree: tree, secret: secret}, nil
}

func metadataOf(labels, annotations map[string]string) metadataState {
	state := metadataState{Labels: maps.Clone(labels), Annotations: maps.Clone(annotations)}
	// A leftover of the removed Git sync. Cleaning it up is not drift.
	delete(state.Labels, k8s.LabelBackupToGit)
	maps.DeleteFunc(state.Annotations, func(key, _ string) bool {
		_, dropped := droppedAnnotations[key]
		return dropped || strings.HasPrefix(key, kubectlAnnotationPrefix)
	})
	return state
}

func overlay(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

// normalized orders members and drops an empty quota, so that states which
// mean the same are stored the same.
func (n NamespaceState) normalized() NamespaceState {
	n.Users = slices.Compact(slices.Sorted(slices.Values(n.Users)))
	n.Bindings = slices.SortedFunc(slices.Values(n.Bindings), func(a, b BindingState) int {
		return strings.Compare(a.Kind+"\x00"+a.Name+"\x00"+a.Role, b.Kind+"\x00"+b.Name+"\x00"+b.Role)
	})
	n.Bindings = slices.Compact(n.Bindings)
	if n.Quota != nil && *n.Quota == (QuotaState{}) {
		n.Quota = nil
	}
	return n
}
