package benchmarks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
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
	knativev1 "knative.dev/serving/pkg/apis/serving/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testSAToken = "sa-token-value"

func testConfig() config.BenchmarkConfig {
	return config.BenchmarkConfig{
		Enabled:  true,
		Image:    "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
		VMURLs:   []string{"http://vm-a.monitoring.svc:8428"},
		QueryURL: "http://vm-query.monitoring.svc:8428",
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
	return clusterEngine(t, testutil.FakeClient(t, objects...), testSAToken)
}

func engineWithUser(t *testing.T, user string, k8sClient client.Client) *testutil.EngineHelper {
	t.Helper()
	_, eng := gin.CreateTestContext(httptest.NewRecorder())
	eng.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Set(string(middleware.CredentialKey), auth.ClusterCredential{ImpersonateUser: user})
		c.Set(string(middleware.ClusterMetaKey), cluster.ClusterMeta{Name: "c1", BenchmarkSAToken: testSAToken})
		c.Next()
	})
	newHandler().RegisterRoutes(eng.Group(""))
	return testutil.NewEngineHelperFrom(t, eng)
}

func benchmarkPath(name string) string {
	return "/namespaces/ns1/capps/" + name + "/benchmark"
}

func metricsPath(name string) string {
	return "/namespaces/ns1/capps/" + name + "/metrics"
}

func TestTrigger_CreatesJob(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp TriggerResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.RunID)
	resp.RunID = ""
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
	assert.Contains(t, container.Command, "/scripts/run-k6.sh")
}

func TestTrigger_PassesPathAndHeaders(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{
		Type:    TypeLatency,
		Path:    "/api/orders?limit=5",
		Headers: map[string]string{"X-Team": "blue"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	env := envMap(job.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "/api/orders?limit=5", env["TARGET_PATH"])
	assert.JSONEq(t, `{"X-Team":"blue"}`, env["TARGET_HEADERS"])
}

// jobUIDClient assigns Jobs a UID on create, as the API server does.
func jobUIDClient(t *testing.T, createErr error, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testutil.TestScheme(t)).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret && createErr != nil {
					return createErr
				}
				if _, isJob := obj.(*batchv1.Job); isJob {
					obj.SetUID("job-uid")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

// clusterEngine serves requests as if routed to a cluster holding token.
func clusterEngine(t *testing.T, k8sClient client.Client, token string) *testutil.EngineHelper {
	t.Helper()
	meta := cluster.ClusterMeta{Name: "c1", BenchmarkSAToken: token}
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, newHandler())
}

func TestTrigger_UsesTokenOfRequestedCluster(t *testing.T) {
	for _, token := range []string{"east-token", "west-token"} {
		k8sClient := jobUIDClient(t, nil, testCapp("my-capp"))

		w := clusterEngine(t, k8sClient, token).PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeScale})

		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var secrets corev1.SecretList
		require.NoError(t, k8sClient.List(t.Context(), &secrets, client.InNamespace("ns1")))
		require.Len(t, secrets.Items, 1)
		assert.Equal(t, token, string(secrets.Items[0].Data["token"]))
	}
}

func TestTrigger_ScaleWithoutClusterTokenLeavesJobsAlone(t *testing.T) {
	finished := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "bench-my-capp",
			Namespace:   "ns1",
			Labels:      map[string]string{LabelBenchmark: labelValueTrue},
			Annotations: map[string]string{AnnotationCappName: "my-capp"},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	k8sClient := jobUIDClient(t, nil, testCapp("my-capp"), finished)

	w := clusterEngine(t, k8sClient, "").PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeScale})

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Equal(t, finished.Name, job.Name, "the previous result must survive")
}

func TestTrigger_ScaleStoresSATokenInSecretOwnedByJob(t *testing.T) {
	k8sClient := jobUIDClient(t, nil, testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeScale})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), testSAToken)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	jobSpec, err := json.Marshal(job)
	require.NoError(t, err)
	assert.NotContains(t, string(jobSpec), testSAToken)

	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(t.Context(), &secrets, client.InNamespace("ns1")))
	require.Len(t, secrets.Items, 1)
	secret := secrets.Items[0]
	assert.Equal(t, testSAToken, string(secret.Data["token"]))
	require.Len(t, secret.OwnerReferences, 1)
	assert.Equal(t, "Job", secret.OwnerReferences[0].Kind)
	assert.Equal(t, types.UID("job-uid"), secret.OwnerReferences[0].UID)

	var ref *corev1.SecretKeySelector
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "BENCHMARK_SA_TOKEN" {
			ref = env.ValueFrom.SecretKeyRef
		}
	}
	require.NotNil(t, ref)
	assert.Equal(t, secret.Name, ref.Name)
}

func TestTrigger_OtherTypesCreateNoSecret(t *testing.T) {
	k8sClient := jobUIDClient(t, nil, testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var secrets corev1.SecretList
	require.NoError(t, k8sClient.List(t.Context(), &secrets, client.InNamespace("ns1")))
	assert.Empty(t, secrets.Items)
}

func TestTrigger_SecretFailureRemovesJob(t *testing.T) {
	k8sClient := jobUIDClient(t, errors.New("denied"), testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeScale})
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())

	var jobs batchv1.JobList
	require.NoError(t, k8sClient.List(t.Context(), &jobs, client.InNamespace("ns1")))
	assert.Empty(t, jobs.Items, "a Job that cannot get its token would wait forever and block later triggers")
}

