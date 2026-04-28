package capps

import (
	"testing"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	capp := ToK8s(minimalRequest(), "ns1")
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
	capp := ToK8s(req, "ns1")
	assert.Equal(t, "app.example.com", capp.Spec.RouteSpec.Hostname)
	assert.True(t, capp.Spec.RouteSpec.TlsEnabled)
	require.NotNil(t, capp.Spec.RouteSpec.RouteTimeoutSeconds)
	assert.Equal(t, int64(30), *capp.Spec.RouteSpec.RouteTimeoutSeconds)
}

func TestToK8s_WithLogSpec(t *testing.T) {
	req := minimalRequest()
	req.LogSpec = &LogSpec{Type: "elastic", Host: "es.example.com", Index: "logs", User: "admin", PasswordSecret: "pw-secret"}
	capp := ToK8s(req, "ns1")
	assert.Equal(t, cappv1alpha1.LogType("elastic"), capp.Spec.LogSpec.Type)
	assert.Equal(t, "es.example.com", capp.Spec.LogSpec.Host)
}

func TestToK8s_WithNFSVolumes(t *testing.T) {
	req := minimalRequest()
	req.NFSVolumes = []NFSVolume{{Name: "data", Server: "nfs.local", Path: "/export", Capacity: "10Gi"}}
	capp := ToK8s(req, "ns1")
	require.Len(t, capp.Spec.VolumesSpec.NFSVolumes, 1)
	assert.Equal(t, "data", capp.Spec.VolumesSpec.NFSVolumes[0].Name)
}

func TestToK8s_WithSources(t *testing.T) {
	min := int32(1)
	max := int32(10)
	req := minimalRequest()
	req.Sources = []KedaSource{{Name: "src", ScalarType: "kafka", MinReplicas: &min, MaxReplicas: &max}}
	capp := ToK8s(req, "ns1")
	require.Len(t, capp.Spec.Sources, 1)
	assert.Equal(t, "kafka", capp.Spec.Sources[0].ScalarType)
}

func TestToK8s_WithEnvVars(t *testing.T) {
	req := minimalRequest()
	req.Env = []EnvVar{{Name: "FOO", Value: "bar"}}
	capp := ToK8s(req, "ns1")
	require.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env, 1)
	assert.Equal(t, "FOO", capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].Env[0].Name)
}

func TestToK8s_WithVolumeMounts(t *testing.T) {
	req := minimalRequest()
	req.VolumeMounts = []VolumeMount{{Name: "data", MountPath: "/data"}}
	capp := ToK8s(req, "ns1")
	require.Len(t, capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/data", capp.Spec.ConfigurationSpec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
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

func TestFromK8s_WithSources(t *testing.T) {
	capp := minimalCapp()
	min := int32(1)
	capp.Spec.Sources = []cappv1alpha1.KedaSource{{Name: "src", ScalarType: "kafka", MinReplicas: &min}}
	resp := FromK8s(capp)
	require.Len(t, resp.Sources, 1)
	assert.Equal(t, "kafka", resp.Sources[0].ScalarType)
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

// -- filterAnnotations tests --

func TestFilterAnnotations_StripKubectl(t *testing.T) {
	annotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"app.example.com/version":                         "1.0",
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
