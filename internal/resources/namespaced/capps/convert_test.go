package capps

import (
	"testing"

	"github.com/dana-team/capp-backend/internal/config"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func minimalSizes() config.CappSizes {
	small := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "100m", Memory: "128Mi"},
		Limits:   config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
	}
	medium := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "200m", Memory: "256Mi"},
		Limits:   config.ResourceQuantities{CPU: "400m", Memory: "512Mi"},
	}
	large := config.ResourceSize{
		Requests: config.ResourceQuantities{CPU: "400m", Memory: "512Mi"},
		Limits:   config.ResourceQuantities{CPU: "800m", Memory: "1Gi"},
	}
	return config.CappSizes{
		Small:  small,
		Medium: medium,
		Large:  large,
	}
}

func minimalRequest() CappRequest {
	return CappRequest{
		Name:      "my-app",
		Namespace: "ns1",
		Image:     "nginx:latest",
	}
}

func minimalCapp() *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "ns1"},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: knativev1.ConfigurationSpec{
				Template: knativev1.RevisionTemplateSpec{
					Spec: knativev1.RevisionSpec{
						PodSpec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "nginx:latest"}},
						},
					},
				},
			},
		},
	}
}

// -- ToK8s tests --

func TestToK8s_MinimalRequest(t *testing.T) {
	capp, err := ToK8s(minimalRequest(), nil, "ns1", minimalSizes())
	require.NoError(t, err)
	require.NotNil(t, capp)
	assert.Equal(t, "my-app", capp.Name)
	assert.Equal(t, "ns1", capp.Namespace)
	assert.Equal(t, "Capp", capp.Kind)
	assert.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Containers, 1)
	assert.Equal(t, "nginx:latest", capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
}

func TestToK8s_WithRoute(t *testing.T) {
	req := minimalRequest()
	timeout := int64(30)
	req.RouteSpec = &RouteSpec{Hostname: "app.example.com", TLSEnabled: true, RouteTimeoutSeconds: &timeout}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Equal(t, "app.example.com", capp.Spec.RouteSpec.Hostname)
	assert.True(t, capp.Spec.RouteSpec.TlsEnabled)
	require.NotNil(t, capp.Spec.RouteSpec.RouteTimeoutSeconds)
	assert.Equal(t, int64(30), *capp.Spec.RouteSpec.RouteTimeoutSeconds)
}

func TestToK8s_WithLogSpec(t *testing.T) {
	req := minimalRequest()
	req.LogSpec = &LogSpec{Type: "elastic", Host: "es.example.com", Index: "logs", User: "admin", PasswordSecret: "pw-secret"}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Equal(t, cappv1alpha1.LogType("elastic"), capp.Spec.LogSpec.Type)
	assert.Equal(t, "es.example.com", capp.Spec.LogSpec.Host)
}

func TestToK8s_WithNFSVolumes(t *testing.T) {
	req := minimalRequest()
	req.NFSVolumes = []NFSVolume{{Name: "data", Server: "nfs.local", Path: "/export", Capacity: "10Gi"}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	require.Len(t, capp.Spec.VolumesSpec.NFSVolumes, 1)
	assert.Equal(t, "data", capp.Spec.VolumesSpec.NFSVolumes[0].Name)
}

func TestToK8s_InvalidNFSCapacity(t *testing.T) {
	req := minimalRequest()
	req.NFSVolumes = []NFSVolume{{Name: "data", Server: "nfs.local", Path: "/export", Capacity: "not-a-quantity"}}
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid NFS volume capacity")
}

func TestToK8s_WithScaleSpec(t *testing.T) {
	req := minimalRequest()
	req.ScaleSpec = ScaleSpec{Metric: "cpu", MinReplicas: 2, ScaleDelaySeconds: 30}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Equal(t, "cpu", capp.Spec.ScaleSpec.Metric)
	assert.Equal(t, 2, capp.Spec.ScaleSpec.MinReplicas)
	assert.Equal(t, 30, capp.Spec.ScaleSpec.ScaleDelaySeconds)
}

func TestToK8s_WithEnvVars(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "FOO", Value: "bar"}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	require.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env, 1)
	assert.Equal(t, "FOO", capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env[0].Name)
}

