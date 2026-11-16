package functions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cfuncsource"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const nsPath = "/namespaces/ns1/cfunc/functions"

func testSettings() Settings {
	return Settings{
		Functions: config.CfuncFunctionsConfig{
			Enabled:                true,
			Git:                    config.CfuncGitConfig{BaseURL: "https://git.example", Token: "s3cr3t-token", Username: "oauth2"},
			MaxTriggers:            4,
			MaxTimeoutSeconds:      300,
			DefaultConcurrency:     5,
			MaxConcurrency:         20,
			MinCronIntervalSeconds: 60,
		},
		AllowCustomBuild: true,
	}
}

func testCode() *cfuncsource.Archive {
	return &cfuncsource.Archive{Zip: []byte("zip-one"), SingleFile: []byte("file-one"), Commit: strings.Repeat("a", 40)}
}

func builderPool() *fissionv1.Environment {
	return &fissionv1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "py", Namespace: "ns1"},
		Spec: fissionv1.EnvironmentSpec{
			Version: 3,
			Runtime: fissionv1.Runtime{Image: "registry.example/python-env:1"},
			Builder: fissionv1.Builder{Image: "registry.example/python-builder:1", Command: "build"},
		},
	}
}

func plainPool() *fissionv1.Environment {
	pool := builderPool()
	pool.Spec.Builder = fissionv1.Builder{}
	return pool
}

type fakeFetcher struct {
	code  *cfuncsource.Archive
	err   error
	calls int
	delay time.Duration
}

func (f *fakeFetcher) CheckURL(repoURL string) error {
	if !strings.HasPrefix(repoURL, "https://git.example/") {
		return errors.New("source.url must be a repository under https://git.example")
	}
	return nil
}

func (f *fakeFetcher) Fetch(context.Context, string, string, string) (*cfuncsource.Archive, error) {
	f.calls++
	time.Sleep(f.delay)
	return f.code, f.err
}

type fixture struct {
	*testutil.EngineHelper
	client  client.Client
	fetcher *fakeFetcher
	handler *Handler
}

func newFixture(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) *fixture {
	t.Helper()
	k8sClient := fake.NewClientBuilder().
		WithScheme(testutil.TestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&fissionv1.Package{}).
		WithInterceptorFuncs(funcs).
		Build()
	fetcher := &fakeFetcher{code: testCode()}
	handler := &Handler{
		settings:   testSettings(),
		fetcher:    fetcher,
		routerURLs: map[string]string{},
		testClient: newTestClient(),
	}
	return &fixture{
		EngineHelper: testutil.NewEngineHelper(t, k8sClient, handler),
		client:       k8sClient,
		fetcher:      fetcher,
		handler:      handler,
	}
}

func basicRequest() FunctionRequest {
	return FunctionRequest{
		Name:   "shop",
		Pool:   "py",
		Source: FunctionSource{URL: "https://git.example/team/functions.git", Path: "shop"},
		Triggers: []FunctionTrigger{
			{Name: "api", Type: TriggerTypeHTTP, Path: "/orders"},
			{Name: "nightly", Type: TriggerTypeCron, Schedule: "0 2 * * *"},
		},
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v), w.Body.String())
	return v
}

func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Error apierrors.APIError `json:"error"`
	}](t, w).Error.Message
}

func get[T client.Object](t *testing.T, k8sClient client.Client, name string, object T) T {
	t.Helper()
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: name}, object))
	return object
}

func exists(t *testing.T, k8sClient client.Client, name string, object client.Object) bool {
	t.Helper()
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: name}, object)
	require.NoError(t, client.IgnoreNotFound(err))
	return err == nil
}

func TestCreate_WritesAllObjects(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())

	w := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	resp := decode[FunctionResponse](t, w)
	assert.Equal(t, "shop", resp.Name)
	assert.Equal(t, strings.Repeat("a", 40), resp.Source.Commit)
	assert.Equal(t, 60, resp.TimeoutSeconds)
	assert.Nil(t, resp.Advanced, "defaults must not come back as advanced values")
	require.Len(t, resp.Triggers, 2)
	assert.Equal(t, "/ns1/shop/orders", resp.Triggers[0].FullPath)
	assert.Empty(t, resp.Triggers[0].URL, "no router URL is configured")

	fn := get(t, f.client, "shop", &fissionv1.Function{})
	assert.Equal(t, fissionv1.ExecutorTypePoolmgr, fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType)
	assert.Equal(t, fissionv1.StrategyTypeExecution, string(fn.Spec.InvokeStrategy.StrategyType))
	assert.Equal(t, 5, fn.Spec.Concurrency, "the backend default, not Fission's 500")
	assert.Equal(t, "shop", fn.Spec.Package.PackageRef.Name)
	assert.NotEmpty(t, fn.Spec.Package.PackageRef.ResourceVersion)

	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.Equal(t, []byte("zip-one"), pkg.Spec.Source.Literal, "a pool with a build image gets source")
	assert.True(t, pkg.Spec.Deployment.IsEmpty())
	assert.Equal(t, "shop", pkg.Annotations[SourcePathAnnotation])
	assert.NotEmpty(t, pkg.Annotations[BuildRequestAnnotation])
	require.Len(t, pkg.OwnerReferences, 1)
	assert.Equal(t, fn.UID, pkg.OwnerReferences[0].UID)

	route := get(t, f.client, "shop-api", &fissionv1.HTTPTrigger{})
	assert.Equal(t, "/ns1/shop/orders", route.Spec.RelativeURL)
	assert.Equal(t, []string{"GET"}, route.Spec.Methods)
	assert.Equal(t, "shop", route.Spec.FunctionReference.Name)
	require.Len(t, route.OwnerReferences, 1)

	timer := get(t, f.client, "shop-nightly", &fissionv1.TimeTrigger{})
	assert.Equal(t, "0 2 * * *", timer.Spec.Cron)
	assert.Equal(t, "shop", timer.Spec.Name)
}

func TestCreate_PoolWithoutBuilderGetsDeploymentArchive(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, plainPool())

	w := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.True(t, pkg.Spec.Source.IsEmpty())
	assert.Equal(t, []byte("file-one"), pkg.Spec.Deployment.Literal, "a folder with one file is stored as that file")
	assert.NotContains(t, pkg.Annotations, BuildRequestAnnotation)
}

