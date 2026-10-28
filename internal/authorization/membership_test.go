package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSnapshotReplaceUsers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		users     []string
		want      []string
		wantError error
		writes    int
	}{
		{name: "omitted or null users leave membership unchanged", want: []string{"alice"}},
		{name: "empty users clear membership", users: []string{}, want: []string{}, writes: 1},
		{name: "replacement preserves exact identities and sorts users", users: []string{"bob", "Alice", " alice "}, want: []string{" alice ", "Alice", "bob"}, writes: 1},
		{name: "duplicate users rejected", users: []string{"bob", "bob"}, wantError: ErrInvalidMembership},
		{name: "blank user rejected", users: []string{" "}, wantError: ErrInvalidMembership},
		{name: "invalid UTF-8 cannot change an identity", users: []string{"\xff"}, wantError: ErrInvalidMembership},
		{name: "oversized payload rejected", users: []string{strings.Repeat("a", maxPolicyRecordBytes)}, wantError: ErrInvalidMembership},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := snapshotObjects(t)
			objects[1].Annotations = map[string]string{"operator": "keep"}
			objects[1].Labels["extra"] = "keep"
			objects[1].Data["extra"] = "keep"
			writes := 0
			ctx := t.Context()
			target := snapshotClient(t, interceptor.Funcs{Patch: func(gotCtx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				writes++
				if gotCtx != ctx {
					t.Fatal("ReplaceUsers did not propagate the request context")
				}
				data, err := patch.Data(obj)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				record, ok := obj.(*corev1.ConfigMap)
				if !ok {
					t.Fatalf("patched object type = %T, want *corev1.ConfigMap", obj)
				}
				want := map[string]any{
					"metadata": map[string]any{"resourceVersion": "20"},
					"data":     map[string]any{policyDataKey: record.Data[policyDataKey]},
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("membership patch mismatch (-want +got):\n%s", diff)
				}
				return c.Patch(gotCtx, obj, patch, opts...)
			}}, objects...)
			snapshot := membershipSnapshot(t, target)
			input := slices.Clone(tc.users)
			result, err := snapshot.ReplaceUsers(ctx, target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), input)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("ReplaceUsers() error = %v, want %v", err, tc.wantError)
			}
			if writes != tc.writes {
				t.Fatalf("ReplaceUsers() writes = %d, want %d", writes, tc.writes)
			}
			if tc.wantError != nil {
				return
			}
			if diff := cmp.Diff(tc.users, input); diff != "" {
				t.Errorf("input changed (-want +got):\n%s", diff)
			}
			wantResult := tc.want
			if tc.users == nil {
				wantResult = nil
			}
			if diff := cmp.Diff(wantResult, result); diff != "" {
				t.Errorf("persisted users (-want +got):\n%s", diff)
			}
			stored := readMembership(t, target)
			membership, err := parseMembershipPolicy(stored)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, membership.Users); diff != "" {
				t.Errorf("stored users mismatch (-want +got):\n%s", diff)
			}
			original := snapshot.records.memberships[currentNamespace().UID]
			if diff := cmp.Diff(original.Bindings, membership.Bindings); diff != "" {
				t.Errorf("operator bindings changed (-want +got):\n%s", diff)
			}
			if !slices.Equal(original.Users, []string{"alice"}) || snapshot.resourceVersions[stored.Name] != "20" {
				t.Error("ReplaceUsers mutated the request snapshot")
			}
			if stored.Annotations["operator"] != "keep" || stored.Labels["extra"] != "keep" || stored.Data["extra"] != "keep" {
				t.Error("ReplaceUsers removed unrelated ConfigMap fields")
			}
		})
	}

	t.Run("new membership has namespace ownership and no managed-resource label", func(t *testing.T) {
		target := snapshotClient(t, interceptor.Funcs{}, snapshotObjects(t)[:1]...)
		snapshot := membershipSnapshot(t, target)
		if _, err := snapshot.ReplaceUsers(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), []string{"bob"}); err != nil {
			t.Fatal(err)
		}
		stored := readMembership(t, target)
		if err := validateRecord(stored, "policies"); err != nil {
			t.Fatal(err)
		}
		membership, err := parseMembershipPolicy(stored)
		if err != nil {
			t.Fatal(err)
		}
		want := membershipPolicy{Version: 1, Namespace: namespaceIdentity{Name: "team", UID: "uid-1"}, Users: []string{"bob"}, Bindings: []binding{}}
		if diff := cmp.Diff(want, membership); diff != "" {
			t.Errorf("created membership mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestSnapshotAddUsers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users []string
		want  []string
	}{
		{name: "empty adds no users", users: []string{}, want: []string{"alice"}},
		{name: "existing and repeated additions are idempotent", users: []string{"bob", "alice", "bob"}, want: []string{"alice", "bob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := snapshotClient(t, interceptor.Funcs{}, snapshotObjects(t)...)
			snapshot := membershipSnapshot(t, target)
			result, err := snapshot.AddUsers(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), tc.users)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, result); diff != "" {
				t.Errorf("persisted additions (-want +got):\n%s", diff)
			}
			membership, err := parseMembershipPolicy(readMembership(t, target))
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, membership.Users); diff != "" {
				t.Errorf("AddUsers() users mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSnapshotWriteUsers(t *testing.T) {
	t.Run("local cache follows write outcome", testMembershipInvalidation)
	t.Run("persisted write survives failed reload and retires in-flight data", testMembershipWriteReload)

	for _, tc := range []struct {
		name    string
		missing bool
		remove  bool
		add     bool
	}{
		{name: "replacement conflicts with a concurrent operator edit"},
		{name: "idempotent addition still detects a stale snapshot", add: true},
		{name: "initial creation conflicts with a competing creator", missing: true},
		{name: "deleted membership is not silently recreated", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := snapshotObjects(t)
			records := objects
			if tc.missing {
				records = objects[:1]
			}
			writes := 0
			target := snapshotClient(t, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}, records...)
			snapshot := membershipSnapshot(t, target)
			concurrent := objects[1].DeepCopy()
			concurrent.Data[policyDataKey] = strings.ReplaceAll(concurrent.Data[policyDataKey], "alice", "carol")
			concurrent.Data[policyDataKey] = strings.ReplaceAll(concurrent.Data[policyDataKey], "team-editors", "new-editors")
			switch {
			case tc.missing:
				concurrent.ResourceVersion = ""
				if err := target.Create(t.Context(), concurrent); err != nil {
					t.Fatal(err)
				}
			case tc.remove:
				if err := target.Delete(t.Context(), concurrent); err != nil {
					t.Fatal(err)
				}
			default:
				if err := target.Update(t.Context(), concurrent); err != nil {
					t.Fatal(err)
				}
			}
			writes = 0
			write := snapshot.ReplaceUsers
			if tc.add {
				write = snapshot.AddUsers
			}
			_, err := write(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), []string{"alice"})
			if !errors.Is(err, ErrMembershipConflict) || writes != 1 {
				t.Fatalf("write users = %v, %d writes, want conflict and one attempt", err, writes)
			}
			if !tc.remove {
				stored := readMembership(t, target)
				if diff := cmp.Diff(concurrent.Data, stored.Data); diff != "" {
					t.Errorf("concurrent policy overwritten (-want +got):\n%s", diff)
				}
			}
		})
	}

	t.Run("write failure is sanitized and not reported as invalid membership", func(t *testing.T) {
		target := snapshotClient(t, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "private-record", errors.New("private policy"))
		}}, snapshotObjects(t)...)
		_, err := membershipSnapshot(t, target).ReplaceUsers(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), []string{"bob"})
		if err == nil || errors.Is(err, ErrInvalidMembership) || errors.Is(err, ErrMembershipConflict) || strings.Contains(err.Error(), "private") {
			t.Fatalf("ReplaceUsers() error = %v, want sanitized persistence failure", err)
		}
	})

	for _, name := range []string{"cancelled request", "missing snapshot", "missing namespace metadata", "mismatched namespace identity", "missing snapshot version", "undefined member role"} {
		t.Run(name, func(t *testing.T) {
			target := snapshotClient(t, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("invalid write reached the API server")
				return nil
			}}, snapshotObjects(t)...)
			snapshot := membershipSnapshot(t, target)
			namespace := currentNamespace()
			ctx := t.Context()
			switch name {
			case "cancelled request":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing snapshot":
				snapshot = nil
			case "missing namespace metadata":
				namespace = nil
			case "mismatched namespace identity":
				namespace.Name = "other"
			case "missing snapshot version":
				clear(snapshot.resourceVersions)
			case "undefined member role":
				delete(snapshot.records.cluster.Roles, memberRole)
			}
			_, err := snapshot.ReplaceUsers(ctx, target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, namespace, []string{"bob"})
			if err == nil || errors.Is(err, ErrInvalidMembership) || errors.Is(err, ErrMembershipConflict) {
				t.Fatalf("ReplaceUsers() error = %v, want snapshot/context failure", err)
			}
			if ctx.Err() != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("ReplaceUsers() error = %v, want context.Canceled", err)
			}
		})
	}
}