func TestToK8s_WithVolumeMounts(t *testing.T) {
	req := minimalRequest()
	req.VolumeMounts = []VolumeMount{{Name: "data", MountPath: "/data"}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	require.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/data", capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
}

func TestToK8s_ExistingPreservedOnUpdate(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"cpu": resource.MustParse("500m")},
	}
	req := minimalRequest()
	updated, err := ToK8s(req, existing, "ns1", minimalSizes())
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, resource.MustParse("500m"), updated.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources.Limits["cpu"])
}

// -- FromK8s tests --

func TestFromK8s_MinimalCapp(t *testing.T) {
	capp := minimalCapp()
	resp := FromK8s(capp)
	assert.Equal(t, "my-app", resp.Name)
	assert.Equal(t, "ns1", resp.Namespace)
	assert.Equal(t, "nginx:latest", resp.Image)
	assert.Equal(t, "main", resp.ContainerName)
}

func TestFromK8s_WithHostname(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.RouteSpec.Hostname = "app.example.com"
	resp := FromK8s(capp)
	require.NotNil(t, resp.RouteSpec)
	assert.Equal(t, "app.example.com", resp.RouteSpec.Hostname)
}

func TestFromK8s_WithTLS(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.RouteSpec.TlsEnabled = true
	resp := FromK8s(capp)
	require.NotNil(t, resp.RouteSpec)
	assert.True(t, resp.RouteSpec.TLSEnabled)
}

func TestFromK8s_WithTimeout(t *testing.T) {
	capp := minimalCapp()
	timeout := int64(60)
	capp.Spec.RouteSpec.RouteTimeoutSeconds = &timeout
	resp := FromK8s(capp)
	require.NotNil(t, resp.RouteSpec)
	require.NotNil(t, resp.RouteSpec.RouteTimeoutSeconds)
	assert.Equal(t, int64(60), *resp.RouteSpec.RouteTimeoutSeconds)
}

func TestFromK8s_WithLogSpec(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.LogSpec = cappv1alpha1.LogSpec{Type: "elastic", Host: "es.local"}
	resp := FromK8s(capp)
	require.NotNil(t, resp.LogSpec)
	assert.Equal(t, "elastic", resp.LogSpec.Type)
}

