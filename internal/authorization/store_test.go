package authorization

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestStoreSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, source := newPolicyStore(t)
		if _, err := store.Snapshot(auth.ClusterCredential{ImpersonateUser: "admin"}); !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("Snapshot before Start = %v, want unavailable", err)
		}
		startPolicyStore(t, store)
		groups := []string{"global-readers"}
		groupCaller := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "bob", ImpersonateGroups: groups})
		groups[0] = "unbound"
		otherCaller := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "bob", ImpersonateGroups: groups})
		admin := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
		reads, watches := source.counts()
		var callers sync.WaitGroup
		for range 10 {
			callers.Go(func() {
				if allowed, err := admin.CheckCluster("namespaces", "create"); !allowed || err != nil {
					t.Errorf("admin cluster check = %v, %v, want true, nil", allowed, err)
				}
				if allowed, err := groupCaller.CheckAllNamespaces("secrets", "get"); !allowed || err != nil {
					t.Errorf("group check = %v, %v, want true, nil", allowed, err)
				}
				if allowed, err := otherCaller.CheckAllNamespaces("secrets", "get"); allowed || err != nil {
					t.Errorf("unbound caller check = %v, %v, want false, nil", allowed, err)
				}
			})
		}
		callers.Wait()
		// A silent stream neither polls nor expires, even when reality has changed.
		source.changePolicy(`{"version":1,"roles":{},"bindings":[]}`)
		time.Sleep(24 * time.Hour)
		if allowed, err := admin.CheckCluster("namespaces", "create"); !allowed || err != nil {
			t.Fatalf("quiet snapshot check = %v, %v, want retained grant", allowed, err)
		}
		if gotReads, gotWatches := source.counts(); gotReads != reads || gotWatches != watches {
			t.Errorf("quiet/request reads,watches = %d,%d, want %d,%d", gotReads, gotWatches, reads, watches)
		}
	})
	t.Run("retirement during evaluation", func(t *testing.T) {
		for _, retire := range []string{"invalidation", "publication", "namespace invalidation", "namespace publication"} {
			t.Run(retire, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, source := newPolicyStore(t)
					startPolicyStore(t, store)
					namespaced := retire == "namespace invalidation" || retire == "namespace publication"
					credential := auth.ClusterCredential{ImpersonateUser: "admin"}
					if namespaced {
						credential.ImpersonateUser = "alice"
					}
					snapshot := awaitNamespaceSnapshot(t, store, credential)
					release := make(chan struct{})
					enforcer := snapshot.evaluators.cluster
					if namespaced {
						enforcer = snapshot.evaluators.namespaces["uid-1"]
					}
					enforcer.GetModel()["m"]["m"].Value = "pause()"
					enforcer.AddFunction("pause", func(...any) (any, error) {
						<-release
						return true, nil
					})
					var allowed bool
					var checkErr error
					done := make(chan struct{})
					go func() {
						defer close(done)
						if namespaced {
							allowed, checkErr = snapshot.CheckNamespace(currentNamespace(), "capps", "get")
						} else {
							allowed, checkErr = snapshot.CheckCluster("namespaces", "create")
						}
					}()
					synctest.Wait()
					switch retire {
					case "namespace invalidation":
						store.InvalidateNamespaces()
					case "namespace publication":
						source.namespaces.mu.Lock()
						source.namespaces.list.Items[0].UID = "replacement"
						source.namespaces.list.Items[0].ResourceVersion = "201"
						source.namespaces.list.ResourceVersion = "201"
						source.namespaces.mu.Unlock()
						source.namespaces.notify()
					case "invalidation":
						store.Invalidate()
					case "publication":
						source.changePolicy(`{"version":1,"roles":{"capp-user":[],"editor":[],"delegator":[]},"bindings":[]}`)
						source.notify()
					}
					synctest.Wait()
					close(release)
					<-done
					if allowed || !errors.Is(checkErr, ErrPolicyUnavailable) {
						t.Fatalf("check crossing %s = %v, %v, want unavailable", retire, allowed, checkErr)
					}
				})
			})
		}
	})
}