func testMembershipInvalidation(t *testing.T) {
	t.Helper()
	resource := schema.GroupResource{Resource: "configmaps"}
	for _, tc := range []struct {
		name      string
		writeErr  error
		unchanged bool
	}{
		{name: "version conflict", writeErr: k8serrors.NewConflict(resource, "membership", errors.New("changed"))},
		{name: "create conflict", writeErr: k8serrors.NewAlreadyExists(resource, "membership")},
		{name: "deleted record", writeErr: k8serrors.NewNotFound(resource, "membership")},
		{name: "rejected write retains policy", writeErr: k8serrors.NewForbidden(resource, "membership", errors.New("denied")), unchanged: true},
		{name: "omitted users retain policy", unchanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				if k8serrors.IsAlreadyExists(tc.writeErr) {
					source.list.Items = source.list.Items[:1]
				}
				startPolicyStore(t, store)
				credential := auth.ClusterCredential{ImpersonateUser: "admin"}
				snapshot := awaitStoreSnapshot(t, store, credential)
				source.mu.Lock()
				source.listErr = errors.New("offline")
				source.mu.Unlock()
				writes := 0
				target := snapshotClient(t, interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						writes++
						return tc.writeErr
					},
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						writes++
						return tc.writeErr
					},
				})
				var users []string
				if tc.writeErr != nil {
					users = []string{"bob"}
				}
				result, err := snapshot.ReplaceUsers(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), users)
				if result != nil || (err != nil) != (tc.writeErr != nil) {
					t.Fatalf("write = %v, %v, want no result and error presence %v", result, err, tc.writeErr != nil)
				}
				if !tc.unchanged && !errors.Is(err, ErrMembershipConflict) {
					t.Fatalf("write error = %v, want conflict", err)
				}
				wantWrites := 1
				if users == nil {
					wantWrites = 0
				}
				if writes != wantWrites {
					t.Fatalf("write attempts = %d, want %d", writes, wantWrites)
				}
				_, err = store.Snapshot(credential)
				if tc.unchanged && err != nil || !tc.unchanged && !errors.Is(err, ErrPolicyUnavailable) {
					t.Fatalf("snapshot after write = %v, want available %v", err, tc.unchanged)
				}
			})
		})
	}
}

