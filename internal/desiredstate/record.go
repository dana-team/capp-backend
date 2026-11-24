// Package desiredstate records the desired state of the Kubernetes objects the
// backend manages and compares it with the live objects.
//
// A record holds a projection of the object: the fields users own, without
// status, server metadata and the fields the operator writes. The same
// projection is taken of the live object before comparing, so a comparison is
// always projection against projection.
package desiredstate

import (
	"strings"
	"time"
)

// Kind is the kind of object a record describes.
type Kind string

const (
	KindCapp      Kind = "Capp"
	KindConfigMap Kind = "ConfigMap"
	KindSecret    Kind = "Secret"
	KindNamespace Kind = "Namespace"
)

// Source tells how the current desired state was recorded.
type Source string

const (
	// SourceWrite marks a state written through the API.
	SourceWrite Source = "write"
	// SourceAccept marks a state taken over from the cluster.
	SourceAccept Source = "accept"
)

// projectorVersion is stored with every record. Bump it when a projection
// changes in a way that stored records must be told apart.
const projectorVersion = 1

// Ref identifies one object. A Namespace uses its own name as Namespace too,
// so one filter finds everything that belongs to a namespace.
type Ref struct {
	Cluster   string
	Kind      Kind
	Namespace string
	Name      string
}

// NamespaceRef returns the Ref of a namespace.
func NamespaceRef(cluster, name string) Ref {
	return Ref{Cluster: cluster, Kind: KindNamespace, Namespace: name, Name: name}
}

// ID returns the record's key. No part can contain a slash: cluster names are
// URL path segments, and Kubernetes forbids it in the others.
func (r Ref) ID() string {
	return strings.Join([]string{r.Cluster, string(r.Kind), r.Namespace, r.Name}, "/")
}

// Record is one stored desired state.
type Record struct {
	ID        string `bson:"_id"`
	Cluster   string `bson:"cluster"`
	Kind      Kind   `bson:"kind"`
	Namespace string `bson:"namespace"`
	Name      string `bson:"name"`

	Desired Desired `bson:"desired"`

	// Generation is incremented by every write and guards concurrent writes.
	Generation int64 `bson:"generation"`

	// ArchivedAt is set when a user accepted that the object is gone. The
	// desired state is kept, but no drift is reported for it.
	ArchivedAt *time.Time `bson:"archivedAt,omitempty"`

	Source    Source    `bson:"source"`
	UpdatedBy string    `bson:"updatedBy,omitempty"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

// Ref returns the identity of the recorded object.
func (r *Record) Ref() Ref {
	return Ref{Cluster: r.Cluster, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
}

// Active reports whether the record takes part in drift detection.
func (r *Record) Active() bool { return r != nil && r.ArchivedAt == nil }

// Desired is the stored projection. It is JSON text rather than a BSON
// document: BSON would lose resource.Quantity values, which keep their amount
// in unexported fields, and keys such as "tls.crt" contain dots.
type Desired struct {
	ProjectorVersion int           `bson:"projectorVersion"`
	JSON             string        `bson:"json"`
	SecretData       []SealedValue `bson:"secretData,omitempty"`
}

// SealedValue is one encrypted Secret value.
type SealedValue struct {
	Key        string `bson:"key"`
	KeyID      string `bson:"keyId"`
	Nonce      []byte `bson:"nonce"`
	Ciphertext []byte `bson:"ciphertext"`
}