func TestStoreStart(t *testing.T) {
	for _, failure := range []string{"list failure", "partial list", "watch failure"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				source.mu.Lock()
				switch failure {
				case "list failure":
					source.listErr = errors.New("offline")
				case "partial list":
					source.list.Continue = "next"
				case "watch failure":
					source.watchErr = errors.New("offline")
				}
				source.mu.Unlock()
				startPolicyStore(t, store)
				if _, err := store.Snapshot(auth.ClusterCredential{ImpersonateUser: "admin"}); !errors.Is(err, ErrPolicyUnavailable) {
					t.Fatalf("Snapshot during %s = %v, want unavailable", failure, err)
				}
				source.mu.Lock()
				source.listErr, source.watchErr, source.list.Continue = nil, nil, ""
				source.mu.Unlock()
				time.Sleep(time.Minute)
				synctest.Wait()
				snapshot := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				if allowed, err := snapshot.CheckCluster("namespaces", "create"); !allowed || err != nil {
					t.Fatalf("recovered check = %v, %v, want true, nil", allowed, err)
				}
			})
		})
	}
}

func TestStoreReload(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invalidate bool
	}{
		{name: "read failure"},
		{name: "partial list"},
		{name: "missing required policy", invalidate: true},
		{name: "malformed policy", invalidate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				startPolicyStore(t, store)
				before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				source.mu.Lock()
				original := source.list.DeepCopy()
				switch tc.name {
				case "read failure":
					source.listErr = errors.New("offline")
				case "partial list":
					source.list.Continue = "next"
					source.list.Items = nil
				case "missing required policy":
					source.list.Items = source.list.Items[1:]
				case "malformed policy":
					source.list.Items[0].Data[policyDataKey] = "invalid"
					source.list.Items[0].ResourceVersion = "101"
				}
				source.mu.Unlock()
				source.notify()
				synctest.Wait()
				allowed, err := before.CheckCluster("namespaces", "create")
				if tc.invalidate {
					if _, err := before.Namespaces(); err != nil {
						t.Fatalf("invalid policy retired namespace metadata: %v", err)
					}
					if allowed || !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("invalid policy check = %v, %v, want unavailable", allowed, err)
					}
				} else if !allowed || err != nil {
					t.Fatalf("transient failure check = %v, %v, want retained grant", allowed, err)
				}
				source.mu.Lock()
				source.list, source.listErr = original, nil
				source.mu.Unlock()
				// Recovery must not depend on another event or request.
				time.Sleep(time.Minute)
				synctest.Wait()
				after := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				if (before.generation != after.generation) != tc.invalidate {
					t.Fatalf("generation changed = %v, want %v", before.generation != after.generation, tc.invalidate)
				}
				if allowed, err := after.CheckCluster("namespaces", "create"); !allowed || err != nil {
					t.Fatalf("recovered check = %v, %v, want true, nil", allowed, err)
				}
			})
		})
	}
	t.Run("generation reuse", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, source := newPolicyStore(t)
			startPolicyStore(t, store)
			before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
			source.mu.Lock()
			source.list.ResourceVersion = "101"
			source.mu.Unlock()
			source.notify()
			synctest.Wait()
			after := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
			if after.generation != before.generation || after.ResourceVersion() != "101" || before.ResourceVersion() != "100" {
				t.Fatalf("unchanged records must reuse evaluators and retain each capture's LIST version")
			}
			// Identity, not just name/version, participates in generation reuse.
			source.mu.Lock()
			source.list.Items[0].UID = "replacement-policy"
			source.mu.Unlock()
			source.notify()
			synctest.Wait()
			replaced := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
			if replaced.generation == after.generation {
				t.Fatal("replacement ConfigMap reused a superseded generation")
			}
		})
	})
}

