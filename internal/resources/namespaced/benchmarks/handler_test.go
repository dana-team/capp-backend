package benchmarks

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/helm"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// mockRenderer implements chartRenderer for testing.
type mockRenderer struct {
	objects []unstructured.Unstructured
	err     error
}

func (m *mockRenderer) Render(_ context.Context, _ helm.RenderOpts) ([]unstructured.Unstructured, error) {
	return m.objects, m.err
}

func renderedObjects() []unstructured.Unstructured {
	return []unstructured.Unstructured{
		{Object: map[string]interface{}{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata": map[string]interface{}{
				"name":      "bench-my-capp-test",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "runner",
								"image": "busybox",
							},
						},
						"restartPolicy": "Never",
					},
				},
			},
		}},
		{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata": map[string]interface{}{
				"name":      "bench-my-capp-sa",
				"namespace": "default",
			},
		}},
	}
}

func benchmarkJob(name, cappName string, active, succeeded int32) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				helm.LabelBenchmark: "true",
			},
			Annotations: map[string]string{
				helm.AnnotationCappName: cappName,
			},
		},
		Status: batchv1.JobStatus{
			Active:    active,
			Succeeded: succeeded,
		},
	}
}

// ── Trigger ──────────────────────────────────────────────────────────────────

func TestTrigger_Success(t *testing.T) {
	mock := &mockRenderer{objects: renderedObjects()}
	handler := New(mock, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t), handler)

	w := eng.PostJSON("/namespaces/default/capps/my-capp/benchmark", BenchmarkRequest{
		Values: map[string]interface{}{"type": "k6-latency"},
	})

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp BenchmarkResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-capp", resp.CappName)
	assert.Equal(t, 2, resp.Objects)
}

func TestTrigger_RenderError(t *testing.T) {
	mock := &mockRenderer{err: assert.AnError}
	handler := New(mock, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t), handler)

	w := eng.PostJSON("/namespaces/default/capps/my-capp/benchmark", BenchmarkRequest{})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTrigger_InvalidBody(t *testing.T) {
	mock := &mockRenderer{objects: renderedObjects()}
	handler := New(mock, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t), handler)

	w := eng.Post("/namespaces/default/capps/my-capp/benchmark", nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTrigger_ConflictWhenBenchmarkExists(t *testing.T) {
	existing := benchmarkJob("bench-my-capp-test", "my-capp", 1, 0)

	mock := &mockRenderer{objects: renderedObjects()}
	handler := New(mock, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t, existing), handler)

	w := eng.PostJSON("/namespaces/default/capps/my-capp/benchmark", BenchmarkRequest{
		Values: map[string]interface{}{"type": "k6-latency"},
	})

	assert.Equal(t, http.StatusConflict, w.Code)
}

// ── Status ───────────────────────────────────────────────────────────────────

func TestStatus_Found(t *testing.T) {
	job := benchmarkJob("bench-my-capp-latency", "my-capp", 1, 0)

	handler := New(nil, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t, job), handler)

	w := eng.Get("/namespaces/default/capps/my-capp/benchmark")

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-capp", resp.CappName)
	assert.True(t, resp.Found)
	assert.Equal(t, "bench-my-capp-latency", resp.JobName)
	assert.Equal(t, int32(1), resp.Active)
}

func TestStatus_NotFound(t *testing.T) {
	handler := New(nil, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t), handler)

	w := eng.Get("/namespaces/default/capps/my-capp/benchmark")

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Found)
}

func TestStatus_FiltersOtherCapps(t *testing.T) {
	myJob := benchmarkJob("bench-my-capp-test", "my-capp", 0, 1)
	otherJob := benchmarkJob("bench-other-test", "other-capp", 0, 1)

	handler := New(nil, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t, myJob, otherJob), handler)

	w := eng.Get("/namespaces/default/capps/my-capp/benchmark")

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Found)
	assert.Equal(t, "bench-my-capp-test", resp.JobName)
}

// ── Cleanup ──────────────────────────────────────────────────────────────────

func TestCleanup_Success(t *testing.T) {
	job := benchmarkJob("bench-my-capp-test", "my-capp", 0, 1)

	handler := New(nil, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t, job), handler)

	w := eng.Delete("/namespaces/default/capps/my-capp/benchmark")

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestCleanup_NoJobs(t *testing.T) {
	handler := New(nil, zap.NewNop())
	eng := testutil.NewEngineHelper(t, testutil.FakeClient(t), handler)

	w := eng.Delete("/namespaces/default/capps/my-capp/benchmark")

	assert.Equal(t, http.StatusNoContent, w.Code)
}