func TestFromK8s_WithNFSVolumes(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.VolumesSpec.NFSVolumes = []cappv1alpha1.NFSVolume{
		{Name: "vol", Server: "nfs.local", Path: "/export", Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
	}
	resp := FromK8s(capp)
	require.Len(t, resp.NFSVolumes, 1)
	assert.Equal(t, "10Gi", resp.NFSVolumes[0].Capacity)
}

func TestFromK8s_WithScaleSpec(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ScaleSpec = cappv1alpha1.ScaleSpec{Metric: "cpu", MinReplicas: 3, ScaleDelaySeconds: 60}
	resp := FromK8s(capp)
	assert.Equal(t, "cpu", resp.ScaleSpec.Metric)
	assert.Equal(t, 3, resp.ScaleSpec.MinReplicas)
	assert.Equal(t, 60, resp.ScaleSpec.ScaleDelaySeconds)
}

func TestFromK8s_CreationTimestamp_Formatted(t *testing.T) {
	capp := minimalCapp()
	capp.CreationTimestamp = metav1.Now()
	resp := FromK8s(capp)
	assert.NotEmpty(t, resp.CreatedAt)
	assert.Contains(t, resp.CreatedAt, "T")
}

func TestFromK8s_NoRouteSpec_WhenEmpty(t *testing.T) {
	capp := minimalCapp()
	resp := FromK8s(capp)
	assert.Nil(t, resp.RouteSpec)
}

func TestFromK8s_NoLogSpec_WhenEmpty(t *testing.T) {
	capp := minimalCapp()
	resp := FromK8s(capp)
	assert.Nil(t, resp.LogSpec)
}

func TestToK8s_EnvVarValueFrom_SecretKeyRef(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{
		Name:      "DB_PASS",
		ValueFrom: &EnvVarSource{SecretKeyRef: &KeySelector{Name: "db-secret", Key: "password"}},
	}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	env := capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env
	require.Len(t, env, 1)
	require.NotNil(t, env[0].ValueFrom)
	require.NotNil(t, env[0].ValueFrom.SecretKeyRef)
	assert.Equal(t, "db-secret", env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", env[0].ValueFrom.SecretKeyRef.Key)
	assert.Nil(t, env[0].ValueFrom.ConfigMapKeyRef)
}

func TestToK8s_EnvVarValueFrom_ConfigMapKeyRef(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{
		Name:      "LOG_LEVEL",
		ValueFrom: &EnvVarSource{ConfigMapKeyRef: &KeySelector{Name: "app-config", Key: "log.level"}},
	}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	env := capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env
	require.Len(t, env, 1)
	require.NotNil(t, env[0].ValueFrom)
	require.NotNil(t, env[0].ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, "app-config", env[0].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "log.level", env[0].ValueFrom.ConfigMapKeyRef.Key)
	assert.Nil(t, env[0].ValueFrom.SecretKeyRef)
}

func TestToK8s_EnvVarValueFrom_BothRefs_Error(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{
		Name: "AMBIGUOUS",
		ValueFrom: &EnvVarSource{
			SecretKeyRef:    &KeySelector{Name: "s", Key: "k"},
			ConfigMapKeyRef: &KeySelector{Name: "c", Key: "k"},
		},
	}}
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AMBIGUOUS")
	assert.Contains(t, err.Error(), "exactly one")
}

func TestToK8s_EnvVarValueFrom_NeitherRef_Error(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "EMPTY", ValueFrom: &EnvVarSource{}}}
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EMPTY")
	assert.Contains(t, err.Error(), "exactly one")
}

func TestToK8s_SecretVolumes(t *testing.T) {
	req := minimalRequest()
	req.SecretVolumes = []SecretVolume{{Name: "sec-vol", SecretName: "my-secret", MountPath: "/etc/sec"}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	podSpec := capp.Spec.ConfigurationSpec.Template.Spec
	require.Len(t, podSpec.Volumes, 1)
	require.NotNil(t, podSpec.Volumes[0].Secret)
	assert.Equal(t, "my-secret", podSpec.Volumes[0].Secret.SecretName)
	mounts := podSpec.Containers[0].VolumeMounts
	require.Len(t, mounts, 1)
	assert.Equal(t, "sec-vol", mounts[0].Name)
	assert.Equal(t, "/etc/sec", mounts[0].MountPath)
}

func TestToK8s_ConfigMapVolumes(t *testing.T) {
	req := minimalRequest()
	req.ConfigMapVolumes = []ConfigMapVolume{{Name: "cm-vol", ConfigMapName: "my-config", MountPath: "/etc/cfg"}}
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	podSpec := capp.Spec.ConfigurationSpec.Template.Spec
	require.Len(t, podSpec.Volumes, 1)
	require.NotNil(t, podSpec.Volumes[0].ConfigMap)
	assert.Equal(t, "my-config", podSpec.Volumes[0].ConfigMap.Name)
	mounts := podSpec.Containers[0].VolumeMounts
	require.Len(t, mounts, 1)
	assert.Equal(t, "cm-vol", mounts[0].Name)
	assert.Equal(t, "/etc/cfg", mounts[0].MountPath)
}

func TestToK8s_DuplicateVolumeName_SecretVsVolumeMount(t *testing.T) {
	req := minimalRequest()
	req.VolumeMounts = []VolumeMount{{Name: "vol", MountPath: "/a"}}
	req.SecretVolumes = []SecretVolume{{Name: "vol", SecretName: "s", MountPath: "/b"}}
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vol")
}

func TestToK8s_DuplicateVolumeName_ConfigMapVsSecret(t *testing.T) {
	req := minimalRequest()
	req.SecretVolumes = []SecretVolume{{Name: "vol", SecretName: "s", MountPath: "/a"}}
	req.ConfigMapVolumes = []ConfigMapVolume{{Name: "vol", ConfigMapName: "c", MountPath: "/b"}}
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vol")
}

func TestFromK8s_SecretVolumes_RoundTrip(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "sec-vol",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "my-secret"}},
	}}
	capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "sec-vol", MountPath: "/etc/sec",
	}}
	resp := FromK8s(capp)
	require.Len(t, resp.SecretVolumes, 1)
	assert.Equal(t, "sec-vol", resp.SecretVolumes[0].Name)
	assert.Equal(t, "my-secret", resp.SecretVolumes[0].SecretName)
	assert.Equal(t, "/etc/sec", resp.SecretVolumes[0].MountPath)
}

