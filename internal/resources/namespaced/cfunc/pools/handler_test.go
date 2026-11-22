package pools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/testutil"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	nsPath  = "/namespaces/ns1/cfunc/pools"
	prefix  = "registry.example/fission"
	pyImage = prefix + "/python-env:1.35.0"
)

func testConfig() config.CfuncConfig {
	return config.CfuncConfig{
		Enabled:          true,
		RegistryPrefix:   prefix,
		ImagePullSecret:  "regcred",
		AllowCustomBuild: true,
		MaxPoolSize:      10,
		DefaultSize:      "small",
		Sizes: map[string]config.ResourceSize{
			"small": {
				Requests: config.ResourceQuantities{CPU: "100m", Memory: "128Mi"},
				Limits:   config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
			},
			"large": {
				Requests: config.ResourceQuantities{CPU: "500m", Memory: "512Mi"},
				Limits:   config.ResourceQuantities{CPU: "1", Memory: "1Gi"},
			},
		},
		Flavors: map[string]config.CfuncFlavor{
			"python": {
				Image:      "python-env:1.35.0",
				BuildImage: "python-builder:1.35.0",
				BuilderEnv: []config.CfuncEnvVar{{Name: "PIP_INDEX_URL", Value: "https://registry.example/pypi"}},
			},
			"java": {Image: "jvm-env:1.32.0", BuildImage: "jvm-builder:1.32.0", BuildCommand: "build", KeepArchive: true},
			"perl": {Image: "perl-env:1.32.0"},
		},
	}
}

func mustHandler(t *testing.T, cfg config.CfuncConfig) *Handler {
	t.Helper()
	h, err := New(cfg)
	require.NoError(t, err)
	return h
}

func catalogOf(t *testing.T, cfg config.CfuncConfig) *Catalog {
	t.Helper()
	catalog, err := NewCatalog(cfg)
	require.NoError(t, err)
	return catalog
}

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	return catalogOf(t, testConfig())
}

func engine(t *testing.T, objects ...client.Object) (*testutil.EngineHelper, client.Client) {
	t.Helper()
	k8sClient := testutil.FakeClient(t, objects...)
	return testutil.NewEngineHelper(t, k8sClient, mustHandler(t, testConfig()), "ns1", "ns2"), k8sClient
}

func stored(t *testing.T, k8sClient client.Client, name string) *fissionv1.Environment {
	t.Helper()
	env := &fissionv1.Environment{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: name}, env))
	return env
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

func resourcesOf(reqCPU, reqMem, limCPU, limMem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(reqCPU), corev1.ResourceMemory: resource.MustParse(reqMem)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limCPU), corev1.ResourceMemory: resource.MustParse(limMem)},
	}
}

func assertResources(t *testing.T, want, got corev1.ResourceRequirements) {
	t.Helper()
	for name, quantity := range want.Requests {
		assert.Zero(t, quantity.Cmp(got.Requests[name]), "requests.%s: want %s, got %v", name, quantity.String(), got.Requests[name])
	}
	for name, quantity := range want.Limits {
		assert.Zero(t, quantity.Cmp(got.Limits[name]), "limits.%s: want %s, got %v", name, quantity.String(), got.Limits[name])
	}
	assert.Len(t, got.Requests, len(want.Requests))
	assert.Len(t, got.Limits, len(want.Limits))
}

func pythonEnv(name, namespace string) *fissionv1.Environment {
	return &fissionv1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: fissionv1.EnvironmentSpec{
			Version:                3,
			Runtime:                fissionv1.Runtime{Image: pyImage},
			Builder:                fissionv1.Builder{Image: prefix + "/python-builder:1.35.0"},
			Poolsize:               3,
			Resources:              resourcesOf("100m", "128Mi", "200m", "256Mi"),
			TerminationGracePeriod: 360,
		},
	}
}

func function(name, namespace, envName, envNamespace string) *fissionv1.Function {
	return &fissionv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: fissionv1.FunctionSpec{
			Environment: fissionv1.EnvironmentReference{Name: envName, Namespace: envNamespace},
		},
	}
}

