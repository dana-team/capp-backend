package pools

import (
	"testing"
	"time"

	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func minimalRequest() PoolRequest {
	return PoolRequest{Name: "my-pool", Flavor: "python"}
}

func minimalEnvironment() *fissionv1.Environment {
	return &fissionv1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "default"},
		Spec: fissionv1.EnvironmentSpec{
			Version: 3,
			Runtime: fissionv1.Runtime{Image: pyImage},
		},
	}
}

func TestToK8s_MinimalRequest(t *testing.T) {
	env, err := ToK8s(minimalRequest(), nil, "default", testCatalog(t))
	require.NoError(t, err)

	assert.Equal(t, "fission.io/v1", env.APIVersion)
	assert.Equal(t, "Environment", env.Kind)
	assert.Equal(t, "my-pool", env.Name)
	assert.Equal(t, "default", env.Namespace)
	assert.Equal(t, 3, env.Spec.Version)
	assert.Equal(t, int64(360), env.Spec.TerminationGracePeriod)
	assert.Equal(t, pyImage, env.Spec.Runtime.Image)
	assert.Equal(t, prefix+"/python-builder:1.35.0", env.Spec.Builder.Image)
	assert.Equal(t, "build", env.Spec.Builder.Command, "an empty command makes the Fission builder run /build, which fails")
	assert.Equal(t, "regcred", env.Spec.ImagePullSecret)
	assert.Equal(t, 3, env.Spec.Poolsize)
	assertResources(t, resourcesOf("100m", "128Mi", "200m", "256Mi"), env.Spec.Resources)
	assert.Empty(t, env.Labels)
	assert.Empty(t, env.Annotations)
}

func TestToK8s_WithSize(t *testing.T) {
	req := minimalRequest()
	req.Size = "large"

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assertResources(t, resourcesOf("500m", "512Mi", "1", "1Gi"), env.Spec.Resources)
}

func TestToK8s_WithPoolSizeZero(t *testing.T) {
	zero := 0
	req := minimalRequest()
	req.PoolSize = &zero

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Zero(t, env.Spec.Poolsize)
}

func TestToK8s_FlavorBuildSettings(t *testing.T) {
	python, err := ToK8s(minimalRequest(), nil, "default", testCatalog(t))
	require.NoError(t, err)
	require.NotNil(t, python.Spec.Builder.Container)
	assert.Equal(t, fissionv1.BuilderContainerName, python.Spec.Builder.Container.Name)
	assert.Equal(t, []corev1.EnvVar{{Name: "PIP_INDEX_URL", Value: "https://registry.example/pypi"}}, python.Spec.Builder.Container.Env)
	assert.False(t, python.Spec.KeepArchive)

	req := minimalRequest()
	req.Flavor = "java"
	java, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.True(t, java.Spec.KeepArchive)
	assert.Equal(t, "build", java.Spec.Builder.Command)
	assert.Nil(t, java.Spec.Builder.Container, "a flavor with no builder variables must not create a builder container")
}

func TestToK8s_FlavorWithoutBuilder(t *testing.T) {
	req := minimalRequest()
	req.Flavor = "perl"

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Empty(t, env.Spec.Builder.Image, "an empty build image must not become the bare registry prefix")
	assert.Empty(t, env.Spec.Builder.Command, "no build image, so no default command")
}

func TestToK8s_WithAdvanced(t *testing.T) {
	req := minimalRequest()
	req.Advanced = &Advanced{
		BuildImage:   "other.example/team/builder:2",
		BuildCommand: "./build.sh",
		Resources: &ResourceSpec{
			Requests: &ResourceQuantities{CPU: "150m"},
			Limits:   &ResourceQuantities{CPU: "600m", Memory: "400Mi"},
		},
	}

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, pyImage, env.Spec.Runtime.Image, "advanced must not change the runtime image")
	assert.Equal(t, "other.example/team/builder:2", env.Spec.Builder.Image, "an advanced build image is used as written, without the prefix")
	assert.Equal(t, "./build.sh", env.Spec.Builder.Command)
	assertResources(t, resourcesOf("150m", "128Mi", "600m", "400Mi"), env.Spec.Resources)
}

