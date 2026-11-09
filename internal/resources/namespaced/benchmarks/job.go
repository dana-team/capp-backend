package benchmarks

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
)

// cappGVK is the Capp type's group, version, and kind from the operator API.
var cappGVK = cappv1alpha1.GroupVersion.WithKind("Capp")

var (
	// LabelBenchmark marks the Job so it can be listed and deleted by selector.
	LabelBenchmark = cappGVK.Group + "/benchmark"
	// AnnotationCappName records which Capp the Job targets.
	// The operator does not export this key; it is the same one it sets on CappRevisions.
	AnnotationCappName = k8s.LabelCappName
	// AnnotationTriggeredBy records the user who requested the benchmark.
	AnnotationTriggeredBy = cappGVK.Group + "/triggered-by"
)

const (
	defaultTargetPath = "/"
	scaleTargetPods   = "5"
	scaleLoadVUs      = "100"
	scaleLoadDuration = "3m"
	scaleTimeout      = "180"
	saTokenEnv        = "BENCHMARK_SA_TOKEN"
	saTokenSecretKey  = "token"
	jobTTLSeconds     = int32(600)
	jobNamePrefix     = "bench-"
	maxJobNameLen     = 63
	// labelValueTrue is the value set on LabelBenchmark.
	labelValueTrue = "true"
)

// JobOptions are the inputs for one benchmark Job.
type JobOptions struct {
	CappName    string
	CappUID     types.UID
	Namespace   string
	Image       string
	VMURLs      []string
	Type        Type
	Path        string
	Headers     map[string]string
	TriggeredBy string
	// SATokenSecretName names the Secret holding the ServiceAccount token.
	// Required for the scale check.
	SATokenSecretName string
}

// BuildJob returns a Job that runs one script baked into the runner image
// (deploy/Dockerfile.benchmark-runner). It does not set a ServiceAccount. The
// scale check imports samples to every VMURLs entry. k6 remote-write accepts a
// single endpoint, so latency and throughput write to the first URL.
func BuildJob(opts JobOptions) (*batchv1.Job, error) {
	if opts.CappName == "" {
		return nil, fmt.Errorf("capp name is required")
	}
	if opts.CappUID == "" {
		return nil, fmt.Errorf("capp UID is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if opts.Image == "" {
		return nil, fmt.Errorf("image is required")
	}
	if len(opts.VMURLs) == 0 {
		return nil, fmt.Errorf("at least one VictoriaMetrics URL is required")
	}
	if !opts.Type.valid() {
		return nil, fmt.Errorf("unknown benchmark type %q", opts.Type)
	}

	command, env, err := commandAndEnv(opts)
	if err != nil {
		return nil, err
	}

	ttl := jobTTLSeconds
	backoff := int32(0)
	controller := false
	blockOwnerDeletion := false
	annotations := map[string]string{
		AnnotationCappName: opts.CappName,
	}
	if opts.TriggeredBy != "" {
		annotations[AnnotationTriggeredBy] = opts.TriggeredBy
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(opts.CappName),
			Namespace: opts.Namespace,
			Labels: map[string]string{
				LabelBenchmark: labelValueTrue,
			},
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         cappGVK.GroupVersion().String(),
				Kind:               cappGVK.Kind,
				Name:               opts.CappName,
				UID:                opts.CappUID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelBenchmark: labelValueTrue,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:            "benchmark",
						Image:           opts.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         command,
						Env:             env,
					}},
				},
			},
		},
	}, nil
}

func commandAndEnv(opts JobOptions) ([]string, []corev1.EnvVar, error) {
	targetPath := opts.Path
	if targetPath == "" {
		targetPath = defaultTargetPath
	}
	env := []corev1.EnvVar{
		{Name: "TARGET_URL", Value: fmt.Sprintf("http://%s.%s.svc.cluster.local", opts.CappName, opts.Namespace)},
		{Name: "TARGET_PATH", Value: targetPath},
		{Name: "CAPP_NAME", Value: opts.CappName},
		{Name: "CAPP_NAMESPACE", Value: opts.Namespace},
	}
	if len(opts.Headers) > 0 {
		headers, err := json.Marshal(opts.Headers)
		if err != nil {
			return nil, nil, fmt.Errorf("encode headers: %w", err)
		}
		env = append(env, corev1.EnvVar{Name: "TARGET_HEADERS", Value: string(headers)})
	}

	remoteWrite := corev1.EnvVar{Name: "K6_PROMETHEUS_RW_SERVER_URL", Value: remoteWriteURL(opts.VMURLs[0])}
	switch opts.Type {
	case TypeLatency:
		return []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/latency.js"}, append(env, remoteWrite), nil
	case TypeThroughput:
		return []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/throughput.js"}, append(env, remoteWrite), nil
	case TypeScale:
		if opts.SATokenSecretName == "" {
			return nil, nil, fmt.Errorf("SA token secret name is required for the scale benchmark")
		}
		env = append(env,
			corev1.EnvVar{Name: saTokenEnv, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: opts.SATokenSecretName},
				Key:                  saTokenSecretKey,
			}}},
			corev1.EnvVar{Name: "VM_IMPORT_URLS", Value: strings.Join(opts.VMURLs, ",")},
			corev1.EnvVar{Name: "TARGET_PODS", Value: scaleTargetPods},
			corev1.EnvVar{Name: "TIMEOUT_SECONDS", Value: scaleTimeout},
			corev1.EnvVar{Name: "LOAD_VUS", Value: scaleLoadVUs},
			corev1.EnvVar{Name: "LOAD_DURATION", Value: scaleLoadDuration},
		)
		return []string{"bash", "/scripts/scale-test.sh"}, env, nil
	default:
		return nil, nil, fmt.Errorf("unknown benchmark type %q", opts.Type)
	}
}

// saTokenSecretName is unique per run: the previous Job's Secret may still be
// waiting for garbage collection when the same Job name is reused.
func saTokenSecretName(job string) string {
	return fmt.Sprintf("%s-sa-%s", job, utilrand.String(5))
}

func remoteWriteURL(base string) string {
	return strings.TrimRight(base, "/") + "/api/v1/write"
}

func jobName(cappName string) string {
	name := jobNamePrefix + cappName
	if len(name) <= maxJobNameLen {
		return name
	}
	return strings.TrimRight(name[:maxJobNameLen], "-.")
}