// -- Create --

func TestCreate_WritesTheValuesBehindFlavorAndDefaultSize(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py")
	assert.Equal(t, 3, env.Spec.Version)
	assert.Equal(t, pyImage, env.Spec.Runtime.Image)
	assert.Equal(t, prefix+"/python-builder:1.35.0", env.Spec.Builder.Image)
	assert.Equal(t, "build", env.Spec.Builder.Command, "an empty command makes the Fission builder run /build, which fails")
	require.NotNil(t, env.Spec.Builder.Container)
	assert.Equal(t, fissionv1.BuilderContainerName, env.Spec.Builder.Container.Name)
	assert.Equal(t, []corev1.EnvVar{{Name: "PIP_INDEX_URL", Value: "https://registry.example/pypi"}}, env.Spec.Builder.Container.Env)
	assert.False(t, env.Spec.KeepArchive)
	assert.Equal(t, "regcred", env.Spec.ImagePullSecret)
	assert.Equal(t, 3, env.Spec.Poolsize)
	// Fission v1.27.0 treats an omitted grace period as 0 seconds.
	assert.Equal(t, int64(360), env.Spec.TerminationGracePeriod)
	assertResources(t, resourcesOf("100m", "128Mi", "200m", "256Mi"), env.Spec.Resources)
	assert.Empty(t, env.Labels)
	assert.Empty(t, env.Annotations)

	resp := decode[PoolResponse](t, w)
	assert.Equal(t, "py", resp.Name)
	assert.Equal(t, "ns1", resp.Namespace)
	assert.Equal(t, "python", resp.Flavor)
	assert.Equal(t, "small", resp.Size)
	assert.Equal(t, 3, resp.PoolSize)
	assert.Equal(t, pyImage, resp.Image)
	require.NotNil(t, resp.Builder)
	assert.Equal(t, prefix+"/python-builder:1.35.0", resp.Builder.Image)
	assert.Equal(t, int64(360), resp.TerminationGracePeriodSeconds)
	require.NotNil(t, resp.Resources)
	assert.Equal(t, "100m", resp.Resources.Requests.CPU)
	assert.Equal(t, "256Mi", resp.Resources.Limits.Memory)
}

func TestCreate_NamedSizeAndFlavorSettings(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "jvm", "flavor": "java", "size": "large", "poolSize": 1})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	env := stored(t, k8sClient, "jvm")
	assert.True(t, env.Spec.KeepArchive)
	assert.Equal(t, "build", env.Spec.Builder.Command)
	assert.Nil(t, env.Spec.Builder.Container, "a flavor with no builder variables must not create a builder container")
	assert.Equal(t, 1, env.Spec.Poolsize)
	assertResources(t, resourcesOf("500m", "512Mi", "1", "1Gi"), env.Spec.Resources)
	assert.Equal(t, "large", decode[PoolResponse](t, w).Size)
}

func TestCreate_PoolSizeZeroIsKept(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "poolSize": 0})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Zero(t, stored(t, k8sClient, "py").Spec.Poolsize)
}

func TestCreate_AdvancedOverridesFlavorAndSize(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{
		"name": "py", "flavor": "python", "size": "small",
		"advanced": map[string]any{
			"buildImage":   "other.example/team/builder:2",
			"buildCommand": "./build.sh",
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "150m"},
				"limits":   map[string]any{"cpu": "600m", "memory": "400Mi"},
			},
		},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py")
	assert.Equal(t, pyImage, env.Spec.Runtime.Image, "advanced must not change the runtime image")
	assert.Equal(t, "other.example/team/builder:2", env.Spec.Builder.Image, "an advanced build image is used as written, without the prefix")
	assert.Equal(t, "./build.sh", env.Spec.Builder.Command)
	assertResources(t, resourcesOf("150m", "128Mi", "600m", "400Mi"), env.Spec.Resources)

	resp := decode[PoolResponse](t, w)
	assert.Equal(t, "python", resp.Flavor)
	assert.Empty(t, resp.Size, "custom resources match no size")
	require.NotNil(t, resp.Advanced, "the response must carry what a PUT needs to keep the custom values")
	assert.Equal(t, "other.example/team/builder:2", resp.Advanced.BuildImage)
	assert.Equal(t, "./build.sh", resp.Advanced.BuildCommand)
}

