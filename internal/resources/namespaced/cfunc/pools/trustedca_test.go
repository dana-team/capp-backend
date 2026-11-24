package pools

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/testutil"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const caBundle = TrustedCAMountPath + "/ca-bundle.crt"

// trustedCAConfig is the test config with a trusted CA bundle and python
// variables that point at it on the runtime and the builder.
func trustedCAConfig() config.CfuncConfig {
	cfg := testConfig()
	cfg.TrustedCA = config.CfuncTrustedCA{ConfigMap: "trusted-ca"}
	python := cfg.Flavors["python"]
	python.RuntimeEnv = []config.CfuncEnvVar{
		{Name: "SSL_CERT_FILE", Value: caBundle},
		{Name: "REQUESTS_CA_BUNDLE", Value: caBundle},
	}
	python.BuilderEnv = append(python.BuilderEnv, config.CfuncEnvVar{Name: "PIP_CERT", Value: caBundle})
	cfg.Flavors["python"] = python
	return cfg
}

func trustedCAVolume() corev1.Volume {
	return corev1.Volume{
		Name: "cfunc-trusted-ca",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "trusted-ca"},
		}},
	}
}

func trustedCAMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "cfunc-trusted-ca", MountPath: "/etc/cfunc/trusted-ca", ReadOnly: true}
}

func trustedCAConfigMap(namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "trusted-ca", Namespace: namespace}, Data: data}
}

// -- Conversion --

func TestToK8s_TrustedCAIsMountedInTheRuntimeAndTheBuilder(t *testing.T) {
	env, err := ToK8s(minimalRequest(), nil, "default", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)

	runtimePodSpec := env.Spec.Runtime.PodSpec
	require.NotNil(t, runtimePodSpec)
	assert.Equal(t, []corev1.Volume{trustedCAVolume()}, runtimePodSpec.Volumes)
	runtime := containerNamed(runtimePodSpec, "my-pool")
	require.NotNil(t, runtime)
	assert.Equal(t, []corev1.VolumeMount{trustedCAMount()}, runtime.VolumeMounts)
	assert.Empty(t, containerNamed(runtimePodSpec, "fetcher").VolumeMounts, "the fetcher talks only to the cluster")

	require.NotNil(t, env.Spec.Builder.PodSpec)
	assert.Equal(t, []corev1.Volume{trustedCAVolume()}, env.Spec.Builder.PodSpec.Volumes)
	assert.Empty(t, containerNamed(env.Spec.Builder.PodSpec, "fetcher").VolumeMounts)
	require.NotNil(t, env.Spec.Builder.Container)
	assert.Equal(t, []corev1.VolumeMount{trustedCAMount()}, env.Spec.Builder.Container.VolumeMounts)
	assert.Equal(t, []corev1.EnvVar{
		{Name: "PIP_INDEX_URL", Value: "https://registry.example/pypi"},
		{Name: "PIP_CERT", Value: caBundle},
	}, env.Spec.Builder.Container.Env)
}

func TestToK8s_TrustedCAOnAFlavorWithoutBuilder(t *testing.T) {
	env, err := ToK8s(PoolRequest{Name: "my-pool", Flavor: "perl"}, nil, "default", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)

	assert.Equal(t, []corev1.VolumeMount{trustedCAMount()}, containerNamed(env.Spec.Runtime.PodSpec, "my-pool").VolumeMounts)
	assert.Nil(t, env.Spec.Builder.PodSpec)
	assert.Nil(t, env.Spec.Builder.Container)
}

func TestToK8s_RuntimeEnvGoesIntoThePodSpecEntry(t *testing.T) {
	env, err := ToK8s(minimalRequest(), nil, "default", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)

	assert.Equal(t, []corev1.EnvVar{
		{Name: "SSL_CERT_FILE", Value: caBundle},
		{Name: "REQUESTS_CA_BUNDLE", Value: caBundle},
	}, containerNamed(env.Spec.Runtime.PodSpec, "my-pool").Env)
	assert.Nil(t, env.Spec.Runtime.Container, "the pool's own env is not where the configured variables go")
	assert.Empty(t, FromK8s(env, catalogOf(t, trustedCAConfig())).Env, "configured variables are not the pool's env")
}

