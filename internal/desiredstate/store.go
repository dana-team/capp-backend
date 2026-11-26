package desiredstate

import (
	"context"
	"errors"
)

var (
	// ErrNotFound is returned when no record exists for a Ref.
	ErrNotFound = errors.New("desired state not found")

	// ErrConflict is returned when a record changed since it was read.
	ErrConflict = errors.New("desired state changed concurrently")
)

// Filter selects records for List. A nil Namespaces matches every namespace;
// an empty one matches none.
type Filter struct {
	Cluster    string
	Kind       Kind
	Namespaces []string
}

// Store persists records.
type Store interface {
	// Get returns the record of ref, or ErrNotFound.
	Get(ctx context.Context, ref Ref) (*Record, error)

	// List returns the matching records.
	List(ctx context.Context, filter Filter) ([]Record, error)

	// Save writes record if the stored generation still is expected; 0 means
	// that no record may exist yet. Otherwise it returns ErrConflict.
	Save(ctx context.Context, record *Record, expected int64) error

	// Delete removes the record of ref if its generation still is expected.
	// Otherwise it returns ErrConflict. A missing record is not an error.
	Delete(ctx context.Context, ref Ref, expected int64) error

	// DeleteNamespace removes every record of a namespace, including its own.
	DeleteNamespace(ctx context.Context, cluster, namespace string) error

	// Ping reports whether the store can be reached.
	Ping(ctx context.Context) error
}
