package capps

import (
	"testing"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/yaml"
)

func TestGenerateValues_MinimalCapp(t *testing.T) {
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: cappv1alpha1.CappSpec{
			ScaleMetric: "concurrency",
			State:       "enabled",
			ConfigurationSpec: knativev1.ConfigurationSpec{
				Template: knativev1.RevisionTemplateSpec{
					Spec: knativev1.RevisionSpec{
						PodSpec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "nginx:latest"},
							},
						},
					},
				},
			},
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	var vals map[string]interface{}
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "my-app", vals["name"])
	assert.Equal(t, "default", vals["namespace"])
	assert.Equal(t, "nginx:latest", vals["image"])
	assert.Equal(t, "main", vals["containerName"])
	assert.Equal(t, "concurrency", vals["scaleMetric"])
	assert.Equal(t, "enabled", vals["state"])
	assert.Nil(t, vals["routeSpec"])
	assert.Nil(t, vals["logSpec"])
}

func TestGenerateValues_FullCapp(t *testing.T) {
	timeout := int64(300)
	minReplicas := int32(1)
	maxReplicas := int32(10)
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "full-app", Namespace: "prod"},
		Spec: cappv1alpha1.CappSpec{
			ScaleMetric: "cpu",
			State:       "enabled",
			MinReplicas: 2,
			ConfigurationSpec: knativev1.ConfigurationSpec{
				Template: knativev1.RevisionTemplateSpec{
					Spec: knativev1.RevisionSpec{
						PodSpec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "myimage:v2",
									Env: []corev1.EnvVar{
										{Name: "LOG_LEVEL", Value: "debug"},
									},
									VolumeMounts: []corev1.VolumeMount{
										{Name: "data", MountPath: "/data"},
									},
								},
							},
						},
					},
				},
			},
			RouteSpec: cappv1alpha1.RouteSpec{
				Hostname:            "app.example.com",
				TlsEnabled:          true,
				RouteTimeoutSeconds: &timeout,
			},
			LogSpec: cappv1alpha1.LogSpec{
				Type:           "elastic",
				Host:           "es.example.com",
				Index:          "app-logs",
				User:           "logger",
				PasswordSecret: "es-secret",
			},
			VolumesSpec: cappv1alpha1.VolumesSpec{
				NFSVolumes: []cappv1alpha1.NFSVolume{
					{
						Name:     "shared",
						Server:   "nfs.example.com",
						Path:     "/exports/shared",
						Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
					},
				},
			},
			Sources: []cappv1alpha1.KedaSource{
				{
					Name:           "kafka-trigger",
					ScalarType:     "kafka",
					ScalarMetadata: map[string]string{"topic": "events"},
					MinReplicas:    &minReplicas,
					MaxReplicas:    &maxReplicas,
				},
			},
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	var vals map[string]interface{}
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "full-app", vals["name"])
	assert.Equal(t, "prod", vals["namespace"])
	assert.Equal(t, "myimage:v2", vals["image"])
	assert.Equal(t, "cpu", vals["scaleMetric"])
	assert.InDelta(t, 2, vals["minReplicas"], 0.01)

	routeSpec := vals["routeSpec"].(map[string]interface{})
	assert.Equal(t, "app.example.com", routeSpec["hostname"])
	assert.Equal(t, true, routeSpec["tlsEnabled"])

	logSpec := vals["logSpec"].(map[string]interface{})
	assert.Equal(t, "elastic", logSpec["type"])
	assert.Equal(t, "es.example.com", logSpec["host"])

	envList := vals["env"].([]interface{})
	assert.Len(t, envList, 1)

	nfsVolumes := vals["nfsVolumes"].([]interface{})
	assert.Len(t, nfsVolumes, 1)

	sources := vals["sources"].([]interface{})
	assert.Len(t, sources, 1)
}

func TestGenerateValues_NoContainers(t *testing.T) {
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "ns"},
		Spec: cappv1alpha1.CappSpec{
			State: "disabled",
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	var vals map[string]interface{}
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "empty", vals["name"])
	assert.Equal(t, "", vals["image"])
}