func TestCreate_Rejected(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*FunctionRequest)
		objects    []client.Object
		wantStatus int
		errContain string
	}{
		{
			name:       "pool missing",
			objects:    []client.Object{},
			wantStatus: http.StatusBadRequest,
			errContain: "pool py does not exist in namespace ns1",
		},
		{
			name: "pool below version 3",
			objects: func() []client.Object {
				pool := builderPool()
				pool.Spec.Version = 1
				return []client.Object{pool}
			}(),
			wantStatus: http.StatusBadRequest,
			errContain: "version 1 Environment",
		},
		{
			name:       "function exists",
			objects:    []client.Object{builderPool(), &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}},
			wantStatus: http.StatusConflict,
			errContain: "already exists",
		},
		{
			name:       "url outside the base",
			mutate:     func(r *FunctionRequest) { r.Source.URL = "https://evil.example/x.git" },
			wantStatus: http.StatusBadRequest,
			errContain: "must be a repository under",
		},
		{
			name:       "no url",
			mutate:     func(r *FunctionRequest) { r.Source.URL = "" },
			wantStatus: http.StatusBadRequest,
			errContain: "source.url is required",
		},
		{
			name:       "path leaves the repository",
			mutate:     func(r *FunctionRequest) { r.Source.Path = "../x" },
			wantStatus: http.StatusBadRequest,
			errContain: "inside the repository",
		},
		{
			name:       "timeout above the maximum",
			mutate:     func(r *FunctionRequest) { r.TimeoutSeconds = new(301) },
			wantStatus: http.StatusBadRequest,
			errContain: "timeoutSeconds must be between 1 and 300",
		},
		{
			name:       "cron each second",
			mutate:     func(r *FunctionRequest) { r.Triggers[1].Schedule = "* * * * * *" },
			wantStatus: http.StatusBadRequest,
			errContain: "runs more often than one time in 60 seconds",
		},
		{
			name:       "cron every 30 seconds",
			mutate:     func(r *FunctionRequest) { r.Triggers[1].Schedule = "@every 30s" },
			wantStatus: http.StatusBadRequest,
			errContain: "runs more often",
		},
		{
			name:       "cron with a burst inside the hour",
			mutate:     func(r *FunctionRequest) { r.Triggers[1].Schedule = "0,30 0 * * * *" },
			wantStatus: http.StatusBadRequest,
			errContain: "runs more often",
		},
		{
			name:       "cron that does not parse",
			mutate:     func(r *FunctionRequest) { r.Triggers[1].Schedule = "not a schedule" },
			wantStatus: http.StatusBadRequest,
			errContain: "triggers[1]: schedule",
		},
		{
			name:       "unknown trigger type",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Type = "queue" },
			wantStatus: http.StatusBadRequest,
			errContain: `type must be "http" or "cron"`,
		},
		{
			name:       "duplicate trigger name",
			mutate:     func(r *FunctionRequest) { r.Triggers[1].Name = "api" },
			wantStatus: http.StatusBadRequest,
			errContain: "used by more than one trigger",
		},
		{
			name:       "trigger without a name",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Name = "" },
			wantStatus: http.StatusBadRequest,
			errContain: "triggers[0].name",
		},
		{
			name: "too many triggers",
			mutate: func(r *FunctionRequest) {
				for _, name := range []string{"a", "b", "c"} {
					r.Triggers = append(r.Triggers, FunctionTrigger{Name: name, Type: TriggerTypeHTTP, Path: "/" + name})
				}
			},
			wantStatus: http.StatusBadRequest,
			errContain: "at most 4 triggers",
		},
		{
			name:       "path that climbs out of the prefix",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Path = "/../other" },
			wantStatus: http.StatusBadRequest,
			errContain: "triggers[0]: path",
		},
		{
			name:       "path without a slash",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Path = "orders" },
			wantStatus: http.StatusBadRequest,
			errContain: "must start with a slash",
		},
		{
			name:       "lowercase method",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Methods = []string{"get"} },
			wantStatus: http.StatusBadRequest,
			errContain: `"get" is not one of`,
		},
		{
			name:       "schedule on an http trigger",
			mutate:     func(r *FunctionRequest) { r.Triggers[0].Schedule = "@hourly" },
			wantStatus: http.StatusBadRequest,
			errContain: "schedule belongs to a cron trigger",
		},
		{
			name: "secret with another mount path",
			mutate: func(r *FunctionRequest) {
				r.SecretVolumes = []SecretVolume{{SecretName: "db", MountPath: "/etc/db"}}
			},
			wantStatus: http.StatusBadRequest,
			errContain: `mountPath must be empty or "/secrets/ns1/db"`,
		},
		{
			name: "secret listed two times",
			mutate: func(r *FunctionRequest) {
				r.SecretVolumes = []SecretVolume{{SecretName: "db"}, {SecretName: "db"}}
			},
			wantStatus: http.StatusBadRequest,
			errContain: "listed more than once",
		},
		{
			name:       "config map without a name",
			mutate:     func(r *FunctionRequest) { r.ConfigMapVolumes = []ConfigMapVolume{{}} },
			wantStatus: http.StatusBadRequest,
			errContain: "configMapVolumes[0].configMapName is required",
		},
		{
			name:       "concurrency above the maximum",
			mutate:     func(r *FunctionRequest) { r.Advanced = &FunctionAdvanced{Concurrency: new(21)} },
			wantStatus: http.StatusBadRequest,
			errContain: "advanced.concurrency must be between 1 and 20",
		},
		{
			name:       "retained pods above the concurrency",
			mutate:     func(r *FunctionRequest) { r.Advanced = &FunctionAdvanced{RetainPods: new(6)} },
			wantStatus: http.StatusBadRequest,
			errContain: "advanced.retainPods must be between 0 and 5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := tc.objects
			if objects == nil {
				objects = []client.Object{builderPool()}
			}
			f := newFixture(t, interceptor.Funcs{}, objects...)
			req := basicRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}

			w := f.PostJSON(nsPath, req)

			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.Contains(t, errorMessage(t, w), tc.errContain)
			if tc.wantStatus == http.StatusBadRequest && tc.objects == nil {
				assert.Zero(t, f.fetcher.calls, "a request that fails validation must not reach the git server")
				assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}), "nothing may be written")
			}
		})
	}
}

