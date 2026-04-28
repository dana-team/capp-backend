package gitops

import (
	"testing"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/yaml"
)

func sampleCapp() *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rcs.dana.io/v1alpha1",
			Kind:       "Capp",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-app",
			Namespace:       "prod",
			UID:             types.UID("abc-123"),
			ResourceVersion: "999",
			Generation:      5,
			Labels:          map[string]string{"team": "backend"},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"custom.io/note": "keep-me",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: cappv1alpha1.CappSpec{
			ScaleMetric: "concurrency",
			State:       "enabled",
			ConfigurationSpec: knativev1.ConfigurationSpec{
				Template: knativev1.RevisionTemplateSpec{
					Spec: knativev1.RevisionSpec{
						PodSpec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "my-app",
									Image: "registry.example.com/my-app:v1",
									Env: []corev1.EnvVar{
										{Name: "DB_HOST", Value: "postgres"},
									},
								},
							},
						},
					},
				},
			},
			RouteSpec: cappv1alpha1.RouteSpec{
				Hostname:   "my-app.example.com",
				TlsEnabled: true,
			},
		},
		Status: cappv1alpha1.CappStatus{
			StateStatus: cappv1alpha1.StateStatus{State: "running"},
		},
	}
}

func parseValues(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, yaml.Unmarshal(data, &m))
	return m
}

func minimalCappSpec() knativev1.ConfigurationSpec {
	return knativev1.ConfigurationSpec{
		Template: knativev1.RevisionTemplateSpec{
			Spec: knativev1.RevisionSpec{
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: "nginx"}},
				},
			},
		},
	}
}

func TestGenerateValues_CoreFields(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	vals := parseValues(t, out)
	assert.Equal(t, "my-app", vals["name"])
	assert.Equal(t, "prod", vals["namespace"])
	assert.Equal(t, "registry.example.com/my-app:v1", vals["image"])
	assert.Equal(t, "my-app", vals["containerName"])
	assert.Equal(t, "concurrency", vals["scaleMetric"])
	assert.Equal(t, "enabled", vals["state"])
}

func TestGenerateValues_EnvVars(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	vals := parseValues(t, out)
	env, ok := vals["env"].([]any)
	require.True(t, ok, "env should be a list")
	require.Len(t, env, 1)

	entry := env[0].(map[string]any)
	assert.Equal(t, "DB_HOST", entry["name"])
	assert.Equal(t, "postgres", entry["value"])
}

func TestGenerateValues_RouteSpec(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	vals := parseValues(t, out)
	route, ok := vals["routeSpec"].(map[string]any)
	require.True(t, ok, "routeSpec should be a map")
	assert.Equal(t, "my-app.example.com", route["hostname"])
	assert.Equal(t, true, route["tlsEnabled"])
}

func TestGenerateValues_FiltersKubectlAnnotations(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	vals := parseValues(t, out)
	annotations, ok := vals["annotations"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, annotations, "kubectl.kubernetes.io/last-applied-configuration")
	assert.Equal(t, "keep-me", annotations["custom.io/note"])
}

func TestGenerateValues_KeepsLabels(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	vals := parseValues(t, out)
	labels, ok := vals["labels"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "backend", labels["team"])
}

func TestGenerateValues_ExcludesRuntimeFields(t *testing.T) {
	out, err := GenerateValues(sampleCapp())
	require.NoError(t, err)

	content := string(out)
	assert.NotContains(t, content, "resourceVersion")
	assert.NotContains(t, content, "uid")
	assert.NotContains(t, content, "managedFields")
	assert.NotContains(t, content, "generation")
	assert.NotContains(t, content, "running")
}

func TestGenerateValues_MinimalCapp(t *testing.T) {
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bare",
			Namespace: "default",
		},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: minimalCappSpec(),
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	vals := parseValues(t, out)
	assert.Equal(t, "bare", vals["name"])
	assert.Equal(t, "default", vals["namespace"])
	assert.Equal(t, "nginx", vals["image"])
	assert.Nil(t, vals["labels"])
	assert.Nil(t, vals["annotations"])
	assert.Nil(t, vals["env"])
	assert.Nil(t, vals["routeSpec"])
}

func TestGenerateValues_NFSVolumes(t *testing.T) {
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs-app", Namespace: "ns1"},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: minimalCappSpec(),
			VolumesSpec: cappv1alpha1.VolumesSpec{
				NFSVolumes: []cappv1alpha1.NFSVolume{
					{
						Name:     "data",
						Server:   "nfs.example.com",
						Path:     "/exports/data",
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
					},
				},
			},
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	vals := parseValues(t, out)
	nfs, ok := vals["nfsVolumes"].([]any)
	require.True(t, ok)
	require.Len(t, nfs, 1)
	vol := nfs[0].(map[string]any)
	assert.Equal(t, "data", vol["name"])
	assert.Equal(t, "nfs.example.com", vol["server"])
	assert.Equal(t, "/exports/data", vol["path"])
	assert.Equal(t, "10Gi", vol["capacity"])
}

func TestGenerateValues_Sources(t *testing.T) {
	minR := int32(1)
	maxR := int32(10)
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "scaled-app", Namespace: "ns1"},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: minimalCappSpec(),
			Sources: []cappv1alpha1.KedaSource{
				{
					Name:           "kafka",
					ScalarType:     "kafka",
					ScalarMetadata: map[string]string{"topic": "events"},
					MinReplicas:    &minR,
					MaxReplicas:    &maxR,
				},
			},
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	vals := parseValues(t, out)
	sources, ok := vals["sources"].([]any)
	require.True(t, ok)
	require.Len(t, sources, 1)
	src := sources[0].(map[string]any)
	assert.Equal(t, "kafka", src["name"])
	assert.Equal(t, "kafka", src["scalarType"])

	meta, ok := src["scalarMetadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "events", meta["topic"])
}

func TestGenerateValues_LogSpec(t *testing.T) {
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "log-app", Namespace: "ns1"},
		Spec: cappv1alpha1.CappSpec{
			ConfigurationSpec: minimalCappSpec(),
			LogSpec: cappv1alpha1.LogSpec{
				Type:           cappv1alpha1.LogTypeElastic,
				Host:           "elastic.example.com",
				Index:          "my-index",
				User:           "admin",
				PasswordSecret: "es-secret",
			},
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	vals := parseValues(t, out)
	logSpec, ok := vals["logSpec"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "elastic", logSpec["type"])
	assert.Equal(t, "elastic.example.com", logSpec["host"])
	assert.Equal(t, "my-index", logSpec["index"])
	assert.Equal(t, "admin", logSpec["user"])
	assert.Equal(t, "es-secret", logSpec["passwordSecret"])
}
