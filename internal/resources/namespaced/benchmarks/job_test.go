package benchmarks

import (
	"os"
	"path/filepath"
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
		Cluster:   "c1",
		Namespace: "team-a",
		Image:     "ghcr.io/dana-team/capp-benchmark-runner:0.1.0",
		VMURLs: []string{
			"http://vm-a.monitoring.svc:8428",
			"http://vm-b.monitoring.svc:8428/",
		},
		Type:              typ,
		SATokenSecretName: "bench-my-capp-sa-abcde",
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
				"TARGET_PATH":                 "/",
				"K6_PROMETHEUS_RW_SERVER_URL": "http://vm-a.monitoring.svc:8428/api/v1/write",
			},
			absent: []string{"LATENCY_PATH", "TARGET_HEADERS", "VM_IMPORT_URLS"},
		},
		{
			typ:     TypeThroughput,
			command: []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/throughput.js"},
			extra: map[string]string{
				"TARGET_PATH":                 "/",
				"K6_PROMETHEUS_RW_SERVER_URL": "http://vm-a.monitoring.svc:8428/api/v1/write",
			},
			absent: []string{"LATENCY_PATH", "TARGET_HEADERS", "VM_IMPORT_URLS"},
		},
		{
			typ:     TypeScale,
			command: []string{"bash", "/scripts/scale-test.sh"},
			extra: map[string]string{
				"TARGET_PATH":     "/",
				"TARGET_PODS":     "5",
				"TIMEOUT_SECONDS": "180",
				"LOAD_VUS":        "100",
				"LOAD_DURATION":   "3m",
				"VM_IMPORT_URLS":  "http://vm-a.monitoring.svc:8428,http://vm-b.monitoring.svc:8428/",
			},
			absent: []string{"LATENCY_PATH", "K6_PROMETHEUS_RW_SERVER_URL", "TARGET_HEADERS", "ZERO_WAIT_SECONDS"},
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
			assert.Equal(t, "c1", env["CAPP_CLUSTER"])
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

// Namespaces can have a ResourceQuota on limits, which rejects pods that set none.
func TestBuildJob_SetsResourceLimits(t *testing.T) {
	for _, typ := range []Type{TypeLatency, TypeThroughput, TypeScale} {
		job, err := BuildJob(testOptions(typ))
		require.NoError(t, err)
		resources := job.Spec.Template.Spec.Containers[0].Resources

		assert.Equal(t, "250m", resources.Requests.Cpu().String())
		assert.Equal(t, "256Mi", resources.Requests.Memory().String())
		assert.Equal(t, "1", resources.Limits.Cpu().String())
		assert.Equal(t, "1Gi", resources.Limits.Memory().String())
	}
}

func TestBuildJob_CustomPathAndHeaders(t *testing.T) {
	opts := testOptions(TypeLatency)
	opts.Path = "/api/orders?limit=5"
	opts.Headers = map[string]string{"X-Team": "blue", "Authorization": "Bearer t"}

	job, err := BuildJob(opts)

	require.NoError(t, err)
	env := envMap(job.Spec.Template.Spec.Containers[0].Env)
	assert.Equal(t, "/api/orders?limit=5", env["TARGET_PATH"])
	assert.JSONEq(t, `{"X-Team":"blue","Authorization":"Bearer t"}`, env["TARGET_HEADERS"])
}

// The Job runs scripts from the image, so a command naming a file that the
// image source does not contain fails on every run.
func TestBuildJob_RunnerImageContainsScripts(t *testing.T) {
	const scripts = "../../../../deploy/benchmark-runner/scripts"
	for _, typ := range []Type{TypeLatency, TypeThroughput, TypeScale} {
		job, err := BuildJob(testOptions(typ))
		require.NoError(t, err)
		command := job.Spec.Template.Spec.Containers[0].Command
		script := command[len(command)-1]
		require.True(t, strings.HasPrefix(script, "/scripts/"), script)
		assert.FileExists(t, filepath.Join(scripts, strings.TrimPrefix(script, "/scripts/")))
	}

	assert.FileExists(t, filepath.Join(scripts, "load.js"), "scale-test.sh runs load.js")

	common, err := os.ReadFile(filepath.Join(scripts, "common.js"))
	require.NoError(t, err)
	for _, name := range []string{"TARGET_URL", "TARGET_PATH", "TARGET_HEADERS", "CAPP_NAME", "CAPP_NAMESPACE", "CAPP_CLUSTER"} {
		assert.Contains(t, string(common), "__ENV."+name)
	}
}

func TestBuildJob_SATokenFromSecretOnlyForScale(t *testing.T) {
	findToken := func(t *testing.T, typ Type) *corev1.EnvVar {
		t.Helper()
		job, err := BuildJob(testOptions(typ))
		require.NoError(t, err)
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "BENCHMARK_SA_TOKEN" {
				return &e
			}
		}
		return nil
	}

	token := findToken(t, TypeScale)
	require.NotNil(t, token)
	assert.Empty(t, token.Value, "the token must not be stored in the Job spec")
	require.NotNil(t, token.ValueFrom)
	require.NotNil(t, token.ValueFrom.SecretKeyRef)
	assert.Equal(t, "bench-my-capp-sa-abcde", token.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "token", token.ValueFrom.SecretKeyRef.Key)

	assert.Nil(t, findToken(t, TypeLatency))
	assert.Nil(t, findToken(t, TypeThroughput))
}

func TestBuildJob_ScaleRequiresSATokenSecret(t *testing.T) {
	opts := testOptions(TypeScale)
	opts.SATokenSecretName = ""
	_, err := BuildJob(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SA token")
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
		{name: "missing cluster", mutate: func(o *JobOptions) { o.Cluster = "" }, wantErr: "cluster"},
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
