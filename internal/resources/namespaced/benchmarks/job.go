package benchmarks

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// LabelBenchmark marks the Job so it can be listed and deleted by selector.
	LabelBenchmark = "rcs.dana.io/benchmark"
	// AnnotationCappName records which Capp the Job targets.
	// Same key as pkg/k8s.LabelCappName.
	AnnotationCappName = "rcs.dana.io/cappName"
	// AnnotationTriggeredBy records the user who requested the benchmark.
	AnnotationTriggeredBy = "rcs.dana.io/triggered-by"
	cappAPIVersion        = "rcs.dana.io/v1alpha1"
	cappKind              = "Capp"

	latencyPath       = "/delay/0.05"
	scaleTargetPods   = "5"
	scaleLoadVUs      = "100"
	scaleLoadDuration = "3m"
	scaleTimeout      = "180"
	scaleLoadPath     = "/delay/0.5"
	jobTTLSeconds     = int32(600)
	jobNamePrefix     = "bench-"
	maxJobNameLen     = 63
)

// JobOptions are the inputs for one benchmark Job.
type JobOptions struct {
	CappName    string
	CappUID     types.UID
	Namespace   string
	Image       string
	VMURLs      []string
	Type        Type
	TriggeredBy string
}

// BuildJob returns a Job that runs one baked-in check. It does not set a
// ServiceAccount. Samples are imported to every VMURLs entry. k6 remote-write
// accepts a single endpoint, so latency and throughput write to the first URL.
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
				LabelBenchmark: "true",
			},
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         cappAPIVersion,
				Kind:               cappKind,
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
						LabelBenchmark: "true",
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
	env := []corev1.EnvVar{
		{Name: "TARGET_URL", Value: fmt.Sprintf("http://%s.%s.svc.cluster.local", opts.CappName, opts.Namespace)},
		{Name: "CAPP_NAME", Value: opts.CappName},
		{Name: "CAPP_NAMESPACE", Value: opts.Namespace},
		{Name: "VM_IMPORT_URLS", Value: strings.Join(opts.VMURLs, ",")},
	}

	switch opts.Type {
	case TypeLatency:
		env = append(env,
			corev1.EnvVar{Name: "LATENCY_PATH", Value: latencyPath},
			corev1.EnvVar{Name: "K6_PROMETHEUS_RW_SERVER_URL", Value: remoteWriteURL(opts.VMURLs[0])},
		)
		return []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/latency.js"}, env, nil
	case TypeThroughput:
		env = append(env,
			corev1.EnvVar{Name: "K6_PROMETHEUS_RW_SERVER_URL", Value: remoteWriteURL(opts.VMURLs[0])},
		)
		return []string{"k6", "run", "--out", "experimental-prometheus-rw", "/scripts/throughput.js"}, env, nil
	case TypeScale:
		env = append(env,
			corev1.EnvVar{Name: "TARGET_PODS", Value: scaleTargetPods},
			corev1.EnvVar{Name: "TIMEOUT_SECONDS", Value: scaleTimeout},
			corev1.EnvVar{Name: "LOAD_VUS", Value: scaleLoadVUs},
			corev1.EnvVar{Name: "LOAD_DURATION", Value: scaleLoadDuration},
			corev1.EnvVar{Name: "LOAD_PATH", Value: scaleLoadPath},
		)
		return []string{"bash", "/scripts/scale-test.sh"}, env, nil
	default:
		return nil, nil, fmt.Errorf("unknown benchmark type %q", opts.Type)
	}
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