func TestUpdate_ResponseSentBackKeepsCustomValues(t *testing.T) {
	h, k8sClient := engine(t)
	created := h.PostJSON(nsPath, map[string]any{
		"name": "py", "flavor": "python", "poolSize": 2,
		"advanced": map[string]any{
			"buildImage": "other.example/team/builder:2",
			"resources":  map[string]any{"limits": map[string]any{"memory": "400Mi"}},
		},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	before := stored(t, k8sClient, "py").Spec

	got := decode[PoolResponse](t, h.Get(nsPath+"/py"))
	w := h.PutJSON(nsPath+"/py", map[string]any{
		"name": got.Name, "flavor": got.Flavor, "size": got.Size, "poolSize": 5, "env": got.Env, "advanced": got.Advanced,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after := stored(t, k8sClient, "py").Spec
	assert.Equal(t, 5, after.Poolsize)
	assert.Equal(t, before.Builder.Image, after.Builder.Image, "the custom build image must survive a read-modify-write")
	assert.Equal(t, before.Builder.Command, after.Builder.Command)
	assertResources(t, before.Resources, after.Resources)
}

func TestCreate_PoolSizeAboveTheMaximum(t *testing.T) {
	h, _ := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "poolSize": 11})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, errorMessage(t, w), "poolSize 11 is above the maximum of 10")

	assert.Equal(t, http.StatusCreated, h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "poolSize": 10}).Code)
}

func TestCreate_AdvancedBuildImageOnAFlavorWithoutOne(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{
		"name": "pl", "flavor": "perl",
		"advanced": map[string]any{"buildImage": "other.example/perl-builder:1", "buildCommand": "make"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	env := stored(t, k8sClient, "pl")
	assert.Equal(t, "other.example/perl-builder:1", env.Spec.Builder.Image)
	assert.Equal(t, "make", env.Spec.Builder.Command)
}

func TestCreate_FlavorWithoutBuilder(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "pl", "flavor": "perl"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	assert.Empty(t, stored(t, k8sClient, "pl").Spec.Builder.Image)
	assert.Nil(t, decode[PoolResponse](t, w).Builder)
}

func TestCreate_BadRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        map[string]any
		msgContains string
	}{
		{"missing name", map[string]any{"flavor": "python"}, "invalid request body"},
		{"missing flavor", map[string]any{"name": "x"}, "invalid request body"},
		{"unknown flavor", map[string]any{"name": "x", "flavor": "cobol"}, `unknown flavor "cobol"; available flavors: java, perl, python`},
		{"flavor names are lowercase", map[string]any{"name": "x", "flavor": "Python"}, `unknown flavor "Python"`},
		{"unknown size", map[string]any{"name": "x", "flavor": "python", "size": "huge"}, `unknown size "huge"; available sizes: large, small`},
		{"negative pool size", map[string]any{"name": "x", "flavor": "python", "poolSize": -1}, "poolSize must be 0 or greater"},
		{
			"invalid quantity",
			map[string]any{"name": "x", "flavor": "python", "advanced": map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "lots"}}}},
			`advanced.resources: invalid limits.memory "lots"`,
		},
		{
			"negative quantity",
			map[string]any{"name": "x", "flavor": "python", "advanced": map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "-1"}}}},
			"advanced.resources.requests.cpu must not be negative",
		},
		{
			"request larger than limit",
			map[string]any{"name": "x", "flavor": "python", "advanced": map[string]any{"resources": map[string]any{
				"requests": map[string]any{"memory": "1Gi"}, "limits": map[string]any{"memory": "512Mi"},
			}}},
			"advanced.resources.requests.memory 1Gi is larger than its limit 512Mi",
		},
		{
			"build image that is not an image reference",
			map[string]any{"name": "x", "flavor": "python", "advanced": map[string]any{"buildImage": "Not A Reference"}},
			`advanced.buildImage "Not A Reference" is not a valid image reference`,
		},
		{
			"resources above the largest size",
			map[string]any{"name": "x", "flavor": "python", "advanced": map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "2"}}}},
			"advanced.resources.limits.cpu 2 is above the largest configured size (1)",
		},
		{
			"secretKeyRef with no name and key",
			map[string]any{"name": "x", "flavor": "python", "env": []map[string]any{{"name": "X", "valueFrom": map[string]any{"secretKeyRef": map[string]any{}}}}},
			`env var "X": secretKeyRef needs both name and key`,
		},
		{
			"build command with no build image",
			map[string]any{"name": "x", "flavor": "perl", "advanced": map[string]any{"buildCommand": "make"}},
			`flavor "perl" has none; set advanced.buildImage`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, k8sClient := engine(t)

			w := h.PostJSON(nsPath, tt.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, errorMessage(t, w), tt.msgContains)

			list := &fissionv1.EnvironmentList{}
			require.NoError(t, k8sClient.List(context.Background(), list))
			assert.Empty(t, list.Items, "a rejected request must create nothing")
		})
	}
}