func TestStoreObserve(t *testing.T) {
	for _, reason := range []string{"rotation", "expired history", "watch failure"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				startPolicyStore(t, store)
				before := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				_, watches := source.counts()
				// Let the stream outlive the reflector's short-watch error threshold.
				time.Sleep(time.Second)
				// Change policy without delivering an object event.
				source.changePolicy(`{"version":1,"roles":{"capp-user":[],"editor":[],"delegator":[]},"bindings":[]}`)
				stream := source.stream()
				if reason == "watch failure" {
					source.mu.Lock()
					source.watchErr = errors.New("offline")
					source.mu.Unlock()
				}
				if reason == "expired history" {
					stream.Error(&metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: http.StatusGone})
				} else {
					stream.Stop()
				}
				synctest.Wait()
				if reason == "watch failure" {
					for range 10 {
						if _, got := source.counts(); got > watches {
							break
						}
						time.Sleep(100 * time.Millisecond)
						synctest.Wait()
					}
					if _, got := source.counts(); got <= watches {
						t.Fatal("reconnect did not attempt the failing watch")
					}
					if allowed, err := before.CheckCluster("namespaces", "create"); !allowed || err != nil {
						t.Fatalf("watch failure check = %v, %v, want retained grant", allowed, err)
					}
					source.mu.Lock()
					source.watchErr = nil
					source.mu.Unlock()
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				after := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				if allowed, err := after.CheckCluster("namespaces", "create"); allowed || err != nil {
					t.Fatalf("reconnected check = %v, %v, want revoked grant", allowed, err)
				}
				if allowed, err := before.CheckCluster("namespaces", "create"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
					t.Fatalf("superseded check = %v, %v, want unavailable", allowed, err)
				}
				if _, got := source.counts(); got <= watches {
					t.Fatalf("watch attempts = %d, want more than %d", got, watches)
				}
			})
		})
	}
}

func TestWaitPolicyRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backoff := policyRetryBackoff()
		backoff.Duration = backoff.Cap
		backoff.Steps = 0
		var jittered bool
		for range 10 {
			start := time.Now()
			if !waitPolicyRetry(t.Context(), &backoff) {
				t.Fatal("retry canceled unexpectedly")
			}
			delay := time.Since(start)
			if delay < 25*time.Second || delay > 30*time.Second {
				t.Fatalf("capped retry delay = %v, want between 25 and 30 seconds", delay)
			}
			jittered = jittered || delay != backoff.Cap
		}
		if !jittered {
			t.Fatal("capped retries lost their jitter")
		}
	})
}

func TestStoreInvalidate(t *testing.T) {
	for _, trigger := range []string{"event", "invalidation", "required policy deletion"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				startPolicyStore(t, store)
				before := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				release := make(chan struct{})
				source.mu.Lock()
				source.holdNext = release
				source.mu.Unlock()
				source.notify()
				synctest.Wait()
				// The in-flight load has captured the old policy and cannot see this change.
				source.changePolicy(`{"version":1,"roles":{"capp-user":[],"editor":[],"delegator":[]},"bindings":[]}`)
				switch trigger {
				case "event":
					source.notify()
				case "invalidation":
					store.Invalidate()
				case "required policy deletion":
					source.stream().Delete(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterPolicyName, Namespace: "policies", ResourceVersion: "101"}})
				}
				synctest.Wait()
				if trigger != "event" {
					if allowed, err := before.CheckCluster("namespaces", "create"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("invalidated check = %v, %v, want unavailable before reload completes", allowed, err)
					}
				}
				fresh := make(chan struct{})
				source.mu.Lock()
				source.holdNext = fresh
				source.mu.Unlock()
				close(release)
				synctest.Wait()
				time.Sleep(time.Second)
				synctest.Wait()
				if trigger != "event" {
					if _, err := store.Snapshot(auth.ClusterCredential{ImpersonateUser: "admin"}); !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("stale in-flight load restored policy: %v", err)
					}
				}
				close(fresh)
				synctest.Wait()
				after := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				if allowed, err := after.CheckCluster("namespaces", "create"); allowed || err != nil {
					t.Fatalf("pending event check = %v, %v, want false, nil", allowed, err)
				}
				if allowed, err := before.CheckCluster("namespaces", "create"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
					t.Fatalf("retired check = %v, %v, want unavailable", allowed, err)
				}
			})
		})
	}
}

func TestStoreStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, source := newPolicyStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if err := store.Start(ctx); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		snapshot := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
		source.mu.Lock()
		source.holdNext = make(chan struct{})
		source.mu.Unlock()
		source.notify()
		source.namespaces.mu.Lock()
		source.namespaces.holdNext = make(chan struct{})
		source.namespaces.mu.Unlock()
		source.namespaces.notify()
		synctest.Wait()
		cancel()
		if allowed, err := snapshot.CheckCluster("namespaces", "create"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("cancelled check = %v, %v, want unavailable", allowed, err)
		}
		if _, err := snapshot.Namespaces(); !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("canceled namespace accessor = %v", err)
		}
		store.Stop()
		store.Stop()
		if !source.stream().IsStopped() || !source.namespaces.current.IsStopped() {
			t.Error("Stop left the watch running")
		}
		if err := store.Start(t.Context()); err == nil {
			t.Error("stopped store restarted")
		}
	})
}

func TestSnapshotNamespaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, source := newPolicyStore(t)
		ns := source.namespaces
		ns.list.Items[0].Status.Conditions = []corev1.NamespaceCondition{{Type: corev1.NamespaceDeletionDiscoveryFailure, Status: corev1.ConditionFalse}}
		ns.list.Items = append(ns.list.Items, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alpha", UID: "uid-2", ResourceVersion: "199"}})
		startPolicyStore(t, store)
		snapshot := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
		ns.mu.Lock()
		reads, watches := ns.reads, ns.watches
		ns.mu.Unlock()
		namespaces, err := snapshot.Namespaces()
		if err != nil {
			t.Fatal(err)
		}
		want := []corev1.Namespace{*ns.list.Items[1].DeepCopy(), *ns.list.Items[0].DeepCopy()}
		if diff := cmp.Diff(want, namespaces); diff != "" {
			t.Fatalf("Namespaces (-want +got):\n%s", diff)
		}
		namespace, err := snapshot.Namespace("team")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want[1], *namespace); diff != "" {
			t.Fatalf("Namespace (-want +got):\n%s", diff)
		}
		namespaces[1].Labels["team"] = "changed"
		namespaces[1].Status.Conditions[0].Status = corev1.ConditionTrue
		namespace.UID = "forged"
		namespace.Labels["team"] = "changed again"
		namespace.Status.Conditions[0].Status = corev1.ConditionUnknown
		again, err := snapshot.Namespaces()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, again); diff != "" {
			t.Fatalf("caller mutated cached metadata (-want +got):\n%s", diff)
		}
		if allowed, err := snapshot.CheckNamespace(namespace, "capps", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("forged identity check = %v, %v, want unavailable", allowed, err)
		}
		if _, err := snapshot.Namespace("absent"); !k8serrors.IsNotFound(err) {
			t.Fatalf("cache miss = %v, want NotFound", err)
		}
		ns.mu.Lock()
		ns.list.ResourceVersion = "201"
		ns.mu.Unlock()
		ns.notify()
		synctest.Wait()
		unchanged := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
		version, err := unchanged.NamespaceResourceVersion()
		if err != nil || version != "201" || unchanged.namespaces != snapshot.namespaces {
			t.Fatalf("unchanged reload version,generation = %q,%v, %v", version, unchanged.namespaces == snapshot.namespaces, err)
		}
		oldVersion, err := snapshot.NamespaceResourceVersion()
		if err != nil || oldVersion != "200" {
			t.Fatalf("captured namespace version = %q, %v, want 200", oldVersion, err)
		}
		// Namespace access and misses never schedule work; only the explicit event did.
		ns.mu.Lock()
		if ns.reads != reads+1 || ns.watches != watches {
			t.Errorf("namespace traffic = %d,%d, want %d,%d", ns.reads, ns.watches, reads+1, watches)
		}
		ns.list.Items = nil
		ns.list.ResourceVersion = "202"
		stream := ns.current
		ns.mu.Unlock()
		stream.Delete(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", ResourceVersion: "202"}})
		synctest.Wait()
		empty := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
		items, err := empty.Namespaces()
		if err != nil || items == nil || len(items) != 0 {
			t.Fatalf("empty collection = %v, %v, want empty slice", items, err)
		}
		if _, err := empty.Namespace("team"); !k8serrors.IsNotFound(err) {
			t.Fatalf("deleted namespace = %v, want NotFound", err)
		}
		if _, err := snapshot.Namespaces(); !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("retired collection = %v, want unavailable", err)
		}
	})
}

