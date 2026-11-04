package authorization

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	restfake "k8s.io/client-go/rest/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestNewReconciler(t *testing.T) {
	cm, ns := readyObjects("team-a")
	store := staticStore(t, cm, ns)
	client := fake.NewClientset()
	r, err := NewReconciler(client, store)
	require.NoError(t, err)
	t.Cleanup(r.queue.ShutDown)
	_, err = NewReconciler(client, store)
	require.ErrorContains(t, err, "already has a reconciler")
}

func TestNewReconciliationRateLimiter(t *testing.T) {
	limiter := newReconciliationRateLimiter()
	require.Equal(t, 5*time.Millisecond, limiter.When("team-a"))
	var delay time.Duration
	for range 20 {
		delay = limiter.When("team-a")
		require.LessOrEqual(t, delay, time.Minute)
	}
	require.Equal(t, time.Minute, delay)
	limiter.Forget("team-a")
	require.Equal(t, 5*time.Millisecond, limiter.When("team-a"))
}

func TestReconcilerReconcile(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*corev1.ConfigMap, *corev1.Namespace)
	}{
		{"unmanaged", func(_ *corev1.ConfigMap, ns *corev1.Namespace) { ns.Labels = nil }},
		{"terminating namespace", func(_ *corev1.ConfigMap, ns *corev1.Namespace) {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
		}},
		{"deleting policy", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			now := metav1.Now()
			cm.DeletionTimestamp = &now
		}},
		{"policy absent from configured namespace", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Namespace = "team-a" }},
		{"restricted fresh policy", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Data[policyDataKey] = `{"version":1,"namespace":"team-a","bindings":[{"kind":"Group","name":"system:authenticated","role":"editor"}]}`
		}},
		{"invalid", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Data[policyDataKey] = "{" }},
		{"withdrawn", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.Labels = nil }},
		{"replacement", func(cm *corev1.ConfigMap, _ *corev1.Namespace) { cm.UID = "replacement" }},
		{"already associated", func(cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Annotations = map[string]string{NamespaceUIDAnnotation: "another-namespace"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			cm.Annotations = nil
			store := staticStore(t, cm.DeepCopy(), ns.DeepCopy())
			store.options.RestrictedBindingNamePrefixes = []string{"system:"}
			tt.change(cm, ns)
			client := fake.NewClientset(cm, ns)
			r, err := NewReconciler(client, store)
			require.NoError(t, err)
			defer r.queue.ShutDown()
			require.NoError(t, r.reconcile(t.Context(), ns.Name))
			for _, action := range client.Actions() {
				require.Contains(t, []string{"get"}, action.GetVerb())
			}
		})
	}
	t.Run("protected namespace", func(t *testing.T) {
		cm, ns := readyObjects("protected")
		cm.Annotations = nil
		store := staticStore(t, cm, ns)
		store.options.ProtectedNamespaces = []string{ns.Name}
		store.changed(policies, cm)
		client := fake.NewClientset(cm, ns)
		r, err := NewReconciler(client, store)
		require.NoError(t, err)
		defer r.queue.ShutDown()
		require.NoError(t, r.reconcile(t.Context(), ns.Name))
		require.Empty(t, client.Actions())
	})
	t.Run("conditional metadata patch propagates conflict", func(t *testing.T) {
		cm, ns := readyObjects("team-a")
		cm.Annotations = nil
		cm.ResourceVersion = "observed"
		store := staticStore(t, cm.DeepCopy(), ns.DeepCopy())
		cm.ResourceVersion = "fresh"
		client := fake.NewClientset(cm, ns)
		conflict := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("changed"))
		patches := 0
		client.PrependReactor("patch", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
			patches++
			patch, ok := action.(ktesting.PatchAction)
			require.True(t, ok)
			require.Equal(t, cm.Namespace, patch.GetNamespace())
			require.Equal(t, cm.Name, patch.GetName())
			require.Equal(t, types.MergePatchType, patch.GetPatchType())
			var body map[string]any
			require.NoError(t, json.Unmarshal(patch.GetPatch(), &body))
			require.Equal(t, map[string]any{"metadata": map[string]any{
				"uid": string(cm.UID), "resourceVersion": cm.ResourceVersion,
				"annotations": map[string]any{NamespaceUIDAnnotation: string(ns.UID)},
			}}, body)
			return true, nil, conflict
		})
		r, err := NewReconciler(client, store)
		require.NoError(t, err)
		defer r.queue.ShutDown()
		require.ErrorIs(t, r.reconcile(t.Context(), ns.Name), conflict)
		require.Equal(t, 1, patches)
	})
}