func TestToK8s_PartialAdvancedResourcesAreFilledFromTheSize(t *testing.T) {
	tests := []struct {
		name      string
		size      string
		resources ResourceSpec
		want      corev1.ResourceRequirements
	}{
		{"one request, default size", "", ResourceSpec{Requests: &ResourceQuantities{CPU: "1m"}}, resourcesOf("1m", "128Mi", "200m", "256Mi")},
		{"one limit, named size", "large", ResourceSpec{Limits: &ResourceQuantities{Memory: "768Mi"}}, resourcesOf("500m", "512Mi", "1", "768Mi")},
		{"all four", "", ResourceSpec{
			Requests: &ResourceQuantities{CPU: "300m", Memory: "200Mi"},
			Limits:   &ResourceQuantities{CPU: "600m", Memory: "400Mi"},
		}, resourcesOf("300m", "200Mi", "600m", "400Mi")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := minimalRequest()
			req.Size = tt.size
			req.Advanced = &Advanced{Resources: &tt.resources}

			env, err := ToK8s(req, nil, "default", testCatalog(t))
			require.NoError(t, err)
			assertResources(t, tt.want, env.Spec.Resources)
			assert.Len(t, env.Spec.Resources.Limits, 2, "a pool must never be left without limits")
		})
	}
}

func TestToK8s_AdvancedRequestAboveTheSizeLimitItInherits(t *testing.T) {
	req := minimalRequest()
	req.Advanced = &Advanced{Resources: &ResourceSpec{Requests: &ResourceQuantities{CPU: "500m"}}}

	_, err := ToK8s(req, nil, "default", testCatalog(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "advanced.resources.requests.cpu 500m is larger than its limit 200m")
}

func TestToK8s_PoolSizeLimit(t *testing.T) {
	atLimit, above := 10, 11

	req := minimalRequest()
	req.PoolSize = &atLimit
	_, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)

	req.PoolSize = &above
	_, err = ToK8s(req, nil, "default", testCatalog(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poolSize 11 is above the maximum of 10")
}

func TestFromK8s_AdvancedCarriesWhatDiffersFromFlavorAndSize(t *testing.T) {
	plain, err := ToK8s(minimalRequest(), nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Nil(t, FromK8s(plain, testCatalog(t)).Advanced, "a pool that is exactly its flavor and size has no advanced block")

	req := minimalRequest()
	req.Advanced = &Advanced{
		BuildImage: "other.example/team/builder:2",
		Resources:  &ResourceSpec{Limits: &ResourceQuantities{Memory: "400Mi"}},
	}
	custom, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)

	advanced := FromK8s(custom, testCatalog(t)).Advanced
	require.NotNil(t, advanced)
	assert.Equal(t, "other.example/team/builder:2", advanced.BuildImage)
	assert.Empty(t, advanced.BuildCommand, "the default command is not a custom value")
	assert.Equal(t, &ResourceSpec{
		Requests: &ResourceQuantities{CPU: "100m", Memory: "128Mi"},
		Limits:   &ResourceQuantities{CPU: "200m", Memory: "400Mi"},
	}, advanced.Resources)

	again, err := ToK8s(PoolRequest{Name: "my-pool", Flavor: "python", Advanced: advanced}, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, custom.Spec.Builder, again.Spec.Builder, "the advanced block of a response must reproduce the pool")
	assertResources(t, custom.Spec.Resources, again.Spec.Resources)
}

func TestToK8s_WithEnvVars(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	require.NotNil(t, env.Spec.Runtime.Container)
	assert.Equal(t, "my-pool", env.Spec.Runtime.Container.Name, "Fission names the runtime container after the Environment")
	assert.Equal(t, []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}, env.Spec.Runtime.Container.Env)
}

func TestToK8s_NoEnvVars_NoRuntimeContainer(t *testing.T) {
	env, err := ToK8s(minimalRequest(), nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Nil(t, env.Spec.Runtime.Container)
}

func TestToK8s_EnvVarValueFrom_SecretKeyRef(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "DB_PASSWORD", ValueFrom: &EnvVarSource{SecretKeyRef: &KeySelector{Name: "db", Key: "password"}}}}

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	got := env.Spec.Runtime.Container.Env
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ValueFrom)
	require.NotNil(t, got[0].ValueFrom.SecretKeyRef)
	assert.Equal(t, "db", got[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", got[0].ValueFrom.SecretKeyRef.Key)
	assert.Empty(t, got[0].Value)
}

func TestToK8s_EnvVarValueFrom_ConfigMapKeyRef(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "REGION", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &KeySelector{Name: "settings", Key: "region"}}}}

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	got := env.Spec.Runtime.Container.Env
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, "settings", got[0].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "region", got[0].ValueFrom.ConfigMapKeyRef.Key)
}