func TestStoreObserveNamespaces(t *testing.T) {
	for _, failure := range []string{"initial read", "watch reconnect"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				ns := source.namespaces
				if failure == "initial read" {
					ns.listErr = errors.New("offline")
				}
				startPolicyStore(t, store)
				policy := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				switch failure {
				case "initial read":
					if _, err := policy.Namespace("absent"); !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("unavailable cache miss = %v", err)
					}
					if allowed, err := policy.CheckNamespace(currentNamespace(), "capps", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("namespace check = %v, %v, want unavailable", allowed, err)
					}
				case "watch reconnect":
					before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
					time.Sleep(time.Second)
					ns.mu.Lock()
					ns.watchErr = errors.New("offline")
					stream, watches := ns.current, ns.watches
					ns.mu.Unlock()
					stream.Stop()
					synctest.Wait()
					time.Sleep(time.Second)
					synctest.Wait()
					ns.mu.Lock()
					attempts := ns.watches
					ns.mu.Unlock()
					if attempts <= watches {
						t.Fatal("watch failure was not attempted")
					}
					if allowed, err := before.CheckNamespace(currentNamespace(), "capps", "get"); !allowed || err != nil {
						t.Fatalf("watch failure check = %v, %v, want retained grant", allowed, err)
					}
				}
				if allowed, err := policy.CheckCluster("namespaces", "create"); !allowed || err != nil {
					t.Fatalf("policy-only check = %v, %v, want allowed", allowed, err)
				}
				ns.mu.Lock()
				ns.listErr, ns.watchErr = nil, nil
				ns.list.Items[0].UID = "replacement"
				ns.list.Items[0].ResourceVersion = "201"
				ns.list.ResourceVersion = "201"
				ns.mu.Unlock()
				time.Sleep(time.Minute)
				synctest.Wait()
				after := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
				replacement, err := after.Namespace("team")
				if err != nil {
					t.Fatal(err)
				}
				if replacement.UID != "replacement" {
					t.Fatalf("recovered UID = %q", replacement.UID)
				}
				if allowed, err := after.CheckNamespace(replacement, "capps", "get"); allowed || err != nil {
					t.Fatalf("replacement check = %v, %v, want denied", allowed, err)
				}
				if after.generation != policy.generation {
					t.Fatal("namespace recovery recompiled policy")
				}
			})
		})
	}
}