func TestToK8s_PoolEnvWinsOverRuntimeEnv(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Runtime.Container = &corev1.Container{Name: "py", Env: []corev1.EnvVar{
		{Name: "REQUESTS_CA_BUNDLE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
	}}
	req := PoolRequest{Name: "py", Flavor: "python", Env: []EnvVar{{Name: "SSL_CERT_FILE", Value: "/own/bundle.pem"}}}

	updated, err := ToK8s(req, existing, "ns1", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)

	// Fission refuses to merge two variables of one name, so the configured
	// ones the pool sets itself, including one the API cannot show, are left
	// out.
	assert.Empty(t, containerNamed(updated.Spec.Runtime.PodSpec, "py").Env)
	require.Len(t, updated.Spec.Runtime.Container.Env, 2)
	assert.Equal(t, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/own/bundle.pem"}, updated.Spec.Runtime.Container.Env[0])
	assert.Equal(t, "REQUESTS_CA_BUNDLE", updated.Spec.Runtime.Container.Env[1].Name)
}

func TestToK8s_ConfigurationChangesReachThePoolOnItsNextWrite(t *testing.T) {
	withCA, err := ToK8s(minimalRequest(), nil, "default", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)

	t.Run("turned off", func(t *testing.T) {
		updated, err := ToK8s(minimalRequest(), withCA.DeepCopy(), "default", testCatalog(t))
		require.NoError(t, err)
		fresh, err := ToK8s(minimalRequest(), nil, "default", testCatalog(t))
		require.NoError(t, err)

		assert.Equal(t, fresh.Spec.Runtime.PodSpec, updated.Spec.Runtime.PodSpec, "no volume, mount or variable may stay behind")
		assert.Equal(t, fresh.Spec.Builder.PodSpec, updated.Spec.Builder.PodSpec)
		assert.Equal(t, fresh.Spec.Builder.Container, updated.Spec.Builder.Container)
	})

	t.Run("another ConfigMap", func(t *testing.T) {
		cfg := trustedCAConfig()
		cfg.TrustedCA.ConfigMap = "other-ca"
		updated, err := ToK8s(minimalRequest(), withCA.DeepCopy(), "default", catalogOf(t, cfg))
		require.NoError(t, err)

		require.Len(t, updated.Spec.Runtime.PodSpec.Volumes, 1)
		assert.Equal(t, "other-ca", updated.Spec.Runtime.PodSpec.Volumes[0].ConfigMap.Name)
		require.Len(t, updated.Spec.Builder.PodSpec.Volumes, 1)
		assert.Equal(t, "other-ca", updated.Spec.Builder.PodSpec.Volumes[0].ConfigMap.Name)
		assert.Len(t, updated.Spec.Builder.Container.VolumeMounts, 1)
	})

	t.Run("build image dropped", func(t *testing.T) {
		updated, err := ToK8s(PoolRequest{Name: "my-pool", Flavor: "perl"}, withCA.DeepCopy(), "default", catalogOf(t, trustedCAConfig()))
		require.NoError(t, err)

		assert.Nil(t, updated.Spec.Builder.PodSpec)
		require.NotNil(t, updated.Spec.Builder.Container)
		assert.Nil(t, updated.Spec.Builder.Container.VolumeMounts)
		assert.Empty(t, containerNamed(updated.Spec.Runtime.PodSpec, "my-pool").Env, "perl has no runtime variables")
	})
}

func TestToK8s_TrustedCAKeepsWhatWasSetElsewhere(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Runtime.PodSpec = &corev1.PodSpec{
		Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		Containers: []corev1.Container{{
			Name:         "py",
			VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
		}},
	}

	updated, err := ToK8s(PoolRequest{Name: "py", Flavor: "python"}, existing, "ns1", catalogOf(t, trustedCAConfig()))
	require.NoError(t, err)
	podSpec := updated.Spec.Runtime.PodSpec
	assert.Equal(t, []string{"cache", "cfunc-trusted-ca"}, []string{podSpec.Volumes[0].Name, podSpec.Volumes[1].Name})
	assert.Equal(t, []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}, trustedCAMount()}, containerNamed(podSpec, "py").VolumeMounts)

	off, err := ToK8s(PoolRequest{Name: "py", Flavor: "python"}, updated, "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, existing.Spec.Runtime.PodSpec.Volumes, off.Spec.Runtime.PodSpec.Volumes)
	assert.Equal(t, []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}}, containerNamed(off.Spec.Runtime.PodSpec, "py").VolumeMounts)
}