func TestCreate_BuildChoice(t *testing.T) {
	oneFile := &cfuncsource.Archive{Zip: []byte("zip-one"), SingleFile: []byte("file-one"), IsFile: true, Commit: strings.Repeat("a", 40)}
	tests := []struct {
		name       string
		pool       *fissionv1.Environment
		code       *cfuncsource.Archive
		build      *bool
		wantSource bool
		wantStatus int
	}{
		{name: "folder in a pool with a build image is built", pool: builderPool(), code: testCode(), wantSource: true},
		{name: "one file is loaded as it is", pool: builderPool(), code: oneFile},
		{name: "one file with build true is built", pool: builderPool(), code: oneFile, build: new(true), wantSource: true},
		{name: "folder with build false is loaded as it is", pool: builderPool(), code: testCode(), build: new(false)},
		{name: "folder in a pool with no build image is loaded as it is", pool: plainPool(), code: testCode()},
		{name: "build true in a pool with no build image", pool: plainPool(), code: testCode(), build: new(true), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, interceptor.Funcs{}, tc.pool)
			f.fetcher.code = tc.code
			req := basicRequest()
			req.Source.Build = tc.build

			w := f.PostJSON(nsPath, req)

			if tc.wantStatus != 0 {
				require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
				assert.Contains(t, errorMessage(t, w), "source.build needs a pool that builds code")
				return
			}
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			pkg := get(t, f.client, "shop", &fissionv1.Package{})
			assert.Equal(t, tc.wantSource, !pkg.Spec.Source.IsEmpty())
			assert.Equal(t, !tc.wantSource, !pkg.Spec.Deployment.IsEmpty())
			require.NotNil(t, decode[FunctionResponse](t, w).Source.Build)
			assert.Equal(t, tc.wantSource, *decode[FunctionResponse](t, w).Source.Build, "a response always says whether the code is built")
		})
	}
}

func TestCreate_BuildCommandRules(t *testing.T) {
	req := basicRequest()
	req.Advanced = &FunctionAdvanced{BuildCommand: "./build.sh"}

	t.Run("refused when custom builds are off", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, builderPool())
		f.handler.settings.AllowCustomBuild = false

		w := f.PostJSON(nsPath, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, errorMessage(t, w), "does not enable custom builds")
	})

	t.Run("refused for a pool with no build image", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, plainPool())

		w := f.PostJSON(nsPath, req)

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, errorMessage(t, w), "needs code that is built")
		assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}))
	})
}

func TestCreate_GitFailures(t *testing.T) {
	t.Run("a problem with the request is 400", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, builderPool())
		_, f.fetcher.err = cfuncsource.CleanPath("../x")

		w := f.PostJSON(nsPath, basicRequest())

		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("an unreachable server is 502 and hides the cause", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, builderPool())
		f.fetcher.err = errors.New("dial tcp 10.0.0.1: token s3cr3t-token rejected")

		w := f.PostJSON(nsPath, basicRequest())

		require.Equal(t, http.StatusBadGateway, w.Code)
		assert.NotContains(t, w.Body.String(), "s3cr3t-token")
		assert.NotContains(t, w.Body.String(), "10.0.0.1")
		assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}), "nothing may be written")
	})
}

// A create that fails after the package is written leaves a package with no
// function. The same request again must adopt it and finish.
func TestCreate_RepeatAfterFunctionWriteFailed(t *testing.T) {
	fail := true
	f := newFixture(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isFunction := obj.(*fissionv1.Function); isFunction && fail {
				return errors.New("api server went away")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, builderPool())

	first := f.PostJSON(nsPath, basicRequest())
	require.Equal(t, http.StatusInternalServerError, first.Code)
	require.True(t, exists(t, f.client, "shop", &fissionv1.Package{}))

	fail = false
	second := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	require.Len(t, pkg.OwnerReferences, 1)
	assert.True(t, exists(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}))
}

// A trigger failure leaves the function saved. The response must say so, and
// the same body in a PUT must finish the triggers.
func TestCreate_TriggerFailureIsPartialAndRepeatable(t *testing.T) {
	fail := true
	f := newFixture(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isTimer := obj.(*fissionv1.TimeTrigger); isTimer && fail {
				return errors.New("api server went away")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, builderPool())

	first := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusInternalServerError, first.Code)
	message := errorMessage(t, first)
	assert.Contains(t, message, "function shop is saved")
	assert.Contains(t, message, "nightly")
	assert.NotContains(t, message, "api server went away")
	assert.True(t, exists(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}), "one failed trigger must not hold back the others")

	fail = false
	second := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.True(t, exists(t, f.client, "shop-nightly", &fissionv1.TimeTrigger{}))
}

func TestCreate_TriggerNameOfAnotherFunctionIsConflict(t *testing.T) {
	foreign := &fissionv1.HTTPTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-api", Namespace: "ns1"},
		Spec: fissionv1.HTTPTriggerSpec{
			RelativeURL:       "/elsewhere",
			FunctionReference: fissionv1.FunctionReference{Type: fissionv1.FunctionReferenceTypeFunctionName, Name: "other"},
		},
	}
	f := newFixture(t, interceptor.Funcs{}, builderPool(), foreign)

	w := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, errorMessage(t, w), "api")
	assert.Equal(t, "/elsewhere", get(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}).Spec.RelativeURL,
		"the trigger of the other function must not be changed")
}

func TestUpdate_ReplacesTriggers(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

	req := basicRequest()
	req.Name = "ignored-the-url-wins"
	req.Triggers = []FunctionTrigger{
		{Name: "api", Type: TriggerTypeHTTP, Path: "/orders", Methods: []string{"POST"}},
		{Name: "hourly", Type: TriggerTypeCron, Schedule: "@hourly"},
	}
	w := f.PutJSON(nsPath+"/shop", req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"POST"}, get(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}).Spec.Methods)
	assert.True(t, exists(t, f.client, "shop-hourly", &fissionv1.TimeTrigger{}))
	assert.False(t, exists(t, f.client, "shop-nightly", &fissionv1.TimeTrigger{}), "a trigger the body leaves out is deleted")
	resp := decode[FunctionResponse](t, w)
	assert.Equal(t, "shop", resp.Name)
	require.Len(t, resp.Triggers, 2)
}

func TestUpdate_NotFound(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())

	w := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdate_OtherExecutorIsConflict(t *testing.T) {
	foreign := &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
	foreign.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType = fissionv1.ExecutorTypeNewdeploy
	f := newFixture(t, interceptor.Funcs{}, builderPool(), foreign)

	w := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, errorMessage(t, w), "newdeploy executor")
}