func TestStoreReloadNamespaces(t *testing.T) {
	for _, failure := range []string{"read failure", "partial list"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				startPolicyStore(t, store)
				before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
				reads, watches := source.counts()
				ns := source.namespaces
				ns.mu.Lock()
				if failure == "read failure" {
					ns.listErr = errors.New("offline")
				} else {
					ns.list.Continue = "next"
				}
				ns.list.Items[0].UID = "replacement"
				ns.list.Items[0].ResourceVersion = "201"
				ns.list.ResourceVersion = "201"
				ns.mu.Unlock()
				ns.notify()
				synctest.Wait()
				if allowed, err := before.CheckNamespace(currentNamespace(), "capps", "get"); !allowed || err != nil {
					t.Fatalf("failed refresh check = %v, %v, want retained grant", allowed, err)
				}
				ns.mu.Lock()
				ns.listErr, ns.list.Continue = nil, ""
				ns.mu.Unlock()
				time.Sleep(time.Minute)
				synctest.Wait()
				after := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
				if after.namespaces == before.namespaces || after.generation != before.generation {
					t.Fatal("namespace recovery did not change only the namespace generation")
				}
				if allowed, err := before.CheckNamespace(currentNamespace(), "capps", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
					t.Fatalf("retired check = %v, %v, want unavailable", allowed, err)
				}
				if gotReads, gotWatches := source.counts(); gotReads != reads || gotWatches != watches {
					t.Fatalf("namespace refresh caused policy traffic: %d,%d", gotReads, gotWatches)
				}
			})
		})
	}
	t.Run("silent replacement", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, source := newPolicyStore(t)
			startPolicyStore(t, store)
			before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
			ns := source.namespaces
			ns.mu.Lock()
			reads, watches := ns.reads, ns.watches
			ns.list.Items[0].UID = "replacement"
			ns.list.Items[0].ResourceVersion = "201"
			ns.list.ResourceVersion = "201"
			ns.mu.Unlock()
			time.Sleep(24 * time.Hour)
			if allowed, err := before.CheckNamespace(currentNamespace(), "capps", "get"); !allowed || err != nil {
				t.Fatalf("silently stale identity check = %v, %v, want accepted stale grant", allowed, err)
			}
			ns.mu.Lock()
			if ns.reads != reads || ns.watches != watches {
				t.Error("quiet namespace cache polled or expired")
			}
			ns.mu.Unlock()
			ns.notify()
			synctest.Wait()
			after := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "alice"})
			replacement, err := after.Namespace("team")
			if err != nil {
				t.Fatal(err)
			}
			if allowed, err := after.CheckNamespace(replacement, "capps", "get"); allowed || err != nil {
				t.Fatalf("converged replacement check = %v, %v, want denied", allowed, err)
			}
			if allowed, err := before.CheckNamespace(currentNamespace(), "capps", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
				t.Fatalf("captured predecessor check = %v, %v, want unavailable", allowed, err)
			}
		})
	})
}

func TestStoreInvalidateNamespaces(t *testing.T) {
	for _, trigger := range []string{"event", "invalidation"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, source := newPolicyStore(t)
				startPolicyStore(t, store)
				before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				ns := source.namespaces
				release, fresh := make(chan struct{}), make(chan struct{})
				ns.mu.Lock()
				ns.holdNext = release
				ns.mu.Unlock()
				ns.notify()
				synctest.Wait()
				ns.mu.Lock()
				ns.list.Items[0].UID = "replacement"
				ns.list.Items[0].ResourceVersion = "201"
				ns.list.ResourceVersion = "201"
				ns.holdNext = fresh
				ns.mu.Unlock()
				if trigger == "event" {
					ns.notify()
				} else {
					store.InvalidateNamespaces()
				}
				close(release)
				synctest.Wait()
				time.Sleep(time.Second)
				synctest.Wait()
				if trigger == "invalidation" {
					if _, err := before.Namespace("team"); !errors.Is(err, ErrPolicyUnavailable) {
						t.Fatalf("old load restored invalidated namespaces: %v", err)
					}
				}
				if allowed, err := before.CheckCluster("namespaces", "create"); !allowed || err != nil {
					t.Fatalf("namespace invalidation blocked cluster check: %v,%v", allowed, err)
				}
				close(fresh)
				synctest.Wait()
				after := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
				namespace, err := after.Namespace("team")
				if err != nil || namespace.UID != "replacement" {
					t.Fatalf("pending refresh = %v,%v, want replacement", namespace, err)
				}
				if after.generation != before.generation {
					t.Fatal("namespace invalidation recompiled policy")
				}
			})
		})
	}
	t.Run("independent recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store, source := newPolicyStore(t)
			startPolicyStore(t, store)
			before := awaitNamespaceSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
			ns := source.namespaces
			ns.mu.Lock()
			ns.listErr = errors.New("offline")
			ns.mu.Unlock()
			store.InvalidateNamespaces()
			store.Invalidate()
			synctest.Wait()
			policy := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
			if _, err := policy.Namespaces(); !errors.Is(err, ErrPolicyUnavailable) {
				t.Fatalf("policy recovery restored namespace availability: %v", err)
			}
			source.mu.Lock()
			source.listErr = errors.New("offline")
			source.mu.Unlock()
			store.Invalidate()
			ns.mu.Lock()
			ns.listErr = nil
			ns.mu.Unlock()
			time.Sleep(time.Minute)
			synctest.Wait()
			store.mu.RLock()
			ready := store.namespaces != nil
			store.mu.RUnlock()
			if !ready {
				t.Fatal("namespace recovery blocked by policy failure")
			}
			if _, err := store.Snapshot(auth.ClusterCredential{ImpersonateUser: "admin"}); !errors.Is(err, ErrPolicyUnavailable) {
				t.Fatalf("namespace recovery restored invalidated policy: %v", err)
			}
			if _, err := before.Namespaces(); !errors.Is(err, ErrPolicyUnavailable) {
				t.Fatalf("recovery restored retired namespace capture: %v", err)
			}
		})
	})
}