func TestTrigger_RejectsInvalidTarget(t *testing.T) {
	const secret = "s3cret-value"
	tests := []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{name: "path without leading slash", path: "api/orders"},
		{name: "absolute url", path: "http://other.team-b.svc/"},
		{name: "scheme-relative path", path: "//other.team-b.svc/"},
		{name: "path with space", path: "/a b"},
		{name: "path with newline", path: "/a\nb"},
		{name: "path with fragment", path: "/a#b"},
		{name: "path with bad escape", path: "/a%zz"},
		{name: "path too long", path: "/" + strings.Repeat("a", maxTargetPathLen)},
		{name: "invalid header name", headers: map[string]string{"Bad Name": "v"}},
		{name: "empty header name", headers: map[string]string{"": "v"}},
		{name: "header value with newline", headers: map[string]string{"X-A": secret + "\r\nX-B: c"}},
		{name: "host header retargets the request", headers: map[string]string{"Host": "other.team-b.svc"}},
		{name: "host header in lower case", headers: map[string]string{"host": "other.team-b.svc"}},
		{name: "content-length", headers: map[string]string{"Content-Length": "5"}},
		{name: "transfer-encoding", headers: map[string]string{"Transfer-Encoding": "chunked"}},
		{name: "connection", headers: map[string]string{"Connection": "close"}},
		{name: "too many headers", headers: manyHeaders(maxTargetHeaders + 1)},
		{name: "headers too large", headers: map[string]string{"X-A": strings.Repeat("a", maxTargetHeaderBytes)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
			e := clusterEngine(t, k8sClient, testSAToken)

			w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency, Path: tt.path, Headers: tt.headers})

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), secret, "header values must not be echoed")
			var jobs batchv1.JobList
			require.NoError(t, k8sClient.List(t.Context(), &jobs))
			assert.Empty(t, jobs.Items)
		})
	}
}

func manyHeaders(n int) map[string]string {
	headers := make(map[string]string, n)
	for i := range n {
		headers[fmt.Sprintf("X-Header-%d", i)] = "v"
	}
	return headers
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
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Command, "/scripts/run-k6.sh")
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

func TestTrigger_ReturnsRunIDMatchingJob(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	e := clusterEngine(t, k8sClient, testSAToken)

	w := e.PostJSON(benchmarkPath("my-capp"), TriggerRequest{Type: TypeLatency})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp TriggerResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.RunID)

	var job batchv1.Job
	require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "ns1", Name: "bench-my-capp"}, &job))
	assert.Equal(t, resp.RunID, job.Annotations[AnnotationRunID])
	assert.Equal(t, resp.RunID, envMap(job.Spec.Template.Spec.Containers[0].Env)["RUN_ID"])
}

func TestStatus_ReturnsRunID(t *testing.T) {
	job := benchmarkJob("bench-my-capp", "my-capp", batchv1.JobComplete)
	job.Annotations[AnnotationRunID] = "run-42"
	w := engine(t, job).Get(benchmarkPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "run-42", resp.RunID)
}

func TestRevisionLabel(t *testing.T) {
	tests := []struct {
		name   string
		status cappv1alpha1.CappStatus
		want   string
	}{
		{
			name:   "no rollout",
			status: cappv1alpha1.CappStatus{KnativeObjectStatus: knativeStatus("c-00007")}, //nolint:staticcheck // the field is still filled by the operator
			want:   "c-00007",
		},
		{
			name: "aborted rollout serves stable",
			status: cappv1alpha1.CappStatus{Rollout: &cappv1alpha1.RolloutStatus{
				StableRevision: "c-00006", CandidateRevision: "c-00007", Phase: cappv1alpha1.RolloutPhaseAborted,
			}},
			want: "c-00006",
		},
		{
			name: "in-flight rollout serves both",
			status: cappv1alpha1.CappStatus{Rollout: &cappv1alpha1.RolloutStatus{
				StableRevision: "c-00006", CandidateRevision: "c-00007", Phase: cappv1alpha1.RolloutPhaseProgressing,
			}},
			want: "c-00006,c-00007",
		},
		{
			name: "holding rollout serves both",
			status: cappv1alpha1.CappStatus{Rollout: &cappv1alpha1.RolloutStatus{
				StableRevision: "c-00006", CandidateRevision: "c-00007", Phase: cappv1alpha1.RolloutPhaseHolding,
			}},
			want: "c-00006,c-00007",
		},
		{
			name: "promoting rollout serves both",
			status: cappv1alpha1.CappStatus{Rollout: &cappv1alpha1.RolloutStatus{
				StableRevision: "c-00006", CandidateRevision: "c-00007", Phase: cappv1alpha1.RolloutPhasePromoting,
			}},
			want: "c-00006,c-00007",
		},
		{
			name:   "nothing known",
			status: cappv1alpha1.CappStatus{},
			want:   "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capp := &cappv1alpha1.Capp{Status: tt.status}
			assert.Equal(t, tt.want, revisionLabel(capp))
		})
	}
}

func knativeStatus(latestReady string) knativev1.ServiceStatus {
	return knativev1.ServiceStatus{ConfigurationStatusFields: knativev1.ConfigurationStatusFields{
		LatestReadyRevisionName: latestReady,
	}}
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