// Fission builds a package again only when its status is set to pending.
func TestUpdate_NewCodeRequestsABuild(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	built := get(t, f.client, "shop", &fissionv1.Package{})
	built.Status.BuildStatus = fissionv1.BuildStatusSucceeded
	built.Status.BuildLog = "old log"
	require.NoError(t, f.client.Status().Update(context.Background(), built))
	firstRequest := built.Annotations[BuildRequestAnnotation]
	firstRef := get(t, f.client, "shop", &fissionv1.Function{}).Spec.Package.PackageRef.ResourceVersion

	t.Run("the same commit asks for no build", func(t *testing.T) {
		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		pkg := get(t, f.client, "shop", &fissionv1.Package{})
		assert.Equal(t, fissionv1.BuildStatusSucceeded, string(pkg.Status.BuildStatus))
		assert.Equal(t, built.ResourceVersion, pkg.ResourceVersion, "an unchanged package must not be written")
	})

	t.Run("a new commit asks for a build", func(t *testing.T) {
		f.fetcher.code = &cfuncsource.Archive{Zip: []byte("zip-two"), Commit: strings.Repeat("b", 40)}

		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		pkg := get(t, f.client, "shop", &fissionv1.Package{})
		assert.Equal(t, []byte("zip-two"), pkg.Spec.Source.Literal)
		assert.Equal(t, fissionv1.BuildStatusPending, string(pkg.Status.BuildStatus))
		assert.Empty(t, pkg.Status.BuildLog)
		assert.NotEqual(t, firstRequest, pkg.Annotations[BuildRequestAnnotation])
		assert.Equal(t, strings.Repeat("b", 40), decode[FunctionResponse](t, w).Source.Commit)
		fn := get(t, f.client, "shop", &fissionv1.Function{})
		assert.Equal(t, firstRef, fn.Spec.Package.PackageRef.ResourceVersion,
			"the function must keep pointing at the last good build until Fission's builder moves it")
		assert.NotContains(t, fn.Annotations, CodeAnnotation)
	})
}

// The code and the build request are two writes. When the second one fails,
// the same PUT again must still ask for the build.
func TestUpdate_RepeatAfterBuildRequestFailed(t *testing.T) {
	fail := false
	f := newFixture(t, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if fail {
				return errors.New("api server went away")
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	built := get(t, f.client, "shop", &fissionv1.Package{})
	built.Status.BuildStatus = fissionv1.BuildStatusSucceeded
	require.NoError(t, f.client.Status().Update(context.Background(), built))

	f.fetcher.code = &cfuncsource.Archive{Zip: []byte("zip-two"), Commit: strings.Repeat("b", 40)}
	fail = true
	first := f.PutJSON(nsPath+"/shop", basicRequest())
	require.Equal(t, http.StatusInternalServerError, first.Code)
	half := get(t, f.client, "shop", &fissionv1.Package{})
	require.Equal(t, []byte("zip-two"), half.Spec.Source.Literal)
	require.Equal(t, fissionv1.BuildStatusSucceeded, string(half.Status.BuildStatus))

	fail = false
	second := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, fissionv1.BuildStatusPending, string(get(t, f.client, "shop", &fissionv1.Package{}).Status.BuildStatus))
}

// A function that changes from built code to code that is loaded as it is
// must not keep the build state of the earlier build: Fission does not load
// a package whose state is failed or pending.
func TestUpdate_NoBuildClearsAnOldBuildState(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	failed := get(t, f.client, "shop", &fissionv1.Package{})
	failed.Status.BuildStatus = fissionv1.BuildStatusFailed
	failed.Status.BuildLog = "npm error"
	require.NoError(t, f.client.Status().Update(context.Background(), failed))

	req := basicRequest()
	req.Source.Build = new(false)
	w := f.PutJSON(nsPath+"/shop", req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.True(t, pkg.Spec.Source.IsEmpty())
	assert.Equal(t, fissionv1.BuildStatusNone, string(pkg.Status.BuildStatus))
	assert.Empty(t, pkg.Status.BuildLog)
	assert.NotContains(t, pkg.Annotations, BuildRequestAnnotation)
	assert.Equal(t, FunctionBuild{Status: "none"}, decode[FunctionResponse](t, w).Build)
}

// The limit applies to the bytes that are stored. A file that compresses
// well has a small zip, but it is stored as the file when it is not built.
func TestCreate_SizeLimitUsesTheStoredBytes(t *testing.T) {
	limit := int(fissionv1.ArchiveLiteralSizeLimit)
	tests := []struct {
		name    string
		code    *cfuncsource.Archive
		build   bool
		wantErr string
	}{
		{
			name:    "one large file that is not built",
			code:    &cfuncsource.Archive{Zip: []byte("small zip"), SingleFile: make([]byte, limit+1), IsFile: true},
			wantErr: "bytes as it is stored; the limit is 262144 bytes",
		},
		{
			name: "the same file within the limit",
			code: &cfuncsource.Archive{Zip: []byte("small zip"), SingleFile: make([]byte, limit), IsFile: true},
		},
		{
			name:    "built code whose zip is over the limit",
			code:    &cfuncsource.Archive{Zip: make([]byte, limit+1), SingleFile: []byte("small file"), IsFile: true},
			build:   true,
			wantErr: "bytes as it is stored",
		},
		{
			name:  "built code whose zip is within the limit",
			code:  &cfuncsource.Archive{Zip: make([]byte, limit), SingleFile: make([]byte, limit+1), IsFile: true},
			build: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, interceptor.Funcs{}, builderPool())
			tc.code.Commit = strings.Repeat("a", 40)
			f.fetcher.code = tc.code
			req := basicRequest()
			req.Source.Build = &tc.build

			w := f.PostJSON(nsPath, req)

			if tc.wantErr == "" {
				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
				return
			}
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, errorMessage(t, w), tc.wantErr)
			assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}), "nothing may be written")
		})
	}
}

func TestCreate_TwoTriggersOnOneRouteAreRefused(t *testing.T) {
	tests := []struct {
		name     string
		triggers []FunctionTrigger
		wantErr  string
	}{
		{
			name: "same path and method",
			triggers: []FunctionTrigger{
				{Name: "a", Type: TriggerTypeHTTP, Path: "/x"},
				{Name: "b", Type: TriggerTypeHTTP, Path: "/x", Methods: []string{"POST", "GET"}},
			},
			wantErr: `triggers[1]: GET on path "/x" is already served by trigger "a"`,
		},
		{
			name: "empty path and a slash are the same path",
			triggers: []FunctionTrigger{
				{Name: "a", Type: TriggerTypeHTTP},
				{Name: "b", Type: TriggerTypeHTTP, Path: "/"},
			},
			wantErr: `is already served by trigger "a"`,
		},
		{
			name: "same path with other methods is allowed",
			triggers: []FunctionTrigger{
				{Name: "a", Type: TriggerTypeHTTP, Path: "/x"},
				{Name: "b", Type: TriggerTypeHTTP, Path: "/x", Methods: []string{"POST"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, interceptor.Funcs{}, builderPool())
			req := basicRequest()
			req.Triggers = tc.triggers

			w := f.PostJSON(nsPath, req)

			if tc.wantErr == "" {
				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
				return
			}
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, errorMessage(t, w), tc.wantErr)
			assert.Zero(t, f.fetcher.calls)
		})
	}
}