// policySource supplies complete LISTs and manually controlled observation. Its
// state is separate from the published store so silent changes remain observable.
type policySource struct {
	namespaces *namespaceSource
	mu         sync.Mutex
	list       *corev1.ConfigMapList
	listErr    error
	watchErr   error
	holdNext   <-chan struct{}
	reads      int
	watches    int
	current    *watch.RaceFreeFakeWatcher
	version    int
}

func newPolicyStore(t *testing.T) (*Store, *policySource) {
	t.Helper()
	source := &policySource{
		list: &corev1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: "100"}}, version: 100,
		namespaces: &namespaceSource{list: &corev1.NamespaceList{
			ListMeta: metav1.ListMeta{ResourceVersion: "200"},
			Items: []corev1.Namespace{{
				ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-1", ResourceVersion: "200", Labels: map[string]string{"team": "dev"}},
				Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
			}},
		}},
	}
	for _, object := range snapshotObjects(t) {
		source.list.Items = append(source.list.Items, *object)
	}
	target := snapshotClient(t, interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, out client.ObjectList, opts ...client.ListOption) error {
			if result, ok := out.(*corev1.NamespaceList); ok {
				return source.namespaces.read(ctx, t, result, opts...)
			}
			options := (&client.ListOptions{}).ApplyOptions(opts)
			raw := options.AsListOptions()
			if options.Namespace != "policies" || raw.LabelSelector != policyLabelKey+"="+policyLabelValue || raw.ResourceVersion != "" || raw.Limit != 0 || raw.Continue != "" {
				t.Errorf("policy LIST options = %+v, want authoritative unpaginated namespace/label selection", options)
			}
			source.mu.Lock()
			source.reads++
			list, err, held := source.list.DeepCopy(), source.listErr, source.holdNext
			source.holdNext = nil
			source.mu.Unlock()
			if held != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-held:
				}
			}
			if err != nil {
				return err
			}
			result, ok := out.(*corev1.ConfigMapList)
			if !ok {
				return errors.New("policy LIST requires ConfigMapList")
			}
			*result = *list
			return nil
		},
		Watch: func(_ context.Context, _ client.WithWatch, out client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			if _, ok := out.(*corev1.NamespaceList); ok {
				return source.namespaces.watch(t, opts...)
			}
			options := (&client.ListOptions{}).ApplyOptions(opts)
			raw := options.AsListOptions()
			if options.Namespace != "policies" || raw.LabelSelector != policyLabelKey+"="+policyLabelValue || raw.ResourceVersion == "" || raw.ResourceVersion == "0" || raw.SendInitialEvents != nil {
				t.Errorf("policy WATCH options = %+v, want versioned namespace/label selection", options)
			}
			source.mu.Lock()
			defer source.mu.Unlock()
			source.watches++
			if source.watches == 1 && raw.ResourceVersion != "100" {
				t.Errorf("initial watch version = %q, want LIST version 100", raw.ResourceVersion)
			}
			if source.watchErr != nil {
				return nil, source.watchErr
			}
			source.current = watch.NewRaceFreeFake()
			return source.current, nil
		},
	})
	store, err := NewStore(target, config.ClusterAuthorizationConfig{PolicyNamespace: "policies"}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Stop)
	return store, source
}

func startPolicyStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
}

func awaitStoreSnapshot(t *testing.T, store *Store, credential auth.ClusterCredential) *Snapshot {
	t.Helper()
	for range 600 {
		snapshot, err := store.Snapshot(credential)
		if err == nil {
			return snapshot
		}
		if !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
	}
	t.Fatal("policy snapshot did not become available within one simulated minute")
	return nil
}

func (s *policySource) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.watches
}

func (s *policySource) stream() *watch.RaceFreeFakeWatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *policySource) notify() {
	s.mu.Lock()
	stream, version := s.current, s.list.ResourceVersion
	s.mu.Unlock()
	stream.Modify(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterPolicyName, Namespace: "policies", ResourceVersion: version}})
}

func (s *policySource) changePolicy(payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	s.list.ResourceVersion = strconv.Itoa(s.version)
	s.list.Items[0].ResourceVersion = s.list.ResourceVersion
	s.list.Items[0].Data[policyDataKey] = payload
}

// namespaceSource controls namespace recovery independently of policy traffic.
type namespaceSource struct {
	mu                sync.Mutex
	list              *corev1.NamespaceList
	listErr, watchErr error
	holdNext          <-chan struct{}
	reads, watches    int
	current           *watch.RaceFreeFakeWatcher
}

func (s *namespaceSource) read(ctx context.Context, t *testing.T, out *corev1.NamespaceList, opts ...client.ListOption) error {
	t.Helper()
	options := (&client.ListOptions{}).ApplyOptions(opts)
	raw := options.AsListOptions()
	if options.Namespace != "" || raw.LabelSelector != "" || raw.FieldSelector != "" || raw.ResourceVersion != "" || raw.Limit != 0 || raw.Continue != "" {
		t.Errorf("namespace LIST options = %+v, want complete authoritative collection", options)
	}
	s.mu.Lock()
	s.reads++
	list, err, held := s.list.DeepCopy(), s.listErr, s.holdNext
	s.holdNext = nil
	s.mu.Unlock()
	if held != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-held:
		}
	}
	if err != nil {
		return err
	}
	*out = *list
	return nil
}

func (s *namespaceSource) watch(t *testing.T, opts ...client.ListOption) (watch.Interface, error) {
	t.Helper()
	options := (&client.ListOptions{}).ApplyOptions(opts)
	raw := options.AsListOptions()
	if options.Namespace != "" || raw.LabelSelector != "" || raw.FieldSelector != "" || raw.ResourceVersion == "" || raw.ResourceVersion == "0" || raw.SendInitialEvents != nil {
		t.Errorf("namespace WATCH options = %+v, want complete versioned collection", options)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watches++
	if s.watchErr != nil {
		return nil, s.watchErr
	}
	s.current = watch.NewRaceFreeFake()
	return s.current, nil
}

func (s *namespaceSource) notify() {
	s.mu.Lock()
	stream, version := s.current, s.list.ResourceVersion
	s.mu.Unlock()
	stream.Modify(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", ResourceVersion: version}})
}

func awaitNamespaceSnapshot(t *testing.T, store *Store, credential auth.ClusterCredential) *Snapshot {
	t.Helper()
	for range 600 {
		snapshot, err := store.Snapshot(credential)
		if err == nil {
			if _, err = snapshot.NamespaceResourceVersion(); err == nil {
				return snapshot
			}
		}
		if !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
	}
	t.Fatal("namespace snapshot did not become available within one simulated minute")
	return nil
}
