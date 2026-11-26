package desiredstate

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Cooldown is how long an unreachable store is left alone.
const Cooldown = 10 * time.Second

// errCoolingDown is returned without contacting the store.
var errCoolingDown = errors.New("the desired state store was unreachable moments ago")

// WithCooldown returns a store that fails at once for a while after store could
// not be reached. Requests do not depend on the store, so they must not all
// wait for its timeout during an outage. When the cooldown ends, one call tries
// the store again while the others keep failing.
func WithCooldown(store Store, cooldown time.Duration) Store {
	return &cooldownStore{store: store, cooldown: cooldown, now: time.Now}
}

type cooldownStore struct {
	store    Store
	cooldown time.Duration
	now      func() time.Time

	mu sync.Mutex
	// until is zero while the store is reachable.
	until time.Time
}

func (s *cooldownStore) call(operation func() error) error {
	s.mu.Lock()
	if now := s.now(); now.Before(s.until) {
		s.mu.Unlock()
		return errCoolingDown
	} else if !s.until.IsZero() {
		s.until = now.Add(s.cooldown)
	}
	s.mu.Unlock()

	err := operation()
	// A caller that gave up says nothing about the store.
	if errors.Is(err, context.Canceled) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until = time.Time{}
	if mongo.IsTimeout(err) || mongo.IsNetworkError(err) {
		s.until = s.now().Add(s.cooldown)
	}
	return err
}

// Get implements Store.
func (s *cooldownStore) Get(ctx context.Context, ref Ref) (record *Record, err error) {
	err = s.call(func() error {
		record, err = s.store.Get(ctx, ref)
		return err
	})
	return record, err
}

// List implements Store.
func (s *cooldownStore) List(ctx context.Context, filter Filter) (records []Record, err error) {
	err = s.call(func() error {
		records, err = s.store.List(ctx, filter)
		return err
	})
	return records, err
}

// Save implements Store.
func (s *cooldownStore) Save(ctx context.Context, record *Record, expected int64) error {
	return s.call(func() error { return s.store.Save(ctx, record, expected) })
}

// Delete implements Store.
func (s *cooldownStore) Delete(ctx context.Context, ref Ref, expected int64) error {
	return s.call(func() error { return s.store.Delete(ctx, ref, expected) })
}

// DeleteNamespace implements Store.
func (s *cooldownStore) DeleteNamespace(ctx context.Context, cluster, namespace string) error {
	return s.call(func() error { return s.store.DeleteNamespace(ctx, cluster, namespace) })
}

// Ping implements Store.
func (s *cooldownStore) Ping(ctx context.Context) error {
	return s.call(func() error { return s.store.Ping(ctx) })
}