func TestFromK8s_ConfigMapVolumes_RoundTrip(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{
		Name: "cm-vol",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"},
		}},
	}}
	capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "cm-vol", MountPath: "/etc/cfg",
	}}
	resp := FromK8s(capp)
	require.Len(t, resp.ConfigMapVolumes, 1)
	assert.Equal(t, "cm-vol", resp.ConfigMapVolumes[0].Name)
	assert.Equal(t, "my-config", resp.ConfigMapVolumes[0].ConfigMapName)
	assert.Equal(t, "/etc/cfg", resp.ConfigMapVolumes[0].MountPath)
}

// -- PUT semantics: full replacement, not partial update --

func TestToK8s_UpdateClearsEnvWhenNotProvided(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "OLD_VAR", Value: "old"},
	}
	updated, err := ToK8s(minimalRequest(), existing, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Empty(t, updated.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env)
}

func TestToK8s_UpdateClearsVolumesWhenNotProvided(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "old-vol",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "old-secret"}},
	}}
	existing.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "old-vol", MountPath: "/old",
	}}
	updated, err := ToK8s(minimalRequest(), existing, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Empty(t, updated.Spec.ConfigurationSpec.Template.Spec.Volumes)
	assert.Empty(t, updated.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts)
}

func TestToK8s_UpdateReplacesOldPodVolumes(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "old-vol",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "old-secret"}},
	}}
	existing.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "old-vol", MountPath: "/old",
	}}
	req := minimalRequest()
	req.SecretVolumes = []SecretVolume{{Name: "new-vol", SecretName: "new-secret", MountPath: "/new"}}
	updated, err := ToK8s(req, existing, "ns1", minimalSizes())
	require.NoError(t, err)
	podSpec := updated.Spec.ConfigurationSpec.Template.Spec
	require.Len(t, podSpec.Volumes, 1)
	assert.Equal(t, "new-vol", podSpec.Volumes[0].Name)
	require.Len(t, podSpec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "new-vol", podSpec.Containers[0].VolumeMounts[0].Name)
}

