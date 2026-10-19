package benchmarks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testConfig() config.BenchmarkConfig {
	return config.BenchmarkConfig{
		Enabled: true,
		Image:   "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
		VMURLs:  []string{"http://vm-a.monitoring.svc:8428"},
	}
}

func testCapp(name string) *cappv1alpha1.Capp {
	return &cappv1alpha1.Capp{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns1",
			UID:       types.UID("uid-" + name),
		},
	}
}

func newHandler() *Handler {
	return New(testConfig(), zap.NewNop())
}

func engine(t *testing.T, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, objects...), newHandler())
}

func engineWithUser(t *testing.T, user string, k8sClient client.Client) *testutil.EngineHelper {
	t.Helper()
	_, eng := gin.CreateTestContext(httptest.NewRecorder())
	eng.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Set(string(middleware.CredentialKey), auth.ClusterCredential{ImpersonateUser: user})
		c.Next()
	})
	newHandler().RegisterRoutes(eng.Group(""))
	return testutil.NewEngineHelperFrom(t, eng)
}

func benchmarkPath(name string) string {
	return "/namespaces/ns1/capps/" + name + "/benchmark"
}

func TestTrigger_CreatesJob(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	e := testutil.NewEngineHelper(t, k8sClient, newHandler())

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp TriggerResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, TriggerResponse{CappName: "my-capp", JobName: "bench-my-capp", Type: TypeLatency}, resp)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Equal(t, "true", job.Labels[LabelBenchmark])
	assert.Equal(t, "my-capp", job.Annotations[AnnotationCappName])
	_, hasUser := job.Annotations[AnnotationTriggeredBy]
	assert.False(t, hasUser)
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, types.UID("uid-my-capp"), job.OwnerReferences[0].UID)
	assert.Equal(t, "Capp", job.OwnerReferences[0].Kind)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/dana-team/capp-benchmark-runner:0.1.0", container.Image)
	assert.Contains(t, container.Command, "/scripts/latency.js")
}

func TestTrigger_RecordsUser(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	e := engineWithUser(t, "alice", k8sClient)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeScale})
	require.Equal(t, http.StatusCreated, w.Code)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Equal(t, "alice", job.Annotations[AnnotationTriggeredBy])
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Command, "/scripts/scale-test.sh")
}

func TestTrigger_Conflict(t *testing.T) {
	e := engine(t, testCapp("my-capp"))
	path := benchmarkPath("my-capp")
	first := e.PostJSON(path, TriggerRequest{Type: TypeThroughput})
	require.Equal(t, http.StatusCreated, first.Code)

	second := e.PostJSON(path, TriggerRequest{Type: TypeLatency})
	assert.Equal(t, http.StatusConflict, second.Code)
	assert.Contains(t, second.Body.String(), "already running")
}

func TestTrigger_ReplacesFinished(t *testing.T) {
	finished := benchmarkJob("bench-my-capp", "my-capp", batchv1.JobComplete)
	other := benchmarkJob("bench-other", "other", batchv1.JobComplete)
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"), testCapp("other"), finished, other)
	e := testutil.NewEngineHelper(t, k8sClient, newHandler())

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Command, "/scripts/latency.js")
	_, finishedNow := jobOutcome(&job)
	assert.False(t, finishedNow)
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-other"}, &batchv1.Job{}))
}

func TestTrigger_CappNotFound(t *testing.T) {
	w := engine(t).PostJSON(benchmarkPath("missing"), TriggerRequest{Type: TypeLatency})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTrigger_UnknownType(t *testing.T) {
	w := engine(t, testCapp("my-capp")).PostJSON(benchmarkPath("my-capp"), map[string]string{"type": "cold-start"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "unknown benchmark type")
}

func TestStatus_NotRunning(t *testing.T) {
	w := engine(t, testCapp("my-capp")).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, StatusResponse{}, resp)
}

func TestStatus_Running(t *testing.T) {
	w := engine(t, benchmarkJob("bench-my-capp", "my-capp", "")).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, StatusResponse{JobName: "bench-my-capp", Status: StatusRunning}, resp)
}

func TestStatus_Completed(t *testing.T) {
	w := engine(t, benchmarkJob("bench-my-capp", "my-capp", batchv1.JobComplete)).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, StatusResponse{JobName: "bench-my-capp", Status: StatusCompleted}, resp)
}

func TestStatus_Failed(t *testing.T) {
	w := engine(t, benchmarkJob("bench-my-capp", "my-capp", batchv1.JobFailed)).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, StatusResponse{JobName: "bench-my-capp", Status: StatusFailed}, resp)
}

func TestStatus_IgnoresOtherCapp(t *testing.T) {
	other := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-other",
			Namespace: "ns1",
			Labels:    map[string]string{LabelBenchmark: "true"},
			Annotations: map[string]string{
				AnnotationCappName: "other",
			},
		},
	}
	w := engine(t, other).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, StatusResponse{}, resp)
}

func benchmarkJob(name, cappName string, condition batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns1",
			Labels:    map[string]string{LabelBenchmark: labelValueTrue},
			Annotations: map[string]string{
				AnnotationCappName: cappName,
			},
		},
	}
	if condition != "" {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type:   condition,
			Status: corev1.ConditionTrue,
		}}
	}
	return job
}