// Exercise observation and reconciliation together against an actual API server,
// including retries, namespace recreation, and persisted associations on restart.
// Run with KUBEBUILDER_ASSETS pointing to setup-envtest's binaries.
func TestReconcilerRun(t *testing.T) {
	const (
		policyNamespace  = "capp-system"
		reconcileTimeout = 5 * time.Second
		pollInterval     = 10 * time.Millisecond
	)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run API-backed reconciliation tests")
	}
	environment := &envtest.Environment{}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: policyNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	// Inject transient write failures over real transport, without simulating API semantics.
	var failPatch atomic.Bool
	var patchFailures atomic.Int64
	var unexpectedCalls atomic.Int64
	t.Cleanup(func() {
		require.Zero(t, unexpectedCalls.Load(), "workers may only observe policies/namespaces and patch policy metadata")
	})
	replicaConfig := rest.CopyConfig(cfg)
	replicaConfig.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		return reconcileTransport{next: next, failPatch: &failPatch, patchFailures: &patchFailures, unexpectedCalls: &unexpectedCalls}
	}
	replicaClient, err := kubernetes.NewForConfig(replicaConfig)
	require.NoError(t, err)
	start := func() (*Store, func()) {
		store, err := NewStore(replicaClient, StoreOptions{PolicyNamespace: policyNamespace, ProtectedNamespaces: []string{"protected"}})
		require.NoError(t, err)
		reconciler, err := NewReconciler(replicaClient, store)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		workers.Go(func() {
			if err := store.Run(ctx); err != nil {
				t.Errorf("Store.Run: %v", err)
			}
		})
		workers.Go(func() {
			if err := reconciler.Run(ctx); err != nil {
				t.Errorf("Reconciler.Run: %v", err)
			}
		})
		stop := func() { cancel(); workers.Wait() }
		t.Cleanup(stop)
		require.Eventually(t, func() bool { return store.Health().Synced }, 10*time.Second, pollInterval)
		return store, stop
	}
	first, stopFirst := start()
	second, stopSecond := start()
	waitReady := func(namespace string) {
		require.Eventually(t, func() bool { return first.Lookup(namespace).Ready() && second.Lookup(namespace).Ready() }, 10*time.Second, pollInterval)
	}
	create := func(namespace string) (*corev1.ConfigMap, *corev1.Namespace) {
		cm, ns := readyObjects(namespace)
		ns.UID, cm.UID = "", ""
		cm.Namespace = policyNamespace
		cm.Annotations = map[string]string{"operator": "preserve"}
		ns, err = client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{})
		require.NoError(t, err)
		if namespace == "replicas" {
			cm.Immutable = new(true)
		}
		cm, err = client.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
		require.NoError(t, err)
		return cm, ns
	}
	t.Run("replicas associate and retry without another edit", func(t *testing.T) {
		failPatch.Store(true)
		cm, ns := create("replicas")
		require.Eventually(t, func() bool { return patchFailures.Load() >= 2 }, reconcileTimeout, pollInterval)
		failPatch.Store(false)
		waitReady(ns.Name)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"operator": "preserve", NamespaceUIDAnnotation: string(ns.UID)}, persisted.Annotations)
		require.Equal(t, cm.Data, persisted.Data)
		require.Equal(t, cm.Labels, persisted.Labels)
		require.Equal(t, cm.Immutable, persisted.Immutable)
	})
	t.Run("namespace recreation requires policy recreation", func(t *testing.T) {
		cm, ns := create("recreated")
		waitReady(ns.Name)
		require.NoError(t, client.CoreV1().Namespaces().Delete(t.Context(), ns.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).Reason == NamespaceUnavailable && second.Lookup(ns.Name).Reason == NamespaceUnavailable
		}, reconcileTimeout, pollInterval)
		terminating, err := client.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{})
		require.NoError(t, err)
		// envtest has no namespace controller; complete its finalization explicitly.
		terminating.Spec.Finalizers = nil
		_, err = client.CoreV1().Namespaces().Finalize(t.Context(), terminating, metav1.UpdateOptions{})
		require.NoError(t, err)
		replacement := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns.Name, Labels: ns.Labels}}
		replacement, err = client.CoreV1().Namespaces().Create(t.Context(), replacement, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).Reason == NamespaceUIDMismatch && second.Lookup(ns.Name).Reason == NamespaceUIDMismatch
		}, reconcileTimeout, pollInterval)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, string(ns.UID), persisted.Annotations[NamespaceUIDAnnotation])
		require.NotEqual(t, ns.UID, replacement.UID)
		require.NoError(t, client.CoreV1().ConfigMaps(cm.Namespace).Delete(t.Context(), cm.Name, metav1.DeleteOptions{}))
		cm.UID, cm.ResourceVersion = "", ""
		cm, err = client.CoreV1().ConfigMaps(cm.Namespace).Create(t.Context(), cm, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return first.Lookup(ns.Name).PolicyUID == cm.UID && second.Lookup(ns.Name).PolicyUID == cm.UID
		}, reconcileTimeout, pollInterval)
		waitReady(ns.Name)
		persisted, err = client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, string(replacement.UID), persisted.Annotations[NamespaceUIDAnnotation])
	})
	// Restart both replicas against the persisted association.
	stopFirst()
	stopSecond()
	t.Run("restart retains persisted associations", func(t *testing.T) {
		cm, ns := create("restart")
		cm.Annotations[NamespaceUIDAnnotation] = string(ns.UID)
		cm, err = client.CoreV1().ConfigMaps(cm.Namespace).Update(t.Context(), cm, metav1.UpdateOptions{})
		require.NoError(t, err)
		first, _ = start()
		second, _ = start()
		waitReady(ns.Name)
		persisted, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, cm.ResourceVersion, persisted.ResourceVersion, "an existing association needs no rewrite after restart")
	})
}