func TestCreate_WithEnvVars(t *testing.T) {
	h, k8sClient := engine(t)

	w := h.PostJSON(nsPath, map[string]any{
		"name": "py", "flavor": "python",
		"env": []map[string]any{
			{"name": "LOG_LEVEL", "value": "debug"},
			{"name": "DB_PASSWORD", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "password"}}},
		},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	container := stored(t, k8sClient, "py").Spec.Runtime.Container
	require.NotNil(t, container)
	assert.Equal(t, "py", container.Name)
	require.Len(t, container.Env, 2)
	assert.Equal(t, corev1.EnvVar{Name: "LOG_LEVEL", Value: "debug"}, container.Env[0])
	require.NotNil(t, container.Env[1].ValueFrom)
	assert.Equal(t, "db", container.Env[1].ValueFrom.SecretKeyRef.Name)

	resp := decode[PoolResponse](t, w)
	require.Len(t, resp.Env, 2)
	assert.Equal(t, EnvVar{Name: "LOG_LEVEL", Value: "debug"}, resp.Env[0])
}

func TestCreate_EnvVarWithoutName(t *testing.T) {
	h, _ := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "env": []map[string]any{{"value": "x"}}})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, errorMessage(t, w), "env[0]: name is required")
}

func TestCreate_EnvVarValueFromWithoutRef(t *testing.T) {
	h, _ := engine(t)

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "env": []map[string]any{{"name": "X", "valueFrom": map[string]any{}}}})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, errorMessage(t, w), "valueFrom must set exactly one")
}

func TestUpdate_OmittedEnvIsCleared(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Runtime.Container = &corev1.Container{Name: "py", Env: []corev1.EnvVar{{Name: "OLD", Value: "1"}}}
	h, k8sClient := engine(t, existing)

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "python"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Empty(t, stored(t, k8sClient, "py").Spec.Runtime.Container.Env)
	assert.Empty(t, decode[PoolResponse](t, w).Env)
}