func TestToK8s_RepeatedWriteWithTrustedCAIsUnchanged(t *testing.T) {
	catalog := catalogOf(t, trustedCAConfig())
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}
	first, err := ToK8s(req, nil, "default", catalog)
	require.NoError(t, err)

	// The response, sent back as it is, must write the same Environment.
	resp := FromK8s(first, catalog)
	assert.Equal(t, req.Env, resp.Env)
	second, err := ToK8s(PoolRequest{Name: resp.Name, Flavor: resp.Flavor, Size: resp.Size, PoolSize: &resp.PoolSize, Env: resp.Env}, first, "default", catalog)
	require.NoError(t, err)

	assert.Equal(t, first.Spec, second.Spec)
}

// -- Handler --

func TestCreate_TrustedCA(t *testing.T) {
	request := PoolRequest{Name: "py", Flavor: "python"}

	t.Run("mounted when the namespace has the ConfigMap", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t, trustedCAConfigMap("ns1", map[string]string{"ca-bundle.crt": "pem"}))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))

		w := h.PostJSON(nsPath, request)

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		env := stored(t, k8sClient, "py")
		assert.Equal(t, []corev1.Volume{trustedCAVolume()}, env.Spec.Runtime.PodSpec.Volumes)
		assert.Empty(t, decode[PoolResponse](t, w).Env)
	})

	t.Run("refused without the ConfigMap, before the namespace is labelled", func(t *testing.T) {
		cfg := trustedCAConfig()
		cfg.OnboardNamespaces = true
		k8sClient := testutil.FakeClient(t,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}},
			trustedCAConfigMap("other", map[string]string{"ca-bundle.crt": "pem"}))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, cfg))

		w := h.PostJSON(nsPath, request)

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, errorMessage(t, w), "namespace ns1 has no ConfigMap trusted-ca")
		var envs fissionv1.EnvironmentList
		require.NoError(t, k8sClient.List(context.Background(), &envs))
		assert.Empty(t, envs.Items)
		var ns corev1.Namespace
		require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Name: "ns1"}, &ns))
		assert.NotContains(t, ns.Labels, FissionEnabledLabel)
	})

	t.Run("refused with an empty ConfigMap", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t, trustedCAConfigMap("ns1", nil))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))

		w := h.PostJSON(nsPath, request)

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, errorMessage(t, w), "ConfigMap trusted-ca in namespace ns1 is empty")
	})

	t.Run("a caller who may not read ConfigMaps is let through", func(t *testing.T) {
		k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, fmt.Errorf("no"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))

		require.Equal(t, http.StatusCreated, h.PostJSON(nsPath, request).Code)
	})

	t.Run("another read failure is not taken for a missing ConfigMap", func(t *testing.T) {
		k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return k8serrors.NewTimeoutError("the api server is slow", 1)
			},
		})
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))

		w := h.PostJSON(nsPath, request)

		assert.GreaterOrEqual(t, w.Code, http.StatusInternalServerError, w.Body.String())
		assert.NotContains(t, w.Body.String(), "has no ConfigMap")
	})

	t.Run("not read when no bundle is configured", func(t *testing.T) {
		k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					t.Errorf("ConfigMap %s read with no trusted CA configured", key)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, testConfig()))

		require.Equal(t, http.StatusCreated, h.PostJSON(nsPath, request).Code)
	})
}

func TestUpdate_TrustedCA(t *testing.T) {
	t.Run("refused without the ConfigMap, and the pool is left as it was", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t, pythonEnv("py", "ns1"))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))
		before := stored(t, k8sClient, "py")

		w := h.PutJSON(nsPath+"/py", PoolRequest{Name: "py", Flavor: "python"})

		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Equal(t, before, stored(t, k8sClient, "py"))
	})

	t.Run("a pool written before the bundle was configured gets it on its next PUT", func(t *testing.T) {
		k8sClient := testutil.FakeClient(t, pythonEnv("py", "ns1"), trustedCAConfigMap("ns1", map[string]string{"ca-bundle.crt": "pem"}))
		h := testutil.NewEngineHelper(t, k8sClient, mustHandler(t, trustedCAConfig()))

		w := h.PutJSON(nsPath+"/py", PoolRequest{Name: "py", Flavor: "python"})

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		env := stored(t, k8sClient, "py")
		assert.Equal(t, []corev1.VolumeMount{trustedCAMount()}, containerNamed(env.Spec.Runtime.PodSpec, "py").VolumeMounts)
		assert.Equal(t, []corev1.VolumeMount{trustedCAMount()}, env.Spec.Builder.Container.VolumeMounts)
	})
}
