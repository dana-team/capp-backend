package testutil

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/consts"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PolicyStore grants the fixture identity editor access to the supplied namespaces.
func PolicyStore(t *testing.T, namespaces ...string) *authorization.Store {
	t.Helper()
	var objects []runtime.Object
	seen := map[string]bool{}
	for _, name := range namespaces {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		uid := types.UID(name + "-uid")
		assignment, err := (authorization.Policy{
			Version:   authorization.PolicySchemaVersion,
			Namespace: name,
			Bindings:  []authorization.Binding{{Kind: rbacv1.UserKind, Name: "fixture", Role: authorization.RoleEditor}},
		}).ConfigMap("policy-system", nil, nil)
		require.NoError(t, err)
		assignment.UID = types.UID(name + "-policy")
		assignment.ResourceVersion = "1"
		assignment.Annotations = map[string]string{authorization.NamespaceUIDAnnotation: string(uid)}
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: uid,
			Labels: map[string]string{consts.ManagedNameSpaceLabelKey: consts.ManagedLabelValue},
		}}
		objects = append(objects, namespace, assignment)
	}
	store, err := authorization.NewStore(kubefake.NewClientset(objects...), authorization.StoreOptions{PolicyNamespace: "policy-system"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	require.Eventually(t, func() bool { return store.Health().Synced }, 5*time.Second, time.Millisecond)
	return store
}

// PolicyMiddleware uses the production authorizer with a verified fixture identity.
func PolicyMiddleware(t *testing.T, mgr cluster.ClusterManager, targets map[string]policy.Target) gin.HandlerFunc {
	t.Helper()
	rules, err := policy.NewRoutes("")
	require.NoError(t, err)
	identity := &MockAuthManager{AuthenticateFn: func(context.Context, string, *http.Request) (auth.ClusterCredential, error) {
		return auth.ClusterCredential{ImpersonateUser: "fixture"}, nil
	}}
	return policy.New(identity, config.AuthModeLDAP, mgr, rules, targets, zap.NewNop())
}

// EngineWithClient creates a resource router with policy-authorized backend execution.
// Aggregate routes use the supplied namespaces; namespaced routes authorize the request namespace.
func EngineWithClient(t *testing.T, execution client.Client, handler RouteRegistrar, aggregateNamespaces ...string) *gin.Engine {
	t.Helper()
	return EngineWithMetadata(t, execution, cluster.ClusterMeta{Name: "test"}, handler, aggregateNamespaces...)
}

// EngineWithMetadata adds cluster metadata to a policy-authorized resource router.
// Aggregate routes use the supplied namespaces; namespaced routes authorize the request namespace.
func EngineWithMetadata(t *testing.T, execution client.Client, metadata cluster.ClusterMeta, handler RouteRegistrar, aggregateNamespaces ...string) *gin.Engine {
	t.Helper()
	cc := &cluster.ClusterClient{Meta: metadata}
	cc.SetHealthy(true)
	mgr := &MockClusterManager{GetFn: func(string) (*cluster.ClusterClient, error) { return cc, nil }}
	aggregateStore := PolicyStore(t, aggregateNamespaces...)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		store := aggregateStore
		if namespace := c.Param("namespace"); namespace != "" {
			store = PolicyStore(t, namespace)
		}
		target := policy.Target{Store: store, ExecutionClient: execution}
		PolicyMiddleware(t, mgr, map[string]policy.Target{c.Param("cluster"): target})(c)
	})
	handler.RegisterRoutes(engine.Group("/api/v1/clusters/:cluster"))
	return engine
}

// ResourceMapper supplies list mappings used by namespace-bound fake clients.
func ResourceMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion, cappv1alpha1.GroupVersion, fissionv1.SchemeGroupVersion})
	for _, kind := range []string{"Pod", "PodList", "Secret", "SecretList", "ConfigMap", "ConfigMapList", "ResourceQuota", "ResourceQuotaList"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Namespace", "NamespaceList"} {
		mapper.Add(corev1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeRoot)
	}
	for _, kind := range []string{"Capp", "CappList", "CappRevision", "CappRevisionList"} {
		mapper.Add(cappv1alpha1.GroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	for _, kind := range []string{"Environment", "EnvironmentList", "Function", "FunctionList", "Package", "PackageList", "HTTPTrigger", "HTTPTriggerList", "TimeTrigger", "TimeTriggerList"} {
		mapper.Add(fissionv1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	return mapper
}