func TestToK8s_UpdateClearsRouteSpecWhenNil(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.RouteSpec = cappv1alpha1.RouteSpec{Hostname: "app.example.com"}
	updated, err := ToK8s(minimalRequest(), existing, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Equal(t, cappv1alpha1.RouteSpec{}, updated.Spec.RouteSpec)
}

func TestToK8s_UpdateClearsLogSpecWhenNil(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.LogSpec = cappv1alpha1.LogSpec{Type: "elastic", Host: "es.local"}
	updated, err := ToK8s(minimalRequest(), existing, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Equal(t, cappv1alpha1.LogSpec{}, updated.Spec.LogSpec)
}

func TestToK8s_UpdateClearsNFSVolumesWhenNotProvided(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.VolumesSpec = cappv1alpha1.VolumesSpec{
		NFSVolumes: []cappv1alpha1.NFSVolume{{Name: "old-nfs", Server: "nfs.local", Path: "/export"}},
	}
	updated, err := ToK8s(minimalRequest(), existing, "ns1", minimalSizes())
	require.NoError(t, err)
	assert.Empty(t, updated.Spec.VolumesSpec.NFSVolumes)
}

func TestToK8s_SetResourceBySize(t *testing.T) {
	req := minimalRequest()
	req.Size = "medium"
	capp, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.NoError(t, err)
	resources := capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources
	assert.Equal(t, resource.MustParse("200m"), resources.Requests["cpu"])
	assert.Equal(t, resource.MustParse("256Mi"), resources.Requests["memory"])
	assert.Equal(t, resource.MustParse("400m"), resources.Limits["cpu"])
	assert.Equal(t, resource.MustParse("512Mi"), resources.Limits["memory"])
}

func TestToK8s_PreserveResourcesIfSizeNotSet(t *testing.T) {
	existing := minimalCapp()
	existing.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"cpu": resource.MustParse("500m")},
		Limits:   corev1.ResourceList{"memory": resource.MustParse("1Gi")},
	}
	req := minimalRequest()
	req.Size = "" // Size not set, should preserve existing resources
	updated, err := ToK8s(req, existing, "ns1", minimalSizes())
	require.NoError(t, err)
	resources := updated.Spec.ConfigurationSpec.Template.Spec.Containers[0].Resources
	assert.Equal(t, resource.MustParse("500m"), resources.Requests["cpu"])
	assert.Equal(t, resource.MustParse("1Gi"), resources.Limits["memory"])
}

func TestToK8s_InvalidSize(t *testing.T) {
	req := minimalRequest()
	req.Size = "extra-large" // Not defined in sizes config
	_, err := ToK8s(req, nil, "ns1", minimalSizes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid size")
}

func TestFromK8s_UnmountedVolume_Skipped(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ConfigurationSpec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "orphan",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}},
	}}
	// No VolumeMounts entry for "orphan".
	resp := FromK8s(capp)
	assert.Empty(t, resp.SecretVolumes)
}

func TestFromK8s_EnvVarValueFrom_SecretKeyRef(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "DB_PASS",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
				Key:                  "password",
			},
		},
	}}
	resp := FromK8s(capp)
	require.Len(t, resp.Env, 1)
	require.NotNil(t, resp.Env[0].ValueFrom)
	require.NotNil(t, resp.Env[0].ValueFrom.SecretKeyRef)
	assert.Equal(t, "db-secret", resp.Env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "password", resp.Env[0].ValueFrom.SecretKeyRef.Key)
}

func TestFromK8s_EnvVarValueFrom_ConfigMapKeyRef(t *testing.T) {
	capp := minimalCapp()
	capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "LOG_LEVEL",
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
				Key:                  "log.level",
			},
		},
	}}
	resp := FromK8s(capp)
	require.Len(t, resp.Env, 1)
	require.NotNil(t, resp.Env[0].ValueFrom)
	require.NotNil(t, resp.Env[0].ValueFrom.ConfigMapKeyRef)
	assert.Equal(t, "app-config", resp.Env[0].ValueFrom.ConfigMapKeyRef.Name)
	assert.Equal(t, "log.level", resp.Env[0].ValueFrom.ConfigMapKeyRef.Key)
}

// -- filterAnnotations tests --

func TestFilterAnnotations_StripKubectl(t *testing.T) {
	annotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"app.example.com/version":                          "1.0",
	}
	result := filterAnnotations(annotations)
	assert.Len(t, result, 1)
	assert.Equal(t, "1.0", result["app.example.com/version"])
}

func TestFilterAnnotations_EmptyInput(t *testing.T) {
	assert.Nil(t, filterAnnotations(nil))
	assert.Nil(t, filterAnnotations(map[string]string{}))
}

func TestFilterAnnotations_AllStripped(t *testing.T) {
	annotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
	}
	assert.Nil(t, filterAnnotations(annotations))
}