func TestCreate_BadJSON(t *testing.T) {
	h, _ := engine(t)

	w := h.Post(nsPath, bytes.NewBufferString("{invalid"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreate_AlreadyExists(t *testing.T) {
	h, _ := engine(t, pythonEnv("py", "ns1"))

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python"})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

// -- Get / List --

func TestGet(t *testing.T) {
	h, _ := engine(t, pythonEnv("py", "ns1"))

	w := h.Get(nsPath + "/py")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decode[PoolResponse](t, w)
	assert.Equal(t, "python", resp.Flavor)
	assert.Equal(t, "small", resp.Size)
	assert.Equal(t, 3, resp.Version)
}

func TestGet_NotFound(t *testing.T) {
	h, _ := engine(t, pythonEnv("py", "ns2"))
	assert.Equal(t, http.StatusNotFound, h.Get(nsPath+"/py").Code)
}

func TestGet_ReportsNoFlavorOrSizeWhenNothingMatches(t *testing.T) {
	foreign := pythonEnv("old", "ns1")
	foreign.Spec.Version = 2
	foreign.Spec.Runtime.Image = prefix + "/python-env:1.34.0"
	foreign.Spec.Resources = resourcesOf("100m", "128Mi", "200m", "300Mi")
	foreign.Spec.AllowedFunctionsPerContainer = fissionv1.AllowedFunctionsPerContainerInfinite
	foreign.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PoolReady"}}
	h, _ := engine(t, foreign)

	w := h.Get(nsPath + "/old")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decode[PoolResponse](t, w)
	assert.Empty(t, resp.Flavor, "an image tag the config no longer has matches no flavor")
	assert.Empty(t, resp.Size)
	assert.Equal(t, prefix+"/python-env:1.34.0", resp.Image)
	assert.Equal(t, 2, resp.Version)
	assert.Equal(t, "infinite", resp.AllowedFunctionsPerContainer)
	require.Len(t, resp.Status.Conditions, 1)
	assert.Equal(t, "Ready", resp.Status.Conditions[0].Type)
	assert.Equal(t, "True", resp.Status.Conditions[0].Status)
}

func TestGet_SizeMatchesAcrossQuantityNotation(t *testing.T) {
	env := pythonEnv("py", "ns1")
	env.Spec.Resources = resourcesOf("0.5", "512Mi", "1000m", "1024Mi")
	h, _ := engine(t, env)

	assert.Equal(t, "large", decode[PoolResponse](t, h.Get(nsPath+"/py")).Size)
}

func TestList_OnlyTheNamespace(t *testing.T) {
	h, _ := engine(t, pythonEnv("a", "ns1"), pythonEnv("b", "ns1"), pythonEnv("c", "ns2"))

	w := h.Get(nsPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decode[PoolListResponse](t, w)
	assert.Equal(t, 2, resp.Total)
	require.Len(t, resp.Items, 2)
	assert.ElementsMatch(t, []string{"a", "b"}, []string{resp.Items[0].Name, resp.Items[1].Name})
}

func TestList_EmptyIsAnEmptyArray(t *testing.T) {
	h, _ := engine(t)

	w := h.Get(nsPath)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"items":[],"total":0}`, w.Body.String())
}

func TestListAll_AcrossNamespaces(t *testing.T) {
	h, _ := engine(t, pythonEnv("a", "ns1"), pythonEnv("c", "ns2"))

	w := h.Get("/cfunc/pools")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, decode[PoolListResponse](t, w).Total)
}

func TestList_ClusterWithoutFissionIsNotSupported(t *testing.T) {
	noMatch := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "fission.io", Kind: "Environment"}}
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return noMatch },
	})
	h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, testConfig()), "ns1", "ns2")

	for _, path := range []string{nsPath, "/cfunc/pools"} {
		assert.Equal(t, http.StatusNotImplemented, h.Get(path).Code, path)
	}
}

// -- Update --

func TestUpdate_KeepsWhatTheAPIDoesNotExpose(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.TerminationGracePeriod = 120
	existing.Spec.AllowAccessToExternalNetwork = true
	existing.Spec.Runtime.PodSpec = &corev1.PodSpec{NodeSelector: map[string]string{"pool": "faas"}}
	existing.Spec.Builder.Container = &corev1.Container{
		Name:      fissionv1.BuilderContainerName,
		Env:       []corev1.EnvVar{{Name: "PIP_INDEX_URL", Value: "stale"}},
		Resources: resourcesOf("1", "1Gi", "1", "1Gi"),
	}
	h, k8sClient := engine(t, existing)

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "java", "size": "large", "poolSize": 5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py")
	assert.Equal(t, prefix+"/jvm-env:1.32.0", env.Spec.Runtime.Image)
	assert.Equal(t, prefix+"/jvm-builder:1.32.0", env.Spec.Builder.Image)
	assert.Equal(t, "build", env.Spec.Builder.Command)
	assert.True(t, env.Spec.KeepArchive)
	assert.Equal(t, 5, env.Spec.Poolsize)
	assertResources(t, resourcesOf("500m", "512Mi", "1", "1Gi"), env.Spec.Resources)

	assert.Equal(t, int64(120), env.Spec.TerminationGracePeriod)
	assert.True(t, env.Spec.AllowAccessToExternalNetwork)
	require.NotNil(t, env.Spec.Runtime.PodSpec)
	assert.Equal(t, map[string]string{"pool": "faas"}, env.Spec.Runtime.PodSpec.NodeSelector)
	require.NotNil(t, env.Spec.Builder.Container)
	assert.Empty(t, env.Spec.Builder.Container.Env, "the java flavor has no builder variables, so python's must go")
	assertResources(t, resourcesOf("1", "1Gi", "1", "1Gi"), env.Spec.Builder.Container.Resources)

	resp := decode[PoolResponse](t, w)
	assert.Equal(t, "java", resp.Flavor)
	assert.Equal(t, "large", resp.Size)
}

func TestUpdate_OmittedFieldsFallBackToDefaults(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Poolsize = 9
	existing.Spec.Builder.Image = "other.example/team/builder:2"
	existing.Spec.Builder.Command = "./build.sh"
	existing.Spec.Resources = resourcesOf("300m", "200Mi", "600m", "400Mi")
	h, k8sClient := engine(t, existing)

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "python"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py")
	assert.Equal(t, 3, env.Spec.Poolsize)
	assert.Equal(t, prefix+"/python-builder:1.35.0", env.Spec.Builder.Image, "without advanced, the flavor's build image comes back")
	assert.Equal(t, "build", env.Spec.Builder.Command)
	assertResources(t, resourcesOf("100m", "128Mi", "200m", "256Mi"), env.Spec.Resources)
}

func TestUpdate_RejectedRequestChangesNothing(t *testing.T) {
	h, k8sClient := engine(t, pythonEnv("py", "ns1"))
	before := stored(t, k8sClient, "py").ResourceVersion

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "java", "size": "huge"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py")
	assert.Equal(t, before, env.ResourceVersion)
	assert.Equal(t, pyImage, env.Spec.Runtime.Image)
}

func TestUpdate_URLNameIsAuthoritative(t *testing.T) {
	h, k8sClient := engine(t, pythonEnv("py", "ns1"))

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "other", "flavor": "java"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, "py", decode[PoolResponse](t, w).Name)
	assert.Equal(t, prefix+"/jvm-env:1.32.0", stored(t, k8sClient, "py").Spec.Runtime.Image)
	list := &fissionv1.EnvironmentList{}
	require.NoError(t, k8sClient.List(context.Background(), list))
	assert.Len(t, list.Items, 1, "the body name must not create or rename anything")
}

func TestUpdate_RuntimeContainerIsNamedAfterTheURL(t *testing.T) {
	h, k8sClient := engine(t, pythonEnv("py", "ns1"))

	w := h.PutJSON(nsPath+"/py", map[string]any{
		"name": "other", "flavor": "python",
		"env": []map[string]any{{"name": "LOG_LEVEL", "value": "debug"}},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	container := stored(t, k8sClient, "py").Spec.Runtime.Container
	require.NotNil(t, container)
	assert.Equal(t, "py", container.Name, "a container named after the body would make Fission rename the pod's main container")
}

func TestUpdate_OldVersionEnvironmentIsRejected(t *testing.T) {
	for _, version := range []int{1, 2} {
		existing := pythonEnv("py", "ns1")
		existing.Spec.Version = version
		h, k8sClient := engine(t, existing)
		before := stored(t, k8sClient, "py").ResourceVersion

		w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "java", "poolSize": 5})
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, errorMessage(t, w), fmt.Sprintf("version %d Environment", version))

		env := stored(t, k8sClient, "py")
		assert.Equal(t, before, env.ResourceVersion, "a rejected update must change nothing")
		assert.Equal(t, pyImage, env.Spec.Runtime.Image)
	}
}

func TestUpdate_OldVersionEnvironmentCanStillBeReadAndDeleted(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Version = 2
	h, _ := engine(t, existing)

	w := h.Get(nsPath + "/py")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, decode[PoolResponse](t, w).Version)
	assert.Equal(t, http.StatusNoContent, h.Delete(nsPath+"/py").Code)
}

func TestUpdate_ZeroGracePeriodIsReplaced(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.TerminationGracePeriod = 0
	h, k8sClient := engine(t, existing)

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "python"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, int64(360), stored(t, k8sClient, "py").Spec.TerminationGracePeriod)
	assert.Equal(t, int64(360), decode[PoolResponse](t, w).TerminationGracePeriodSeconds)
}

func TestUpdate_ResponseCanBeSentBack(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Runtime.Container = &corev1.Container{Name: "py", Env: []corev1.EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
	}}
	h, k8sClient := engine(t, existing)

	got := decode[PoolResponse](t, h.Get(nsPath+"/py"))
	assert.Equal(t, []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}, got.Env, "a fieldRef cannot be expressed, so it is left out rather than sent as an empty valueFrom")

	w := h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": got.Flavor, "size": got.Size, "poolSize": got.PoolSize, "env": got.Env})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := stored(t, k8sClient, "py").Spec.Runtime.Container.Env
	require.Len(t, env, 2)
	assert.Equal(t, "LOG_LEVEL", env[0].Name)
	assert.Equal(t, "POD_IP", env[1].Name)
	require.NotNil(t, env[1].ValueFrom.FieldRef, "the fieldRef variable must survive the round trip")
}

func TestCreate_CustomBuildDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.AllowCustomBuild = false
	for name, advanced := range map[string]map[string]any{
		"build image":   {"buildImage": "other.example/builder:1"},
		"build command": {"buildCommand": "./build.sh"},
	} {
		t.Run(name, func(t *testing.T) {
			k8sClient := testutil.FakeClient(t)
			h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, cfg))

			w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "advanced": advanced})
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, errorMessage(t, w), "not enabled on this backend")

			list := &fissionv1.EnvironmentList{}
			require.NoError(t, k8sClient.List(context.Background(), list))
			assert.Empty(t, list.Items)
		})
	}
}

func TestCreate_CustomBuildDisabled_AdvancedResourcesStillWork(t *testing.T) {
	cfg := testConfig()
	cfg.AllowCustomBuild = false
	h := testutil.NewEngineHelper(t, testutil.FakeClient(t), mustHandler(t, cfg))

	w := h.PostJSON(nsPath, map[string]any{"name": "py", "flavor": "python", "advanced": map[string]any{
		"resources": map[string]any{"limits": map[string]any{"cpu": "300m"}},
	}})
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestUpdate_BadJSON(t *testing.T) {
	h, _ := engine(t, pythonEnv("py", "ns1"))

	w := h.Put(nsPath+"/py", bytes.NewBufferString("{invalid"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdate_NotFound(t *testing.T) {
	h, _ := engine(t)
	assert.Equal(t, http.StatusNotFound, h.PutJSON(nsPath+"/py", map[string]any{"name": "py", "flavor": "python"}).Code)
}

// -- Delete --

func TestDelete(t *testing.T) {
	h, k8sClient := engine(t, pythonEnv("py", "ns1"))

	require.Equal(t, http.StatusNoContent, h.Delete(nsPath+"/py").Code)
	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "ns1", Name: "py"}, &fissionv1.Environment{})
	assert.True(t, k8serrors.IsNotFound(err), "the environment must be gone, got %v", err)
}

func TestDelete_NotFound(t *testing.T) {
	h, _ := engine(t)
	assert.Equal(t, http.StatusNotFound, h.Delete(nsPath+"/py").Code)
}

func TestDelete_RefusedWhileFunctionsUseIt(t *testing.T) {
	h, k8sClient := engine(t,
		pythonEnv("py", "ns1"),
		function("explicit", "ns1", "py", "ns1"),
		function("implicit", "ns1", "py", ""),
		function("other-env", "ns1", "js", "ns1"),
	)

	w := h.Delete(nsPath + "/py")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	msg := errorMessage(t, w)
	assert.Contains(t, msg, "explicit")
	assert.Contains(t, msg, "implicit")
	assert.NotContains(t, msg, "other-env")
	stored(t, k8sClient, "py")
}

func TestDelete_IgnoresFunctionsOfASameNamedEnvironmentElsewhere(t *testing.T) {
	h, _ := engine(t,
		pythonEnv("py", "ns1"),
		function("points-away", "ns1", "py", "ns2"),
		function("other-namespace", "ns2", "py", "ns2"),
	)

	assert.Equal(t, http.StatusNoContent, h.Delete(nsPath+"/py").Code)
}

func TestOnboardNamespace(t *testing.T) {
	namespace := func(labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", Labels: labels}}
	}
	labelsOf := func(t *testing.T, k8sClient client.Client) map[string]string {
		t.Helper()
		var ns corev1.Namespace
		require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: "ns1"}, &ns))
		return ns.Labels
	}
	request := PoolRequest{Name: "py", Flavor: "python"}

	t.Run("off by default", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t, namespace(nil))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, testConfig()), "ns1", "ns2")

		require.Equal(t, http.StatusCreated, h.PostJSON(nsPath, request).Code)
		assert.NotContains(t, labelsOf(t, k8sClient), FissionEnabledLabel)
	})

	t.Run("sets the label and keeps the others", func(t *testing.T) {
		cfg := testConfig()
		cfg.OnboardNamespaces = true
		k8sClient := testutil.FakeClient(t, namespace(map[string]string{"team": "a"}))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, cfg))

		require.Equal(t, http.StatusCreated, h.PostJSON(nsPath, request).Code)
		assert.Equal(t, map[string]string{"team": "a", FissionEnabledLabel: "true"}, labelsOf(t, k8sClient))
	})

	forbidden := interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, obj.GetName(), fmt.Errorf("no"))
		},
	}

	t.Run("says what to do when nobody may label the namespace", func(t *testing.T) {
		cfg := testConfig()
		cfg.OnboardNamespaces = true
		userClient := testutil.FakeClientWithInterceptors(t, forbidden, namespace(nil))
		h := testutil.NewEngineHelper(t, userClient, mustHandler(t, cfg))

		w := h.PostJSON(nsPath, request)

		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, errorMessage(t, w), "fission.io/enabled=true")
		var envs fissionv1.EnvironmentList
		require.NoError(t, userClient.List(context.Background(), &envs))
		assert.Empty(t, envs.Items)
	})

	t.Run("a backend failure that is not a refusal is not reported as one", func(t *testing.T) {
		cfg := testConfig()
		cfg.OnboardNamespaces = true
		userClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return k8serrors.NewTimeoutError("the api server is slow", 1)
			},
		}, namespace(nil))
		h := testutil.NewEngineHelper(t, userClient, mustHandler(t, cfg))

		w := h.PostJSON(nsPath, request)

		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "ask a cluster administrator")
		assert.GreaterOrEqual(t, w.Code, http.StatusInternalServerError)
	})

	t.Run("a failed label leaves no pool", func(t *testing.T) {
		cfg := testConfig()
		cfg.OnboardNamespaces = true
		k8sClient := testutil.FakeClient(t)
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, cfg))

		require.Equal(t, http.StatusNotFound, h.PostJSON(nsPath, request).Code)
		var envs fissionv1.EnvironmentList
		require.NoError(t, k8sClient.List(context.Background(), &envs))
		assert.Empty(t, envs.Items)
	})
}
