package containerspec

import (
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	api "github.com/dana-team/capp-backend/pkg/api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestEnvVarsToK8s(t *testing.T) {
	got, err := EnvVarsToK8s([]api.EnvVar{
		{Name: "PLAIN", Value: "val"},
		{Name: "FROM_SECRET", ValueFrom: &api.EnvVarSource{SecretKeyRef: &api.KeySelector{Name: "s1", Key: "k1"}}},
		{Name: "FROM_CM", ValueFrom: &api.EnvVarSource{ConfigMapKeyRef: &api.KeySelector{Name: "cm1", Key: "k2"}}},
	})
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, corev1.EnvVar{Name: "PLAIN", Value: "val"}, got[0])
	require.NotNil(t, got[1].ValueFrom.SecretKeyRef)
	assert.Equal(t, "s1", got[1].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "k1", got[1].ValueFrom.SecretKeyRef.Key)
	require.NotNil(t, got[2].ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, "cm1", got[2].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "k2", got[2].ValueFrom.ConfigMapKeyRef.Key)
}

func TestEnvVarsToK8s_Empty(t *testing.T) {
	got, err := EnvVarsToK8s(nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestEnvVarsToK8s_Invalid(t *testing.T) {
	tests := []struct {
		name        string
		vars        []api.EnvVar
		errContains string
	}{
		{"no name", []api.EnvVar{{Name: "OK", Value: "1"}, {Value: "x"}}, "env[1]: name is required"},
		{"valueFrom with no reference", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{}}}, `env var "X": valueFrom must set exactly one`},
		{
			"valueFrom with two references",
			[]api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{
				SecretKeyRef:    &api.KeySelector{Name: "s", Key: "k"},
				ConfigMapKeyRef: &api.KeySelector{Name: "c", Key: "k"},
			}}},
			"valueFrom must set exactly one",
		},
		{"secret with no name and key", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{SecretKeyRef: &api.KeySelector{}}}}, `env var "X": secretKeyRef needs both name and key`},
		{"secret with no key", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{SecretKeyRef: &api.KeySelector{Name: "s"}}}}, "secretKeyRef needs both name and key"},
		{"secret with no name", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{SecretKeyRef: &api.KeySelector{Key: "k"}}}}, "secretKeyRef needs both name and key"},
		{"config map with no key", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{ConfigMapKeyRef: &api.KeySelector{Name: "c"}}}}, "configMapKeyRef needs both name and key"},
		{"config map with no name", []api.EnvVar{{Name: "X", ValueFrom: &api.EnvVarSource{ConfigMapKeyRef: &api.KeySelector{Key: "k"}}}}, "configMapKeyRef needs both name and key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EnvVarsToK8s(tt.vars)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

func TestEnvVarsFromK8s_RoundTrip(t *testing.T) {
	vars := []api.EnvVar{
		{Name: "PLAIN", Value: "val"},
		{Name: "FROM_SECRET", ValueFrom: &api.EnvVarSource{SecretKeyRef: &api.KeySelector{Name: "s1", Key: "k1"}}},
		{Name: "FROM_CM", ValueFrom: &api.EnvVarSource{ConfigMapKeyRef: &api.KeySelector{Name: "cm1", Key: "k2"}}},
	}
	k8sVars, err := EnvVarsToK8s(vars)
	require.NoError(t, err)

	assert.Equal(t, vars, EnvVarsFromK8s(k8sVars))
}

func TestEnvVarsFromK8s_LeavesOutWhatTheDTOCannotExpress(t *testing.T) {
	got := EnvVarsFromK8s([]corev1.EnvVar{
		{Name: "PLAIN", Value: "val"},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
	})
	assert.Equal(t, []api.EnvVar{{Name: "PLAIN", Value: "val"}}, got)

	_, err := EnvVarsToK8s(got)
	assert.NoError(t, err, "what EnvVarsFromK8s returns must always be accepted back")
}

func TestMergeEnvVars(t *testing.T) {
	podIP := corev1.EnvVar{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}
	cpuLimit := corev1.EnvVar{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}
	existing := []corev1.EnvVar{{Name: "OLD", Value: "1"}, podIP, cpuLimit}

	assert.Equal(t, []corev1.EnvVar{{Name: "NEW", Value: "2"}, podIP, cpuLimit},
		MergeEnvVars([]corev1.EnvVar{{Name: "NEW", Value: "2"}}, existing), "expressible variables are replaced, the others are kept")
	assert.Equal(t, []corev1.EnvVar{podIP, cpuLimit}, MergeEnvVars(nil, existing), "an empty request removes only what the client could see")
	assert.Equal(t, []corev1.EnvVar{{Name: "POD_IP", Value: "10.0.0.1"}, cpuLimit},
		MergeEnvVars([]corev1.EnvVar{{Name: "POD_IP", Value: "10.0.0.1"}}, existing), "a requested variable of the same name wins")
	assert.Nil(t, MergeEnvVars(nil, []corev1.EnvVar{{Name: "OLD", Value: "1"}}))
	assert.Nil(t, MergeEnvVars(nil, nil))
}

func TestRepresentable(t *testing.T) {
	assert.True(t, Representable(corev1.EnvVar{Name: "A", Value: "1"}))
	assert.True(t, Representable(corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}))
	assert.True(t, Representable(corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{}}}))
	assert.False(t, Representable(corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}))
	assert.False(t, Representable(corev1.EnvVar{Name: "A", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}))
}

func TestParseResourceQuantities(t *testing.T) {
	got, err := ParseResourceQuantities(
		config.ResourceQuantities{CPU: "100m", Memory: "128Mi"},
		config.ResourceQuantities{CPU: "1", Memory: "1Gi"},
	)
	require.NoError(t, err)
	assert.Zero(t, got.Requests.Cpu().Cmp(resource.MustParse("100m")))
	assert.Zero(t, got.Requests.Memory().Cmp(resource.MustParse("128Mi")))
	assert.Zero(t, got.Limits.Cpu().Cmp(resource.MustParse("1")))
	assert.Zero(t, got.Limits.Memory().Cmp(resource.MustParse("1Gi")))
}

func TestParseResourceQuantities_RequiresAllFour(t *testing.T) {
	_, err := ParseResourceQuantities(
		config.ResourceQuantities{CPU: "100m"},
		config.ResourceQuantities{CPU: "1", Memory: "1Gi"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid requests.memory")
}

func TestParseCustomResources_Partial(t *testing.T) {
	got, err := ParseCustomResources(&api.ResourceSpec{Limits: &api.ResourceQuantities{CPU: "500m"}})
	require.NoError(t, err)
	assert.Nil(t, got.Requests)
	require.Len(t, got.Limits, 1)
	assert.Zero(t, got.Limits.Cpu().Cmp(resource.MustParse("500m")))
}

func TestParseCustomResources_Invalid(t *testing.T) {
	_, err := ParseCustomResources(&api.ResourceSpec{Requests: &api.ResourceQuantities{Memory: "lots"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid requests.memory "lots"`)
}

func TestResourceSpecFromK8s(t *testing.T) {
	assert.Nil(t, ResourceSpecFromK8s(corev1.ResourceRequirements{}))

	got := ResourceSpecFromK8s(corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	})
	require.NotNil(t, got)
	assert.Equal(t, &api.ResourceQuantities{CPU: "100m"}, got.Requests)
	assert.Equal(t, &api.ResourceQuantities{CPU: "1", Memory: "1Gi"}, got.Limits)
}