// A Package of the function's name that this API did not make can hold the
// code of other functions, so it is not taken over.
func TestForeignPackageIsNotReplaced(t *testing.T) {
	foreign := func() *fissionv1.Package {
		return &fissionv1.Package{
			ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"},
			Spec:       fissionv1.PackageSpec{Deployment: fissionv1.Archive{Type: fissionv1.ArchiveTypeLiteral, Literal: []byte("theirs")}},
		}
	}

	t.Run("create", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{}, builderPool(), foreign())

		w := f.PostJSON(nsPath, basicRequest())

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, errorMessage(t, w), "was not created through this API")
		assert.Equal(t, []byte("theirs"), get(t, f.client, "shop", &fissionv1.Package{}).Spec.Deployment.Literal)
		assert.False(t, exists(t, f.client, "shop", &fissionv1.Function{}))
	})

	t.Run("update of a function that uses another package", func(t *testing.T) {
		fn := &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
		fn.Spec.Package.PackageRef = fissionv1.PackageRef{Namespace: "ns1", Name: "shop-1234"}
		f := newFixture(t, interceptor.Funcs{}, builderPool(), foreign(), fn)

		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Equal(t, []byte("theirs"), get(t, f.client, "shop", &fissionv1.Package{}).Spec.Deployment.Literal)
	})
}

// A build can fail for a cause outside the code, so the same body in a PUT
// must ask for it again.
func TestUpdate_FailedBuildIsRequestedAgain(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	failed := get(t, f.client, "shop", &fissionv1.Package{})
	failed.Status.BuildStatus = fissionv1.BuildStatusFailed
	failed.Status.BuildLog = "registry timeout"
	require.NoError(t, f.client.Status().Update(context.Background(), failed))

	w := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.Equal(t, fissionv1.BuildStatusPending, string(pkg.Status.BuildStatus))
	assert.Empty(t, pkg.Status.BuildLog)
}

// Fission's builder writes to a package while it builds. An update that
// meets that write must apply again, and keep what the builder wrote.
func TestUpdate_RetriesWhenTheBuilderWroteThePackage(t *testing.T) {
	builderWrote := false
	f := newFixture(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if pkg, isPackage := obj.(*fissionv1.Package); isPackage && !builderWrote {
				builderWrote = true
				var stored fissionv1.Package
				if err := c.Get(ctx, client.ObjectKeyFromObject(pkg), &stored); err != nil {
					return err
				}
				stored.Spec.Deployment = fissionv1.Archive{Type: fissionv1.ArchiveTypeUrl, URL: "http://storagesvc/built"}
				if err := c.Update(ctx, &stored); err != nil {
					return err
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

	f.fetcher.code = &cfuncsource.Archive{Zip: []byte("zip-two"), Commit: strings.Repeat("b", 40)}
	w := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, builderWrote)
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.Equal(t, []byte("zip-two"), pkg.Spec.Source.Literal, "the new code must be stored")
	assert.Equal(t, "http://storagesvc/built", pkg.Spec.Deployment.URL, "the builder's output must be kept")
	assert.Equal(t, fissionv1.BuildStatusPending, string(pkg.Status.BuildStatus))
}

// Fission writes the status of a Function and of a trigger at any time. An
// update that was prepared before such a write must still go through.
func TestUpdate_RetriesOverStatusWritesOfFissionControllers(t *testing.T) {
	bumped := map[string]bool{}
	f := newFixture(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			_, isFunction := obj.(*fissionv1.Function)
			_, isRoute := obj.(*fissionv1.HTTPTrigger)
			if key := obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName(); (isFunction || isRoute) && !bumped[key] {
				bumped[key] = true
				stored, _ := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
					return err
				}
				stored.SetLabels(map[string]string{"written-by": "a-controller"})
				if err := c.Update(ctx, stored); err != nil {
					return err
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

	req := basicRequest()
	req.TimeoutSeconds = new(90)
	req.Triggers[0].Methods = []string{"POST"}
	w := f.PutJSON(nsPath+"/shop", req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, bumped, 2, "both conflicts must have happened")
	assert.Equal(t, 90, get(t, f.client, "shop", &fissionv1.Function{}).Spec.FunctionTimeout)
	assert.Equal(t, []string{"POST"}, get(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}).Spec.Methods)
}

// The package is written before the Function. When the Function write
// fails, the same PUT again finds the package already as wanted. It must
// still point the Function at the new package version, or Fission keeps
// serving the old code from the pods it has.
func TestUpdate_RepeatAfterFunctionWriteFailedPointsAtTheNewCode(t *testing.T) {
	fail := false
	f := newFixture(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, isFunction := obj.(*fissionv1.Function); isFunction && fail {
				return errors.New("api server went away")
			}
			return c.Update(ctx, obj, opts...)
		},
	}, plainPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	firstRef := get(t, f.client, "shop", &fissionv1.Function{}).Spec.Package.PackageRef.ResourceVersion

	f.fetcher.code = &cfuncsource.Archive{Zip: []byte("zip-two"), SingleFile: []byte("file-two"), Commit: strings.Repeat("b", 40)}
	fail = true
	first := f.PutJSON(nsPath+"/shop", basicRequest())
	require.Equal(t, http.StatusInternalServerError, first.Code)
	require.Equal(t, []byte("file-two"), get(t, f.client, "shop", &fissionv1.Package{}).Spec.Deployment.Literal)
	require.Equal(t, firstRef, get(t, f.client, "shop", &fissionv1.Function{}).Spec.Package.PackageRef.ResourceVersion)

	fail = false
	second := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	fn := get(t, f.client, "shop", &fissionv1.Function{})
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	assert.Equal(t, pkg.ResourceVersion, fn.Spec.Package.PackageRef.ResourceVersion, "the function must point at the package that holds the new code")
	assert.NotEqual(t, firstRef, fn.Spec.Package.PackageRef.ResourceVersion)
}

func TestCreate_BusyBackendIs503(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	f.fetcher.err = fmt.Errorf("%w: %w", cfuncsource.ErrBusy, context.DeadlineExceeded)

	w := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, errorMessage(t, w), "busy cloning other repositories")
	assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}), "nothing may be written")
}

