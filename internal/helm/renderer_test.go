package helm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ── ParseManifests ───────────────────────────────────────────────────────────

func TestParseManifests(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     int
		wantErr  bool
	}{
		{
			name: "multiple documents",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm2
`,
			want: 2,
		},
		{
			name:     "empty string",
			manifest: "",
			want:     0,
		},
		{
			name: "empty documents skipped",
			manifest: `---
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
---
`,
			want: 1,
		},
		{
			name: "comment-only document skipped",
			manifest: `# Source: templates/notes.txt
---
apiVersion: v1
kind: Service
metadata:
  name: svc1
`,
			want: 1,
		},
		{
			name: "invalid YAML",
			manifest: `apiVersion: v1
kind: ConfigMap
  bad-indent: true
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, err := ParseManifests(tt.manifest)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, objects, tt.want)
		})
	}
}

func TestParseManifests_PreservesFields(t *testing.T) {
	manifest := `apiVersion: batch/v1
kind: Job
metadata:
  name: my-job
  namespace: test-ns
`
	objects, err := ParseManifests(manifest)
	require.NoError(t, err)
	require.Len(t, objects, 1)

	assert.Equal(t, "batch/v1", objects[0].GetAPIVersion())
	assert.Equal(t, "Job", objects[0].GetKind())
	assert.Equal(t, "my-job", objects[0].GetName())
	assert.Equal(t, "test-ns", objects[0].GetNamespace())
}

// ── StripHelmAnnotations ─────────────────────────────────────────────────────

func TestStripHelmAnnotations(t *testing.T) {
	objects := []unstructured.Unstructured{
		{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "test",
					"annotations": map[string]interface{}{
						"helm.sh/hook":        "pre-install",
						"helm.sh/hook-weight": "5",
						"app.io/keep":         "yes",
					},
				},
			},
		},
		{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "sa",
					"annotations": map[string]interface{}{
						"helm.sh/resource-policy": "keep",
					},
				},
			},
		},
		{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name": "secret",
				},
			},
		},
	}

	StripHelmAnnotations(objects)

	cm := objects[0]
	assert.Equal(t, map[string]string{"app.io/keep": "yes"}, cm.GetAnnotations())

	sa := objects[1]
	assert.Empty(t, sa.GetAnnotations())

	secret := objects[2]
	assert.Empty(t, secret.GetAnnotations())
}

// ── FilterByGVK ──────────────────────────────────────────────────────────────

func TestFilterByGVK(t *testing.T) {
	objects := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "job1"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata":   map[string]interface{}{"name": "sa1"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata":   map[string]interface{}{"name": "job2"},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "cm1"},
		}},
	}

	jobs := FilterByGVK(objects, schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
	assert.Len(t, jobs, 2)
	assert.Equal(t, "job1", jobs[0].GetName())
	assert.Equal(t, "job2", jobs[1].GetName())

	sas := FilterByGVK(objects, schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"})
	assert.Len(t, sas, 1)
	assert.Equal(t, "sa1", sas[0].GetName())

	secrets := FilterByGVK(objects, schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	assert.Empty(t, secrets)
}

// ── SetBenchmarkMeta ─────────────────────────────────────────────────────────

func TestSetBenchmarkMeta(t *testing.T) {
	objects := []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata": map[string]interface{}{
				"name": "job1",
				"labels": map[string]interface{}{
					"existing": "label",
				},
				"annotations": map[string]interface{}{
					"existing": "annotation",
				},
			},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata":   map[string]interface{}{"name": "sa1"},
		}},
	}

	SetBenchmarkMeta(objects, "my-capp")

	// Job — had existing labels and annotations, should be merged.
	job := objects[0]
	assert.Equal(t, "true", job.GetLabels()[LabelBenchmark])
	assert.Equal(t, "label", job.GetLabels()["existing"])
	assert.Equal(t, "my-capp", job.GetAnnotations()[AnnotationCappName])
	assert.Equal(t, "annotation", job.GetAnnotations()["existing"])

	// ServiceAccount — had no labels or annotations.
	sa := objects[1]
	assert.Equal(t, "true", sa.GetLabels()[LabelBenchmark])
	assert.Equal(t, "my-capp", sa.GetAnnotations()[AnnotationCappName])
}