func TestToK8s_EnvVarValueFrom_BothRefs_Error(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "X", ValueFrom: &EnvVarSource{
		SecretKeyRef:    &KeySelector{Name: "s", Key: "k"},
		ConfigMapKeyRef: &KeySelector{Name: "c", Key: "k"},
	}}}

	_, err := ToK8s(req, nil, "default", testCatalog(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `env var "X": valueFrom must set exactly one of secretKeyRef or configMapKeyRef`)
}

func TestToK8s_EnvVarValueFrom_NeitherRef_Error(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "X", ValueFrom: &EnvVarSource{}}}

	_, err := ToK8s(req, nil, "default", testCatalog(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "valueFrom must set exactly one")
}

func TestToK8s_EnvVarsReplacedOnUpdate(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Runtime.Container = &corev1.Container{
		Name:            "py",
		Env:             []corev1.EnvVar{{Name: "OLD", Value: "1"}, {Name: "KEEP", Value: "2"}},
		ImagePullPolicy: corev1.PullAlways,
	}

	replaced, err := ToK8s(PoolRequest{Name: "py", Flavor: "python", Env: []EnvVar{{Name: "NEW", Value: "3"}}}, existing, "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, []corev1.EnvVar{{Name: "NEW", Value: "3"}}, replaced.Spec.Runtime.Container.Env, "PUT replaces the whole list")
	assert.Equal(t, corev1.PullAlways, replaced.Spec.Runtime.Container.ImagePullPolicy, "the rest of the container must survive")

	cleared, err := ToK8s(PoolRequest{Name: "py", Flavor: "python"}, existing, "ns1", testCatalog(t))
	require.NoError(t, err)
	require.NotNil(t, cleared.Spec.Runtime.Container)
	assert.Empty(t, cleared.Spec.Runtime.Container.Env, "omitting env on update clears it")
	assert.Equal(t, corev1.PullAlways, cleared.Spec.Runtime.Container.ImagePullPolicy)

	assert.Len(t, existing.Spec.Runtime.Container.Env, 2, "ToK8s must not mutate the existing object")
}

func TestToK8s_AdvancedBuildImageGetsDefaultCommand(t *testing.T) {
	req := minimalRequest()
	req.Flavor = "perl"
	req.Advanced = &Advanced{BuildImage: "other.example/perl-builder:1"}

	env, err := ToK8s(req, nil, "default", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, "other.example/perl-builder:1", env.Spec.Builder.Image)
	assert.Equal(t, "build", env.Spec.Builder.Command)
}

func TestToK8s_EmptyAdvancedResourcesUseTheSize(t *testing.T) {
	for name, resources := range map[string]*ResourceSpec{
		"empty block":           {},
		"empty requests/limits": {Requests: &ResourceQuantities{}, Limits: &ResourceQuantities{}},
	} {
		t.Run(name, func(t *testing.T) {
			req := minimalRequest()
			req.Size = "large"
			req.Advanced = &Advanced{Resources: resources}

			env, err := ToK8s(req, nil, "default", testCatalog(t))
			require.NoError(t, err)
			assertResources(t, resourcesOf("500m", "512Mi", "1", "1Gi"), env.Spec.Resources)
		})
	}
}

func TestToK8s_AdvancedResourcesAreCappedAtTheLargestSize(t *testing.T) {
	tests := []struct {
		name        string
		resources   ResourceSpec
		errContains string
	}{
		{"cpu limit", ResourceSpec{Limits: &ResourceQuantities{CPU: "1001m"}}, "advanced.resources.limits.cpu 1001m is above the largest configured size (1)"},
		{"memory limit", ResourceSpec{Limits: &ResourceQuantities{Memory: "2Gi"}}, "advanced.resources.limits.memory 2Gi is above the largest configured size (1Gi)"},
		{"request with no limit", ResourceSpec{Requests: &ResourceQuantities{CPU: "4"}}, "advanced.resources.requests.cpu 4 is above the largest configured size (1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := minimalRequest()
			req.Advanced = &Advanced{Resources: &tt.resources}

			_, err := ToK8s(req, nil, "default", testCatalog(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}

	req := minimalRequest()
	req.Advanced = &Advanced{Resources: &ResourceSpec{Limits: &ResourceQuantities{CPU: "1", Memory: "1Gi"}}}
	_, err := ToK8s(req, nil, "default", testCatalog(t))
	assert.NoError(t, err, "exactly the largest size is allowed")
}

func TestToK8s_CustomBuildDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.AllowCustomBuild = false

	for name, advanced := range map[string]Advanced{
		"build image":   {BuildImage: "other.example/builder:1"},
		"build command": {BuildCommand: "./build.sh"},
	} {
		t.Run(name, func(t *testing.T) {
			req := minimalRequest()
			req.Advanced = &advanced

			_, err := ToK8s(req, nil, "default", catalogOf(t, cfg))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not enabled on this backend")
		})
	}

	env, err := ToK8s(minimalRequest(), nil, "default", catalogOf(t, cfg))
	require.NoError(t, err, "the flavor's own build settings need no switch")
	assert.Equal(t, prefix+"/python-builder:1.35.0", env.Spec.Builder.Image)
}

func TestToK8s_ZeroGracePeriodIsTreatedAsUnset(t *testing.T) {
	zero := pythonEnv("py", "ns1")
	zero.Spec.TerminationGracePeriod = 0
	updated, err := ToK8s(PoolRequest{Name: "py", Flavor: "python"}, zero, "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, int64(360), updated.Spec.TerminationGracePeriod)

	custom := pythonEnv("py", "ns1")
	custom.Spec.TerminationGracePeriod = 120
	updated, err = ToK8s(PoolRequest{Name: "py", Flavor: "python"}, custom, "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, int64(120), updated.Spec.TerminationGracePeriod, "a value set outside the API is kept")
}

func TestToK8s_UnrepresentableEnvVarsArePreserved(t *testing.T) {
	podIP := corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}
	cpuLimit := corev1.EnvVar{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}
	existing := func() *fissionv1.Environment {
		env := pythonEnv("py", "ns1")
		env.Spec.Runtime.Container = &corev1.Container{Name: "py", Env: []corev1.EnvVar{{Name: "OLD", Value: "1"}, podIP, cpuLimit}}
		return env
	}

	replaced, err := ToK8s(PoolRequest{Name: "py", Flavor: "python", Env: []EnvVar{{Name: "NEW", Value: "2"}}}, existing(), "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, []corev1.EnvVar{{Name: "NEW", Value: "2"}, podIP, cpuLimit}, replaced.Spec.Runtime.Container.Env)

	omitted, err := ToK8s(PoolRequest{Name: "py", Flavor: "python"}, existing(), "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, []corev1.EnvVar{podIP, cpuLimit}, omitted.Spec.Runtime.Container.Env, "omitting env removes only what the client could see")

	overridden, err := ToK8s(PoolRequest{Name: "py", Flavor: "python", Env: []EnvVar{{Name: "POD_IP", Value: "10.0.0.1"}}}, existing(), "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, []corev1.EnvVar{{Name: "POD_IP", Value: "10.0.0.1"}, cpuLimit}, overridden.Spec.Runtime.Container.Env, "a request variable of the same name wins")
}

func TestFromK8s_LeavesOutUnrepresentableEnvVars(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Runtime.Container = &corev1.Container{Name: "my-pool", Env: []corev1.EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
	}}
	assert.Equal(t, []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}, FromK8s(env, testCatalog(t)).Env)

	env.Spec.Runtime.Container.Env = env.Spec.Runtime.Container.Env[1:]
	assert.Nil(t, FromK8s(env, testCatalog(t)).Env)
}

func TestToK8s_EnvVarKeySelectorNeedsNameAndKey(t *testing.T) {
	for name, source := range map[string]*EnvVarSource{
		"secret, both empty":  {SecretKeyRef: &KeySelector{}},
		"secret, no key":      {SecretKeyRef: &KeySelector{Name: "db"}},
		"secret, no name":     {SecretKeyRef: &KeySelector{Key: "password"}},
		"config map, no key":  {ConfigMapKeyRef: &KeySelector{Name: "settings"}},
		"config map, no name": {ConfigMapKeyRef: &KeySelector{Key: "region"}},
	} {
		t.Run(name, func(t *testing.T) {
			req := minimalRequest()
			req.Env = []EnvVar{{Name: "X", ValueFrom: source}}

			_, err := ToK8s(req, nil, "default", testCatalog(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "needs both name and key")
		})
	}
}

func TestToK8s_RuntimeContainerNamedAfterThePool(t *testing.T) {
	updated, err := ToK8s(PoolRequest{Name: "py", Flavor: "python", Env: []EnvVar{{Name: "A", Value: "1"}}}, pythonEnv("py", "ns1"), "ns1", testCatalog(t))
	require.NoError(t, err)
	assert.Equal(t, "py", updated.Spec.Runtime.Container.Name)
}

func TestToK8s_InvalidRequest(t *testing.T) {
	negative := -1
	tests := []struct {
		name        string
		mutate      func(*PoolRequest)
		errContains string
	}{
		{"unknown flavor", func(r *PoolRequest) { r.Flavor = "cobol" }, `unknown flavor "cobol"; available flavors: java, perl, python`},
		{"flavor names are lowercase", func(r *PoolRequest) { r.Flavor = "Python" }, `unknown flavor "Python"`},
		{"unknown size", func(r *PoolRequest) { r.Size = "huge" }, `unknown size "huge"; available sizes: large, small`},
		{"negative pool size", func(r *PoolRequest) { r.PoolSize = &negative }, "poolSize must be 0 or greater"},
		{
			"build command with no build image",
			func(r *PoolRequest) { r.Flavor = "perl"; r.Advanced = &Advanced{BuildCommand: "make"} },
			`flavor "perl" has none; set advanced.buildImage`,
		},
		{
			"invalid quantity",
			func(r *PoolRequest) {
				r.Advanced = &Advanced{Resources: &ResourceSpec{Limits: &ResourceQuantities{Memory: "lots"}}}
			},
			`advanced.resources: invalid limits.memory "lots"`,
		},
		{
			"negative quantity",
			func(r *PoolRequest) {
				r.Advanced = &Advanced{Resources: &ResourceSpec{Requests: &ResourceQuantities{CPU: "-1"}}}
			},
			"advanced.resources.requests.cpu must not be negative",
		},
		{
			"request larger than limit",
			func(r *PoolRequest) {
				r.Advanced = &Advanced{Resources: &ResourceSpec{
					Requests: &ResourceQuantities{Memory: "1Gi"},
					Limits:   &ResourceQuantities{Memory: "512Mi"},
				}}
			},
			"advanced.resources.requests.memory 1Gi is larger than its limit 512Mi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := minimalRequest()
			tt.mutate(&req)

			_, err := ToK8s(req, nil, "default", testCatalog(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

func TestToK8s_ExistingPreservedOnUpdate(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.ResourceVersion = "42"
	existing.Labels = map[string]string{"team": "a"}
	existing.Spec.Version = 2
	existing.Spec.TerminationGracePeriod = 120
	existing.Spec.Runtime.PodSpec = &corev1.PodSpec{NodeSelector: map[string]string{"pool": "faas"}}

	updated, err := ToK8s(PoolRequest{Name: "py", Flavor: "java"}, existing, "ns1", testCatalog(t))
	require.NoError(t, err)

	assert.Equal(t, "42", updated.ResourceVersion)
	assert.Equal(t, map[string]string{"team": "a"}, updated.Labels)
	assert.Equal(t, 2, updated.Spec.Version, "spec.version is immutable in the CRD")
	assert.Equal(t, int64(120), updated.Spec.TerminationGracePeriod)
	require.NotNil(t, updated.Spec.Runtime.PodSpec)
	assert.Equal(t, map[string]string{"pool": "faas"}, updated.Spec.Runtime.PodSpec.NodeSelector)
	assert.Equal(t, prefix+"/jvm-env:1.32.0", updated.Spec.Runtime.Image)

	assert.Equal(t, pyImage, existing.Spec.Runtime.Image, "ToK8s must not mutate the existing object")
}

func TestToK8s_BuilderEnvReplacedOnUpdate(t *testing.T) {
	existing := pythonEnv("py", "ns1")
	existing.Spec.Builder.Container = &corev1.Container{
		Name:      fissionv1.BuilderContainerName,
		Env:       []corev1.EnvVar{{Name: "PIP_INDEX_URL", Value: "stale"}},
		Resources: resourcesOf("1", "1Gi", "1", "1Gi"),
	}

	updated, err := ToK8s(PoolRequest{Name: "py", Flavor: "java"}, existing, "ns1", testCatalog(t))
	require.NoError(t, err)

	require.NotNil(t, updated.Spec.Builder.Container)
	assert.Empty(t, updated.Spec.Builder.Container.Env, "the java flavor has no builder variables, so python's must go")
	assertResources(t, resourcesOf("1", "1Gi", "1", "1Gi"), updated.Spec.Builder.Container.Resources)
}

func TestFromK8s_MinimalEnvironment(t *testing.T) {
	resp := FromK8s(minimalEnvironment(), testCatalog(t))

	assert.Equal(t, "my-pool", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "python", resp.Flavor)
	assert.Equal(t, pyImage, resp.Image)
	assert.Equal(t, 3, resp.Version)
	assert.Empty(t, resp.Size, "an Environment with no resources matches no size")
	assert.Nil(t, resp.Resources)
	assert.NotNil(t, resp.Status.Conditions, "conditions must encode as [], not null")
}

func TestFromK8s_SizeMatched(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Resources = resourcesOf("100m", "128Mi", "200m", "256Mi")

	resp := FromK8s(env, testCatalog(t))
	assert.Equal(t, "small", resp.Size)
	require.NotNil(t, resp.Resources)
	assert.Equal(t, &ResourceQuantities{CPU: "100m", Memory: "128Mi"}, resp.Resources.Requests)
	assert.Equal(t, &ResourceQuantities{CPU: "200m", Memory: "256Mi"}, resp.Resources.Limits)
}

func TestFromK8s_SizeMatchesAcrossQuantityNotation(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Resources = resourcesOf("0.5", "512Mi", "1000m", "1024Mi")

	assert.Equal(t, "large", FromK8s(env, testCatalog(t)).Size)
}

func TestFromK8s_NoFlavorOrSize_WhenNothingMatches(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Runtime.Image = prefix + "/python-env:1.34.0"
	env.Spec.Resources = resourcesOf("100m", "128Mi", "200m", "300Mi")

	resp := FromK8s(env, testCatalog(t))
	assert.Empty(t, resp.Flavor, "an image tag the config no longer has matches no flavor")
	assert.Empty(t, resp.Size)
	assert.Equal(t, prefix+"/python-env:1.34.0", resp.Image)
}

func TestFromK8s_WithBuilder(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Builder = fissionv1.Builder{Image: "other.example/builder:1", Command: "make"}

	resp := FromK8s(env, testCatalog(t))
	require.NotNil(t, resp.Builder)
	assert.Equal(t, "other.example/builder:1", resp.Builder.Image)
	assert.Equal(t, "make", resp.Builder.Command)
}

func TestFromK8s_WithEnvVars(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Runtime.Container = &corev1.Container{Name: "my-pool", Env: []corev1.EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password",
		}}},
	}}

	assert.Equal(t, []EnvVar{
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "DB_PASSWORD", ValueFrom: &EnvVarSource{SecretKeyRef: &KeySelector{Name: "db", Key: "password"}}},
	}, FromK8s(env, testCatalog(t)).Env)
}

func TestFromK8s_NoEnv_WhenEmpty(t *testing.T) {
	assert.Nil(t, FromK8s(minimalEnvironment(), testCatalog(t)).Env)

	env := minimalEnvironment()
	env.Spec.Runtime.Container = &corev1.Container{Name: "my-pool"}
	assert.Nil(t, FromK8s(env, testCatalog(t)).Env)
}

func TestFromK8s_NoBuilder_WhenEmpty(t *testing.T) {
	assert.Nil(t, FromK8s(minimalEnvironment(), testCatalog(t)).Builder)
}

func TestFromK8s_CreationTimestamp_Formatted(t *testing.T) {
	env := minimalEnvironment()
	env.CreationTimestamp = metav1.NewTime(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))

	assert.Equal(t, "2026-11-15T12:00:00Z", FromK8s(env, testCatalog(t)).CreatedAt)
}

func TestFromK8s_UnexposedFields(t *testing.T) {
	env := minimalEnvironment()
	env.Spec.Version = 2
	env.Spec.Poolsize = 7
	env.Spec.TerminationGracePeriod = 120
	env.Spec.AllowedFunctionsPerContainer = fissionv1.AllowedFunctionsPerContainerInfinite
	env.Spec.AllowAccessToExternalNetwork = true

	resp := FromK8s(env, testCatalog(t))
	assert.Equal(t, 2, resp.Version)
	assert.Equal(t, 7, resp.PoolSize)
	assert.Equal(t, int64(120), resp.TerminationGracePeriodSeconds)
	assert.Equal(t, "infinite", resp.AllowedFunctionsPerContainer)
	assert.True(t, resp.AllowAccessToExternalNetwork)
}

func TestFromK8s_Status(t *testing.T) {
	env := minimalEnvironment()
	env.Status = fissionv1.EnvironmentStatus{
		ObservedGeneration: 4,
		Conditions: []metav1.Condition{{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "PoolReady",
			Message:            "pool is ready",
			LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)),
		}},
	}

	status := FromK8s(env, testCatalog(t)).Status
	assert.Equal(t, int64(4), status.ObservedGeneration)
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, Condition{
		Type: "Ready", Status: "True", Reason: "PoolReady", Message: "pool is ready",
		LastTransitionTime: "2026-11-15T12:00:00Z",
	}, status.Conditions[0])
}

func TestFromK8s_UsesEmptyRegistryPrefix(t *testing.T) {
	cfg := testConfig()
	cfg.RegistryPrefix = ""
	env := minimalEnvironment()
	env.Spec.Runtime.Image = "python-env:1.35.0"

	assert.Equal(t, "python", FromK8s(env, catalogOf(t, cfg)).Flavor)
}