// While a build runs, Fission's fetcher refuses the package. A Function that
// is pointed at it loses its pods, so for built code only Fission's builder
// moves the reference, when the build succeeds.
func TestUpdate_BuiltCodeLeavesTheReferenceToTheBuilder(t *testing.T) {
	oneFile := &cfuncsource.Archive{Zip: []byte("zip-one"), SingleFile: []byte("file-one"), IsFile: true, Commit: strings.Repeat("a", 40)}
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	f.fetcher.code = oneFile
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	unbuilt := get(t, f.client, "shop", &fissionv1.Function{})
	require.Contains(t, unbuilt.Annotations, CodeAnnotation, "unbuilt code records what the function points at")

	t.Run("a change to built code keeps the reference and drops the record", func(t *testing.T) {
		req := basicRequest()
		req.Source.Build = new(true)

		w := f.PutJSON(nsPath+"/shop", req)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		fn := get(t, f.client, "shop", &fissionv1.Function{})
		assert.Equal(t, unbuilt.Spec.Package.PackageRef.ResourceVersion, fn.Spec.Package.PackageRef.ResourceVersion)
		assert.NotContains(t, fn.Annotations, CodeAnnotation)
		assert.Equal(t, fissionv1.BuildStatusPending, string(get(t, f.client, "shop", &fissionv1.Package{}).Status.BuildStatus))
	})

	t.Run("a change back to the same unbuilt code is seen as a change", func(t *testing.T) {
		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		fn := get(t, f.client, "shop", &fissionv1.Function{})
		pkg := get(t, f.client, "shop", &fissionv1.Package{})
		assert.Equal(t, pkg.ResourceVersion, fn.Spec.Package.PackageRef.ResourceVersion, "unbuilt code is ready at once, so the function is pointed at it")
		assert.Contains(t, fn.Annotations, CodeAnnotation)
	})
}

// A function made outside this API can use a hand-made Package of its own
// name that other functions share. Delete must leave it, as create and
// update refuse to replace it.
func TestDelete_LeavesAPackageThisAPIDidNotMake(t *testing.T) {
	pkg := &fissionv1.Package{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
	fn := &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
	fn.Spec.Package.PackageRef = fissionv1.PackageRef{Namespace: "ns1", Name: "shop"}
	f := newFixture(t, interceptor.Funcs{}, builderPool(), pkg, fn)

	w := f.Delete(nsPath + "/shop")

	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.False(t, exists(t, f.client, "shop", &fissionv1.Function{}))
	assert.True(t, exists(t, f.client, "shop", &fissionv1.Package{}), "a package this API did not make must stay")
}

// The function can reference a package that is gone. The package named
// after the function must still be found, adopted when this API made it and
// refused with the reason when it did not.
func TestUpdate_FindsItsPackageWhenTheReferencedOneIsMissing(t *testing.T) {
	function := func() *fissionv1.Function {
		fn := &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
		fn.Spec.Package.PackageRef = fissionv1.PackageRef{Namespace: "ns1", Name: "deleted-package"}
		return fn
	}

	t.Run("a package this API made is adopted", func(t *testing.T) {
		own := &fissionv1.Package{ObjectMeta: metav1.ObjectMeta{
			Name: "shop", Namespace: "ns1", Annotations: map[string]string{SourceURLAnnotation: "https://git.example/old.git"},
		}}
		f := newFixture(t, interceptor.Funcs{}, builderPool(), function(), own)

		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, []byte("zip-one"), get(t, f.client, "shop", &fissionv1.Package{}).Spec.Source.Literal)
		assert.Equal(t, "shop", get(t, f.client, "shop", &fissionv1.Function{}).Spec.Package.PackageRef.Name)
	})

	t.Run("a package made another way is refused with the reason", func(t *testing.T) {
		foreign := &fissionv1.Package{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}}
		f := newFixture(t, interceptor.Funcs{}, builderPool(), function(), foreign)

		w := f.PutJSON(nsPath+"/shop", basicRequest())

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, errorMessage(t, w), "was not created through this API")
	})
}

// Triggers can name a function before it exists: left by a delete with
// orphans, or made with the fission CLI. They are this function's triggers.
func TestCreate_TakesOverTriggersThatAlreadyNameTheFunction(t *testing.T) {
	ref := fissionv1.FunctionReference{Type: fissionv1.FunctionReferenceTypeFunctionName, Name: "shop"}
	sameName := &fissionv1.HTTPTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-api", Namespace: "ns1"},
		Spec:       fissionv1.HTTPTriggerSpec{RelativeURL: "/old", FunctionReference: ref},
	}
	stale := &fissionv1.HTTPTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "left-over", Namespace: "ns1"},
		Spec:       fissionv1.HTTPTriggerSpec{RelativeURL: "/left-over", FunctionReference: ref},
	}
	staleTimer := &fissionv1.TimeTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "old-timer", Namespace: "ns1"},
		Spec:       fissionv1.TimeTriggerSpec{Cron: "@hourly", FunctionReference: ref},
	}
	f := newFixture(t, interceptor.Funcs{}, builderPool(), sameName, stale, staleTimer)

	w := f.PostJSON(nsPath, basicRequest())

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "/ns1/shop/orders", get(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}).Spec.RelativeURL, "the trigger of the same name is updated, not reported as taken")
	assert.False(t, exists(t, f.client, "left-over", &fissionv1.HTTPTrigger{}), "a trigger the request does not list must not stay live")
	assert.False(t, exists(t, f.client, "old-timer", &fissionv1.TimeTrigger{}))
	assert.Len(t, decode[FunctionResponse](t, w).Triggers, 2)
}

