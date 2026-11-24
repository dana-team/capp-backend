package desiredstate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStore counts the calls that reach the store.
type countingStore struct {
	*desiredstatetest.MemoryStore
	calls int
}

func (s *countingStore) Ping(ctx context.Context) error {
	s.calls++
	return s.MemoryStore.Ping(ctx)
}

func TestCooldown(t *testing.T) {
	const cooldown = time.Minute
	store := &countingStore{MemoryStore: desiredstatetest.NewMemoryStore()}
	now := time.Now()
	s := desiredstate.WithCooldownAt(store, cooldown, func() time.Time { return now })
	ping := func() error { return s.Ping(t.Context()) }
	ref := desiredstate.Ref{Cluster: "c", Kind: desiredstate.KindCapp, Namespace: "n", Name: "x"}

	// Answers of a working store do not start a cooldown.
	_, err := s.Get(t.Context(), ref)
	require.ErrorIs(t, err, desiredstate.ErrNotFound)
	require.ErrorIs(t, s.Save(t.Context(), &desiredstate.Record{ID: ref.ID()}, 5), desiredstate.ErrConflict)
	store.Err = errors.New("one broken record")
	require.ErrorIs(t, ping(), store.Err)
	store.Err = context.Canceled
	require.ErrorIs(t, ping(), context.Canceled)
	store.Err = nil
	require.NoError(t, ping())

	store.Err = context.DeadlineExceeded
	require.ErrorIs(t, ping(), context.DeadlineExceeded)
	store.calls = 0
	for range 3 {
		require.Error(t, ping())
	}
	_, err = s.List(t.Context(), desiredstate.Filter{})
	require.Error(t, err)
	require.Error(t, s.Delete(t.Context(), ref, 1))
	require.Error(t, s.DeleteNamespace(t.Context(), "c", "n"))
	assert.Zero(t, store.calls, "the store is left alone")

	// After the cooldown one call tries again, and a failure starts it over.
	now = now.Add(cooldown)
	require.ErrorIs(t, ping(), context.DeadlineExceeded)
	require.Error(t, ping())
	assert.Equal(t, 1, store.calls)

	store.Err = nil
	require.Error(t, ping())
	now = now.Add(cooldown)
	require.NoError(t, ping())
	require.NoError(t, ping())
	assert.Equal(t, 3, store.calls)
}

func TestCooldownTriesOnce(t *testing.T) {
	store := desiredstatetest.NewMemoryStore()
	now := time.Now()
	s := desiredstate.WithCooldownAt(store, time.Minute, func() time.Time { return now })
	store.Err = context.DeadlineExceeded
	require.Error(t, s.Ping(t.Context()))

	now = now.Add(time.Minute)
	store.Err = nil
	store.BeforeSave = func() {
		require.Error(t, s.Ping(t.Context()), "a second call does not wait for the one that tries the store")
	}
	ref := desiredstate.Ref{Cluster: "c", Kind: desiredstate.KindCapp, Namespace: "n", Name: "x"}
	require.NoError(t, s.Save(t.Context(), &desiredstate.Record{ID: ref.ID(), Generation: 1}, 0))
	require.NoError(t, s.Ping(t.Context()))
}
