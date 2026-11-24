// Package desiredstatetest provides a desired state store for tests.
package desiredstatetest

import (
	"context"
	"slices"
	"sync"

	"github.com/dana-team/capp-backend/internal/desiredstate"
)

// MemoryStore keeps records in memory. Set Err to make every call fail, or
// SaveErr to make only writes fail. BeforeSave runs at the start of a Save.
type MemoryStore struct {
	Err        error
	SaveErr    error
	BeforeSave func()

	mu      sync.Mutex
	records map[string]desiredstate.Record
}

var _ desiredstate.Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]desiredstate.Record{}}
}

// Len returns the number of records.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *MemoryStore) Get(ctx context.Context, ref desiredstate.Ref) (*desiredstate.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(ctx); err != nil {
		return nil, err
	}
	record, ok := s.records[ref.ID()]
	if !ok {
		return nil, desiredstate.ErrNotFound
	}
	return &record, nil
}

func (s *MemoryStore) List(ctx context.Context, filter desiredstate.Filter) ([]desiredstate.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(ctx); err != nil {
		return nil, err
	}
	var records []desiredstate.Record
	for _, record := range s.records {
		if record.Cluster == filter.Cluster && record.Kind == filter.Kind &&
			(filter.Namespaces == nil || slices.Contains(filter.Namespaces, record.Namespace)) {
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *MemoryStore) Save(ctx context.Context, record *desiredstate.Record, expected int64) error {
	if s.BeforeSave != nil {
		s.BeforeSave()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(ctx); err != nil {
		return err
	}
	if s.SaveErr != nil {
		return s.SaveErr
	}
	if s.records[record.ID].Generation != expected {
		return desiredstate.ErrConflict
	}
	s.records[record.ID] = *record
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, ref desiredstate.Ref, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(ctx); err != nil {
		return err
	}
	if s.SaveErr != nil {
		return s.SaveErr
	}
	record, ok := s.records[ref.ID()]
	if !ok {
		return nil
	}
	if record.Generation != expected {
		return desiredstate.ErrConflict
	}
	delete(s.records, ref.ID())
	return nil
}

func (s *MemoryStore) DeleteNamespace(ctx context.Context, cluster, namespace string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(ctx); err != nil {
		return err
	}
	for id, record := range s.records {
		if record.Cluster == cluster && record.Namespace == namespace {
			delete(s.records, id)
		}
	}
	return nil
}

func (s *MemoryStore) Ping(ctx context.Context) error { return s.failure(ctx) }

// failure makes the store fail like a server: also when the context ended.
func (s *MemoryStore) failure(ctx context.Context) error {
	if s.Err != nil {
		return s.Err
	}
	return ctx.Err()
}