// A trigger "x" made outside this API and the trigger "<function>-x" made
// by it, or two kinds with one object name, must not get the same API name.
func TestGet_TriggerNamesAreUnique(t *testing.T) {
	ref := fissionv1.FunctionReference{Type: fissionv1.FunctionReferenceTypeFunctionName, Name: "shop"}
	foreign := &fissionv1.HTTPTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns1"},
		Spec:       fissionv1.HTTPTriggerSpec{RelativeURL: "/elsewhere", Methods: []string{"GET"}, FunctionReference: ref},
	}
	otherKind := &fissionv1.TimeTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-api", Namespace: "ns1"},
		Spec:       fissionv1.TimeTriggerSpec{Cron: "@daily", FunctionReference: ref},
	}
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	f.handler.routerURLs[""] = "https://fn.example"
	req := basicRequest()
	req.Triggers = req.Triggers[:1]
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, req).Code)
	require.NoError(t, f.client.Create(context.Background(), foreign))
	require.NoError(t, f.client.Create(context.Background(), otherKind))

	w := f.Get(nsPath + "/shop")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decode[FunctionResponse](t, w)
	names := map[string]string{}
	for _, trigger := range resp.Triggers {
		require.NotContains(t, names, trigger.Name, "two triggers must not share an API name")
		names[trigger.Name] = trigger.Type + " " + trigger.FullPath
	}
	require.Len(t, names, 3)
	assert.Equal(t, "http /ns1/shop/orders", names["api"], "the trigger this API named keeps its name")

	t.Run("the response can be sent back in a PUT", func(t *testing.T) {
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		var again FunctionRequest
		require.NoError(t, json.Unmarshal(raw, &again))

		w := f.PutJSON(nsPath+"/shop", again)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, exists(t, f.client, "api", &fissionv1.HTTPTrigger{}), "the trigger made outside is stored under its API name now")
	})
}

// The package's resource version moves when only its owner or status
// changes. The function must not follow it, or an unchanged PUT would make
// Fission load the function again.
func TestUpdate_UnchangedBodyLeavesTheFunctionAlone(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	before := get(t, f.client, "shop", &fissionv1.Function{})
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	require.NotEqual(t, before.Spec.Package.PackageRef.ResourceVersion, pkg.ResourceVersion,
		"the owner reference moved the package version after the function was created")

	w := f.PutJSON(nsPath+"/shop", basicRequest())

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	after := get(t, f.client, "shop", &fissionv1.Function{})
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "the function must not be written")
}

// The server's write timeout is shorter than a clone can take. Without a
// longer deadline for this response the client would get a closed
// connection although the function was created.
func TestCreate_AnswersAfterTheServerWriteTimeout(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	f.fetcher.delay = 700 * time.Millisecond
	server := httptest.NewUnstartedServer(f.Engine)
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	resp, err := http.Post(server.URL+nsPath, "application/json", testutil.JSONBody(t, basicRequest()))

	require.NoError(t, err, "the response must arrive after the write timeout")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestGetAndList(t *testing.T) {
	f := newFixture(t, interceptor.Funcs{}, builderPool())
	f.handler.routerURLs[""] = "https://fn.example/"
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)
	pkg := get(t, f.client, "shop", &fissionv1.Package{})
	pkg.Status.BuildStatus = fissionv1.BuildStatusFailed
	pkg.Status.BuildLog = "pip: no such package"
	require.NoError(t, f.client.Status().Update(context.Background(), pkg))

	t.Run("get", func(t *testing.T) {
		w := f.Get(nsPath + "/shop")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decode[FunctionResponse](t, w)
		assert.Equal(t, FunctionBuild{Status: "failed", Log: "pip: no such package"}, resp.Build)
		assert.Equal(t, "https://fn.example/ns1/shop/orders", resp.Triggers[0].URL)
		assert.NotNil(t, resp.Status.Conditions)
	})

	t.Run("get a missing function", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, f.Get(nsPath+"/absent").Code)
	})

	for _, path := range []string{nsPath, "/cfunc/functions"} {
		t.Run("list "+path, func(t *testing.T) {
			w := f.Get(path)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			list := decode[FunctionListResponse](t, w)
			require.Equal(t, 1, list.Total)
			assert.Len(t, list.Items[0].Triggers, 2)
			assert.Equal(t, "failed", list.Items[0].Build.Status)
		})
	}
}

func TestDelete_RemovesEveryObject(t *testing.T) {
	other := &fissionv1.HTTPTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "other-api", Namespace: "ns1"},
		Spec: fissionv1.HTTPTriggerSpec{
			RelativeURL:       "/ns1/other",
			FunctionReference: fissionv1.FunctionReference{Type: fissionv1.FunctionReferenceTypeFunctionName, Name: "other"},
		},
	}
	f := newFixture(t, interceptor.Funcs{}, builderPool(), other)
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

	w := f.Delete(nsPath + "/shop")

	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.False(t, exists(t, f.client, "shop", &fissionv1.Function{}))
	assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}))
	assert.False(t, exists(t, f.client, "shop-api", &fissionv1.HTTPTrigger{}))
	assert.False(t, exists(t, f.client, "shop-nightly", &fissionv1.TimeTrigger{}))
	assert.True(t, exists(t, f.client, "other-api", &fissionv1.HTTPTrigger{}), "the trigger of another function stays")
	assert.Equal(t, http.StatusNotFound, f.Delete(nsPath+"/shop").Code)
}

