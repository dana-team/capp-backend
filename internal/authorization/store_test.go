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
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
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
		if allowed, err := admin.CheckNamespace(currentNamespace(), "capps", "get"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("shared namespace check = %v, %v, want unavailable", allowed, err)
		}
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
		for _, retire := range []string{"invalidation", "publication"} {
			t.Run(retire, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, source := newPolicyStore(t)
					startPolicyStore(t, store)
					snapshot := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
					release := make(chan struct{})
					enforcer := snapshot.evaluators.cluster
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
						allowed, checkErr = snapshot.CheckCluster("namespaces", "create")
					}()
					synctest.Wait()
					if retire == "invalidation" {
						store.Invalidate()
					} else {
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
				before := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
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
			before := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
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
		snapshot := awaitStoreSnapshot(t, store, auth.ClusterCredential{ImpersonateUser: "admin"})
		source.mu.Lock()
		source.holdNext = make(chan struct{})
		source.mu.Unlock()
		source.notify()
		synctest.Wait()
		cancel()
		if allowed, err := snapshot.CheckCluster("namespaces", "create"); allowed || !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("cancelled check = %v, %v, want unavailable", allowed, err)
		}
		store.Stop()
		store.Stop()
		if !source.stream().IsStopped() {
			t.Error("Stop left the watch running")
		}
		if err := store.Start(t.Context()); err == nil {
			t.Error("stopped store restarted")
		}
	})
}

// policySource supplies complete LISTs and manually controlled observation. Its
// state is separate from the published store so silent changes remain observable.
type policySource struct {
	mu       sync.Mutex
	list     *corev1.ConfigMapList
	listErr  error
	watchErr error
	holdNext <-chan struct{}
	reads    int
	watches  int
	current  *watch.RaceFreeFakeWatcher
	version  int
}

func newPolicyStore(t *testing.T) (*Store, *policySource) {
	t.Helper()
	source := &policySource{list: &corev1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: "100"}}, version: 100}
	for _, object := range snapshotObjects(t) {
		source.list.Items = append(source.list.Items, *object)
	}
	target := snapshotClient(t, interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, out client.ObjectList, opts ...client.ListOption) error {
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
		Watch: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
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
