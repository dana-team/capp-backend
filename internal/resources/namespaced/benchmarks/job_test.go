package benchmarks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testOptions(typ Type) JobOptions {
	return JobOptions{
		CappName:  "my-capp",
		CappUID:   types.UID("uid-1"),
		Namespace: "team-a",
		Image:     "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
		VMURLs: []string{
			"http://vm-a.monitoring.svc:8428",
			"http://vm-b.monitoring.svc:8428/",
		},
		Type: typ,
	}
}

func TestBuildJob(t *testing.T) {
	tests := []struct {
		typ     Type
		command []string
		extra   map[string]string
		absent  []string
	}{
		{
			typ:     TypeLatency,
			command: []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/latency.js"},
			extra: map[string]string{
				"LATENCY_PATH":                "/delay/0.05",
				"K6_PROMETHEUS_RW_SERVER_URL": "http://vm-a.monitoring.svc:8428/api/v1/write",
			},
			absent: []string{"TARGET_PODS", "LOAD_PATH"},
		},
		{
			typ:     TypeThroughput,
			command: []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/throughput.js"},
			extra: map[string]string{
				"K6_PROMETHEUS_RW_SERVER_URL": "http://vm-a.monitoring.svc:8428/api/v1/write",
			},
			absent: []string{"LATENCY_PATH", "TARGET_PODS", "LOAD_PATH"},
		},
		{
			typ:     TypeScale,
			command: []string{"bash", "/scripts/scale-test.sh"},
			extra: map[string]string{
				"TARGET_PODS":     "5",
				"TIMEOUT_SECONDS": "180",
				"LOAD_VUS":        "100",
				"LOAD_DURATION":   "3m",
				"LOAD_PATH":       "/delay/0.5",
			},
			absent: []string{"LATENCY_PATH", "K6_PROMETHEUS_RW_SERVER_URL", "ZERO_WAIT_SECONDS"},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			job, err := BuildJob(testOptions(tt.typ))
			require.NoError(t, err)

			assert.Equal(t, "bench-my-capp", job.Name)
			assert.Equal(t, "team-a", job.Namespace)
			assert.Equal(t, "rcs.dana.io/benchmark", LabelBenchmark)
			assert.Equal(t, "true", job.Labels[LabelBenchmark])
			assert.Equal(t, "rcs.dana.io/cappName", AnnotationCappName)
			assert.Equal(t, "my-capp", job.Annotations[AnnotationCappName])
			_, hasUser := job.Annotations[AnnotationTriggeredBy]
			assert.False(t, hasUser)
			require.Len(t, job.OwnerReferences, 1)
			owner := job.OwnerReferences[0]
			assert.Equal(t, "rcs.dana.io/v1alpha1", owner.APIVersion)
			assert.Equal(t, "Capp", owner.Kind)
			assert.Equal(t, "my-capp", owner.Name)
			assert.Equal(t, types.UID("uid-1"), owner.UID)
			require.NotNil(t, owner.BlockOwnerDeletion)
			assert.False(t, *owner.BlockOwnerDeletion)
			require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
			assert.Equal(t, int32(600), *job.Spec.TTLSecondsAfterFinished)
			require.NotNil(t, job.Spec.BackoffLimit)
			assert.Equal(t, int32(0), *job.Spec.BackoffLimit)

			pod := job.Spec.Template
			assert.Equal(t, "true", pod.Labels[LabelBenchmark])
			assert.Empty(t, pod.Spec.ServiceAccountName)
			assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
			require.Len(t, pod.Spec.Containers, 1)

			container := pod.Spec.Containers[0]
			assert.Equal(t, "ghcr.io/dana-team/capp-benchmark-runner:0.1.0", container.Image)
			assert.Equal(t, tt.command, container.Command)
			assert.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy)

			env := envMap(container.Env)
			assert.Equal(t, "http://my-capp.team-a.svc.cluster.local", env["TARGET_URL"])
			assert.Equal(t, "my-capp", env["CAPP_NAME"])
			assert.Equal(t, "team-a", env["CAPP_NAMESPACE"])
			assert.Equal(t, "http://vm-a.monitoring.svc:8428,http://vm-b.monitoring.svc:8428/", env["VM_IMPORT_URLS"])
			for k, v := range tt.extra {
				assert.Equal(t, v, env[k], k)
			}
			for _, k := range tt.absent {
				_, ok := env[k]
				assert.False(t, ok, "env %s should be absent", k)
			}
		})
	}
}

func TestBuildJob_Errors(t *testing.T) {
	base := testOptions(TypeLatency)
	tests := []struct {
		name    string
		mutate  func(*JobOptions)
		wantErr string
	}{
		{name: "missing capp", mutate: func(o *JobOptions) { o.CappName = "" }, wantErr: "capp name"},
		{name: "missing capp uid", mutate: func(o *JobOptions) { o.CappUID = "" }, wantErr: "capp UID"},
		{name: "missing namespace", mutate: func(o *JobOptions) { o.Namespace = "" }, wantErr: "namespace"},
		{name: "missing image", mutate: func(o *JobOptions) { o.Image = "" }, wantErr: "image"},
		{name: "missing vm urls", mutate: func(o *JobOptions) { o.VMURLs = nil }, wantErr: "VictoriaMetrics"},
		{name: "unknown type", mutate: func(o *JobOptions) { o.Type = "cold-start" }, wantErr: "unknown benchmark type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := base
			tt.mutate(&opts)
			_, err := BuildJob(opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestBuildJob_TriggeredBy(t *testing.T) {
	opts := testOptions(TypeLatency)
	opts.TriggeredBy = "alice"
	job, err := BuildJob(opts)
	require.NoError(t, err)
	assert.Equal(t, "alice", job.Annotations[AnnotationTriggeredBy])
}

func TestJobName_TruncatesToDNSLabel(t *testing.T) {
	opts := testOptions(TypeScale)
	opts.CappName = strings.Repeat("a", 80)
	job, err := BuildJob(opts)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(job.Name), 63)
	assert.True(t, strings.HasPrefix(job.Name, "bench-"))
	assert.NotEqual(t, "-", job.Name[len(job.Name)-1:])
}

func envMap(env []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		out[e.Name] = e.Value
	}
	return out
}