// The function is deleted last, so a delete that failed part of the way can
// be sent again and still finds the rest.
func TestDelete_RepeatAfterFailure(t *testing.T) {
	fail := true
	f := newFixture(t, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isPackage := obj.(*fissionv1.Package); isPackage && fail {
				return errors.New("api server went away")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, builderPool())
	require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

	require.Equal(t, http.StatusInternalServerError, f.Delete(nsPath+"/shop").Code)
	require.True(t, exists(t, f.client, "shop", &fissionv1.Function{}))

	fail = false
	require.Equal(t, http.StatusNoContent, f.Delete(nsPath+"/shop").Code)
	assert.False(t, exists(t, f.client, "shop", &fissionv1.Package{}))
}

func TestTestRoute(t *testing.T) {
	var seen *http.Request
	var seenBody string
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		seenBody = string(raw)
		if r.URL.Path == "/ns1/shop/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
			return
		}
		w.Header().Set("X-Fn", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello from shop"))
	}))
	t.Cleanup(router.Close)

	setup := func(t *testing.T, triggers ...FunctionTrigger) *fixture {
		f := newFixture(t, interceptor.Funcs{}, builderPool())
		f.handler.routerURLs[""] = router.URL
		req := basicRequest()
		req.Triggers = triggers
		require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, req).Code)
		return f
	}
	api := FunctionTrigger{Name: "api", Type: TriggerTypeHTTP, Path: "/orders", Methods: []string{"POST", "GET"}}

	t.Run("calls the trigger path on the router", func(t *testing.T) {
		f := setup(t, api)

		w := f.PostJSON(nsPath+"/shop/test", FunctionTestRequest{Body: `{"a":1}`, Headers: map[string]string{"X-Req": "1"}})

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decode[FunctionTestResponse](t, w)
		assert.Equal(t, http.StatusTeapot, resp.StatusCode)
		assert.Equal(t, "hello from shop", resp.Body)
		assert.Equal(t, "yes", resp.Headers["X-Fn"])
		assert.Equal(t, "POST", resp.Method, "the first method of the trigger is the default")
		assert.Equal(t, "/ns1/shop/orders", seen.URL.Path)
		assert.Equal(t, "1", seen.Header.Get("X-Req"))
		assert.JSONEq(t, `{"a":1}`, seenBody)
	})

	t.Run("an empty body is accepted", func(t *testing.T) {
		f := setup(t, api)

		w := f.Post(nsPath+"/shop/test", nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("a redirect is returned, not followed", func(t *testing.T) {
		f := setup(t, FunctionTrigger{Name: "r", Type: TriggerTypeHTTP, Path: "/redirect"})

		w := f.Post(nsPath+"/shop/test", nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, http.StatusFound, decode[FunctionTestResponse](t, w).StatusCode)
	})

	t.Run("a method the trigger does not serve", func(t *testing.T) {
		f := setup(t, api)

		w := f.PostJSON(nsPath+"/shop/test", FunctionTestRequest{Method: "DELETE"})

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, errorMessage(t, w), "does not serve DELETE")
	})

	t.Run("more than one http trigger needs a name", func(t *testing.T) {
		f := setup(t, api, FunctionTrigger{Name: "b", Type: TriggerTypeHTTP, Path: "/b"})

		assert.Equal(t, http.StatusBadRequest, f.Post(nsPath+"/shop/test", nil).Code)
		assert.Equal(t, http.StatusOK, f.PostJSON(nsPath+"/shop/test", FunctionTestRequest{Trigger: "b"}).Code)
	})

	t.Run("a function with only a cron trigger", func(t *testing.T) {
		f := setup(t, FunctionTrigger{Name: "nightly", Type: TriggerTypeCron, Schedule: "@daily"})

		w := f.Post(nsPath+"/shop/test", nil)

		require.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, errorMessage(t, w), "no http trigger")
	})

	t.Run("no router URL is 501", func(t *testing.T) {
		f := setup(t, api)
		delete(f.handler.routerURLs, "")

		assert.Equal(t, http.StatusNotImplemented, f.Post(nsPath+"/shop/test", nil).Code)
	})

	t.Run("a router that does not answer is 502", func(t *testing.T) {
		f := setup(t, api)
		f.handler.routerURLs[""] = "http://127.0.0.1:1"

		assert.Equal(t, http.StatusBadGateway, f.Post(nsPath+"/shop/test", nil).Code)
	})
}

func TestLogsRoute(t *testing.T) {
	pod := func(name, function string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns1",
			Labels: map[string]string{fissionv1.FUNCTION_NAME: function, fissionv1.FUNCTION_NAMESPACE: "ns1"},
		}}
	}
	type call struct {
		pod, container string
		lines          int64
	}
	setup := func(t *testing.T, withReader bool) (*testutil.EngineHelper, *[]call) {
		f := newFixture(t, interceptor.Funcs{}, builderPool(), pod("pod-a", "shop"), pod("pod-gone", "shop"), pod("pod-x", "other"))
		require.Equal(t, http.StatusCreated, f.PostJSON(nsPath, basicRequest()).Code)

		calls := &[]call{}
		var reader cluster.PodLogs = func(_ context.Context, _, pod, container string, tailLines, _ int64) (string, error) {
			*calls = append(*calls, call{pod: pod, container: container, lines: tailLines})
			if pod == "pod-gone" {
				return "", k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod)
			}
			return "line one\nline two\n", nil
		}
		_, engine := gin.CreateTestContext(httptest.NewRecorder())
		engine.Use(func(c *gin.Context) {
			c.Set(string(middleware.K8sClientKey), f.client)
			if withReader {
				c.Set(string(middleware.PodLogsKey), reader)
			}
			c.Next()
		})
		f.handler.RegisterRoutes(engine.Group(""))
		return testutil.NewEngineHelperFrom(t, engine), calls
	}

	t.Run("returns the log of the function's pods only, and no lines for a pod that is gone", func(t *testing.T) {
		engine, calls := setup(t, true)

		w := engine.Get(nsPath + "/shop/logs?lines=50")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decode[FunctionLogsResponse](t, w)
		assert.ElementsMatch(t, []FunctionPodLog{
			{Pod: "pod-a", Lines: []string{"line one", "line two"}},
			{Pod: "pod-gone", Lines: []string{}},
		}, resp.Pods, "a pod whose log cannot be read stays in the answer with no lines")
		assert.ElementsMatch(t, []call{{pod: "pod-a", container: "py", lines: 50}, {pod: "pod-gone", container: "py", lines: 50}}, *calls)
	})

	t.Run("reads only the Function, not its package and triggers", func(t *testing.T) {
		f := newFixture(t, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isPods := list.(*corev1.PodList); !isPods {
					return fmt.Errorf("the logs route must not list %T", list)
				}
				return c.List(ctx, list, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isPackage := obj.(*fissionv1.Package); isPackage {
					return errors.New("the logs route must not read the package")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}, builderPool(), &fissionv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"}})
		var reader cluster.PodLogs = func(context.Context, string, string, string, int64, int64) (string, error) { return "", nil }
		_, engine := gin.CreateTestContext(httptest.NewRecorder())
		engine.Use(func(c *gin.Context) {
			c.Set(string(middleware.K8sClientKey), f.client)
			c.Set(string(middleware.PodLogsKey), reader)
			c.Next()
		})
		f.handler.RegisterRoutes(engine.Group(""))

		w := testutil.NewEngineHelperFrom(t, engine).Get(nsPath + "/shop/logs")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("lines out of range", func(t *testing.T) {
		engine, _ := setup(t, true)

		assert.Equal(t, http.StatusBadRequest, engine.Get(nsPath+"/shop/logs?lines=0").Code)
		assert.Equal(t, http.StatusBadRequest, engine.Get(nsPath+"/shop/logs?lines=5000").Code)
	})

	t.Run("a missing function", func(t *testing.T) {
		engine, _ := setup(t, true)

		assert.Equal(t, http.StatusNotFound, engine.Get(nsPath+"/absent/logs").Code)
	})

	t.Run("no log reader is 501", func(t *testing.T) {
		engine, _ := setup(t, false)

		assert.Equal(t, http.StatusNotImplemented, engine.Get(nsPath+"/shop/logs").Code)
	})
}