type reconcileTransport struct {
	next            http.RoundTripper
	failPatch       *atomic.Bool
	patchFailures   *atomic.Int64
	unexpectedCalls *atomic.Int64
}

func (r reconcileTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.Contains(request.URL.Path, "/serviceaccounts") || strings.HasPrefix(request.URL.Path, "/apis/rbac.authorization.k8s.io/") ||
		(request.Method != http.MethodGet && (request.Method != http.MethodPatch || !strings.Contains(request.URL.Path, "/configmaps/"))) {
		r.unexpectedCalls.Add(1)
	}
	if request.Method == http.MethodPatch && r.failPatch.Load() {
		r.patchFailures.Add(1)
		return nil, errors.New("injected temporary association write failure")
	}
	return r.next.RoundTrip(request)
}

func TestReconcilerRunAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cm, ns := readyObjects("team-a")
		cm.Annotations = nil
		store := staticStore(t, cm, ns)
		otherCM, otherNS := readyObjects("team-b")
		otherCM.Annotations = nil
		require.NoError(t, store.informers[policies].GetIndexer().Add(otherCM))
		require.NoError(t, store.informers[namespaces].GetIndexer().Add(otherNS))
		store.changed(policies, otherCM)

		var stalledError error
		var otherRequests atomic.Int32
		httpClient := restfake.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "/"+cm.Name) {
				<-request.Context().Done()
				stalledError = request.Context().Err()
				return nil, stalledError
			}
			otherRequests.Add(1)
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)),
			}, nil
		})
		client, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "http://cluster"}, httpClient)
		require.NoError(t, err)
		r, err := NewReconciler(client, store)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		defer func() { cancel(); <-done }()
		r.queue.Add(ns.Name)
		r.queue.Add(otherNS.Name)
		synctest.Wait()
		require.Zero(t, otherRequests.Load(), "the first request should still occupy the worker")

		time.Sleep(30 * time.Second)
		synctest.Wait()
		require.ErrorIs(t, stalledError, context.DeadlineExceeded)
		require.NoError(t, ctx.Err(), "attempt expiry must not cancel the worker")
		require.Equal(t, 1, r.queue.NumRequeues(ns.Name))
		require.EqualValues(t, 1, otherRequests.Load(), "the worker should move on to the next namespace")
	})
}

func TestReconcilerRunFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		unsynced   bool
		wantReason string
	}{
		{name: "forbidden patch", err: apierrors.NewForbidden(corev1.Resource("configmaps"), "private-policy", errors.New("private detail")), wantReason: "forbidden"},
		{name: "unauthorized patch", err: apierrors.NewUnauthorized("private detail"), wantReason: "unauthorized"},
		{name: "unavailable patch", err: errors.New("private detail"), wantReason: "unavailable"},
		{name: "conflict retries quietly", err: apierrors.NewConflict(corev1.Resource("configmaps"), "private-policy", errors.New("private detail"))},
		{name: "initial synchronization retries quietly", unsynced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm, ns := readyObjects("team-a")
			cm.Annotations = nil
			store := staticStore(t, cm.DeepCopy(), ns.DeepCopy())
			store.synced = !tc.unsynced
			client := fake.NewClientset(cm, ns)
			var attempts atomic.Int32
			client.PrependReactor("patch", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				if attempts.Add(1) == 1 && tc.err != nil {
					return true, nil, tc.err
				}
				return false, nil, nil
			})
			r, err := NewReconciler(client, store)
			require.NoError(t, err)
			var logsMu sync.Mutex
			var entries []string
			logger := funcr.NewJSON(func(entry string) {
				logsMu.Lock()
				defer logsMu.Unlock()
				entries = append(entries, entry)
			}, funcr.Options{})
			ctx, cancel := context.WithCancel(klog.NewContext(t.Context(), logger))
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			r.queue.Add(ns.Name)
			if tc.unsynced {
				require.Eventually(t, func() bool { return r.queue.NumRequeues(ns.Name) > 0 }, time.Second, time.Millisecond)
				store.mu.Lock()
				store.synced = true
				store.mu.Unlock()
			}
			require.Eventually(t, func() bool {
				policy, err := client.CoreV1().ConfigMaps(cm.Namespace).Get(t.Context(), cm.Name, metav1.GetOptions{})
				return err == nil && policy.Annotations[NamespaceUIDAnnotation] == string(ns.UID)
			}, time.Second, time.Millisecond)
			logsMu.Lock()
			defer logsMu.Unlock()
			if tc.wantReason == "" {
				require.Empty(t, entries)
				return
			}
			require.EqualValues(t, 2, attempts.Load())
			require.Len(t, entries, 1)
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(entries[0]), &fields))
			require.Equal(t, "authorization reconciliation failed", fields["msg"])
			require.Equal(t, ns.Name, fields["namespace"])
			require.Equal(t, tc.wantReason, fields["reason"])
			require.NotContains(t, entries[0], "private")
		})
	}
}