func testMembershipWriteReload(t *testing.T) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		store, source := newPolicyStore(t)
		otherStore, _ := newPolicyStore(t)
		startPolicyStore(t, store)
		startPolicyStore(t, otherStore)
		credential := auth.ClusterCredential{ImpersonateUser: "alice"}
		snapshot := awaitStoreSnapshot(t, store, credential)
		other := awaitStoreSnapshot(t, otherStore, credential)
		release := make(chan struct{})
		source.mu.Lock()
		source.holdNext = release
		source.mu.Unlock()
		source.notify()
		synctest.Wait()
		source.mu.Lock()
		source.listErr = errors.New("offline")
		source.mu.Unlock()
		reads, _ := source.counts()
		target := snapshotClient(t, interceptor.Funcs{}, snapshotObjects(t)...)
		users, err := snapshot.ReplaceUsers(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, currentNamespace(), []string{"bob"})
		if err != nil || !slices.Equal(users, []string{"bob"}) {
			t.Fatalf("persisted write = %v, %v, want [bob], nil", users, err)
		}
		stored, err := parseMembershipPolicy(readMembership(t, target))
		if err != nil || !slices.Equal(stored.Users, users) {
			t.Fatalf("stored membership = %+v, %v, want users %v", stored, err, users)
		}
		close(release)
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got, _ := source.counts(); got <= reads+1 {
			t.Fatalf("reload reads = %d, want retries after baseline %d", got, reads)
		}
		if _, err := store.Snapshot(credential); !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("snapshot after failed retries = %v, want unavailable", err)
		}
		if allowed, err := snapshot.CheckAllNamespaces("secrets", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("captured check after write = %v, %v, want unavailable", allowed, err)
		}
		if _, err := other.CheckAllNamespaces("secrets", "get"); err != nil {
			t.Fatalf("independent replica lost its usable policy: %v", err)
		}
	})
}

func membershipSnapshot(t *testing.T, target client.Client) *Snapshot {
	t.Helper()
	snapshot, err := Load(t.Context(), target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, auth.ClusterCredential{ImpersonateUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func readMembership(t *testing.T, target client.Client) *corev1.ConfigMap {
	t.Helper()
	var record corev1.ConfigMap
	if err := target.Get(t.Context(), client.ObjectKey{Namespace: "policies", Name: membershipNamePrefix + "uid-1"}, &record); err != nil {
		t.Fatal(err)
	}
	return &record
}
