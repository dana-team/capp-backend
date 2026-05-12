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
			ScaleSpec: cappv1alpha1.ScaleSpec{Metric: "concurrency"},
			State:     "enabled",
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

	var vals syncValues
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "my-app", vals.Name)
	assert.Equal(t, "default", vals.Namespace)
	assert.Equal(t, "concurrency", vals.Spec.ScaleSpec.Metric)
	assert.Equal(t, "enabled", vals.Spec.State)
	require.Len(t, vals.Spec.ConfigurationSpec.Template.Spec.Containers, 1)
	assert.Equal(t, "nginx:latest", vals.Spec.ConfigurationSpec.Template.Spec.Containers[0].Image)
	assert.Equal(t, "main", vals.Spec.ConfigurationSpec.Template.Spec.Containers[0].Name)
	assert.Empty(t, vals.Spec.RouteSpec.Hostname)
	assert.Empty(t, string(vals.Spec.LogSpec.Type))
}

func TestGenerateValues_FullCapp(t *testing.T) {
	timeout := int64(300)
	capp := &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{Name: "full-app", Namespace: "prod"},
		Spec: cappv1alpha1.CappSpec{
			ScaleSpec: cappv1alpha1.ScaleSpec{Metric: "cpu", MinReplicas: 2},
			State:     "enabled",
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
		},
	}

	out, err := GenerateValues(capp)
	require.NoError(t, err)

	var vals syncValues
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "full-app", vals.Name)
	assert.Equal(t, "prod", vals.Namespace)
	assert.Equal(t, "cpu", vals.Spec.ScaleSpec.Metric)
	assert.Equal(t, 2, vals.Spec.ScaleSpec.MinReplicas)

	require.Len(t, vals.Spec.ConfigurationSpec.Template.Spec.Containers, 1)
	c := vals.Spec.ConfigurationSpec.Template.Spec.Containers[0]
	assert.Equal(t, "myimage:v2", c.Image)
	require.Len(t, c.Env, 1)
	assert.Equal(t, "LOG_LEVEL", c.Env[0].Name)
	assert.Equal(t, "debug", c.Env[0].Value)
	require.Len(t, c.VolumeMounts, 1)
	assert.Equal(t, "data", c.VolumeMounts[0].Name)
	assert.Equal(t, "/data", c.VolumeMounts[0].MountPath)

	assert.Equal(t, "app.example.com", vals.Spec.RouteSpec.Hostname)
	assert.True(t, vals.Spec.RouteSpec.TlsEnabled)
	require.NotNil(t, vals.Spec.RouteSpec.RouteTimeoutSeconds)
	assert.Equal(t, int64(300), *vals.Spec.RouteSpec.RouteTimeoutSeconds)

	assert.Equal(t, cappv1alpha1.LogType("elastic"), vals.Spec.LogSpec.Type)
	assert.Equal(t, "es.example.com", vals.Spec.LogSpec.Host)
	assert.Equal(t, "app-logs", vals.Spec.LogSpec.Index)

	require.Len(t, vals.Spec.VolumesSpec.NFSVolumes, 1)
	nfs := vals.Spec.VolumesSpec.NFSVolumes[0]
	assert.Equal(t, "shared", nfs.Name)
	assert.Equal(t, "nfs.example.com", nfs.Server)
	assert.Equal(t, "/exports/shared", nfs.Path)
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

	var vals syncValues
	require.NoError(t, yaml.Unmarshal(out, &vals))

	assert.Equal(t, "empty", vals.Name)
	assert.Equal(t, "disabled", vals.Spec.State)
	assert.Empty(t, vals.Spec.ConfigurationSpec.Template.Spec.Containers)
}
