package desiredstate_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/desiredstate"
	"github.com/dana-team/capp-backend/internal/desiredstate/desiredstatetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mongoURIEnv points the contract test at a real server. CI sets it.
const mongoURIEnv = "CAPP_TEST_MONGO_URI"

func TestMemoryStore(t *testing.T) {
	testStore(t, desiredstatetest.NewMemoryStore())
}

func TestMongoStore(t *testing.T) {
	uri := os.Getenv(mongoURIEnv)
	if uri == "" {
		t.Skipf("%s is not set", mongoURIEnv)
	}
	store, err := desiredstate.NewMongoStore(uri, fmt.Sprintf("capp_test_%d", time.Now().UnixNano()), 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, store.Drop(ctx))
		assert.NoError(t, store.Close(ctx))
	})
	require.NoError(t, store.EnsureIndexes(t.Context()))
	require.NoError(t, store.EnsureIndexes(t.Context()), "creating the index again is not an error")
	testStore(t, store)
}

func TestMongoStoreHidesTheURI(t *testing.T) {
	_, err := desiredstate.NewMongoStore("mongodb://user:hunter2@", "capp", time.Second)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestMongoStoreUnreachable(t *testing.T) {
	store, err := desiredstate.NewMongoStore("mongodb://127.0.0.1:1", "capp", 200*time.Millisecond)
	require.NoError(t, err, "connecting is lazy")
	start := time.Now()
	_, err = store.Get(t.Context(), desiredstate.NamespaceRef("c1", "a"))
	require.Error(t, err)
	require.NotErrorIs(t, err, desiredstate.ErrNotFound)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func record(ref desiredstate.Ref, generation int64, json string) *desiredstate.Record {
	return &desiredstate.Record{
		ID: ref.ID(), Cluster: ref.Cluster, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name,
		Desired: desiredstate.Desired{
			ProjectorVersion: 1, JSON: json,
			SecretData: []desiredstate.SealedValue{{Key: "tls.crt", KeyID: "k1", Nonce: []byte{1}, Ciphertext: []byte{0, 0xff}}},
		},
		Generation: generation, Source: desiredstate.SourceWrite, UpdatedBy: "alice",
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func testStore(t *testing.T, store desiredstate.Store) {
	ctx := t.Context()
	require.NoError(t, store.Ping(ctx))
	capp := desiredstate.Ref{Cluster: "c1", Kind: desiredstate.KindCapp, Namespace: "team-a", Name: "app"}

	_, err := store.Get(ctx, capp)
	require.ErrorIs(t, err, desiredstate.ErrNotFound)

	require.NoError(t, store.Save(ctx, record(capp, 1, `{"a.b":1,"$c":2}`), 0))
	require.ErrorIs(t, store.Save(ctx, record(capp, 1, `{}`), 0), desiredstate.ErrConflict, "insert over an existing record")

	got, err := store.Get(ctx, capp)
	require.NoError(t, err)
	got.UpdatedAt = got.UpdatedAt.UTC()
	assert.Equal(t, record(capp, 1, `{"a.b":1,"$c":2}`), got)

	require.NoError(t, store.Save(ctx, record(capp, 2, `{"v":2}`), 1))
	require.ErrorIs(t, store.Save(ctx, record(capp, 2, `{"v":3}`), 1), desiredstate.ErrConflict, "stale generation")

	archivedAt := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	archived := record(capp, 3, `{"v":2}`)
	archived.ArchivedAt = &archivedAt
	require.NoError(t, store.Save(ctx, archived, 2))
	got, err = store.Get(ctx, capp)
	require.NoError(t, err)
	assert.False(t, got.Active())

	for _, ref := range []desiredstate.Ref{
		{Cluster: "c1", Kind: desiredstate.KindCapp, Namespace: "team-b", Name: "app"},
		{Cluster: "c1", Kind: desiredstate.KindSecret, Namespace: "team-a", Name: "app"},
		{Cluster: "c2", Kind: desiredstate.KindCapp, Namespace: "team-a", Name: "app"},
		desiredstate.NamespaceRef("c1", "team-a"),
	} {
		require.NoError(t, store.Save(ctx, record(ref, 1, `{}`), 0))
	}
	list := func(filter desiredstate.Filter) []string {
		records, err := store.List(ctx, filter)
		require.NoError(t, err)
		ids := make([]string, 0, len(records))
		for i := range records {
			ids = append(ids, records[i].ID)
		}
		return ids
	}
	assert.ElementsMatch(t, []string{"c1/Capp/team-a/app", "c1/Capp/team-b/app"}, list(desiredstate.Filter{Cluster: "c1", Kind: desiredstate.KindCapp}))
	assert.Equal(t, []string{"c1/Capp/team-b/app"}, list(desiredstate.Filter{Cluster: "c1", Kind: desiredstate.KindCapp, Namespaces: []string{"team-b"}}))
	assert.Empty(t, list(desiredstate.Filter{Cluster: "c1", Kind: desiredstate.KindCapp, Namespaces: []string{}}), "an empty filter grants nothing")

	require.ErrorIs(t, store.Delete(ctx, capp, 1), desiredstate.ErrConflict)
	require.NoError(t, store.Delete(ctx, capp, 3))
	require.NoError(t, store.Delete(ctx, capp, 3), "deleting twice is not an error")

	require.NoError(t, store.DeleteNamespace(ctx, "c1", "team-a"))
	_, err = store.Get(ctx, desiredstate.NamespaceRef("c1", "team-a"))
	require.ErrorIs(t, err, desiredstate.ErrNotFound)
	assert.Equal(t, []string{"c2/Capp/team-a/app"}, list(desiredstate.Filter{Cluster: "c2", Kind: desiredstate.KindCapp}), "other clusters are untouched")
	assert.Equal(t, []string{"c1/Capp/team-b/app"}, list(desiredstate.Filter{Cluster: "c1", Kind: desiredstate.KindCapp}), "other namespaces are untouched")
}
