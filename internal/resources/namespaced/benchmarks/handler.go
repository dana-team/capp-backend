// Package benchmarks implements the benchmark resource handler.
// Trigger creates one Job for a Capp. A Job that is still running blocks
// another trigger. A finished Job is replaced. Status reports that Job.
// Dashboards returns the preset PromQL for a Capp. Query and query_range
// proxy those expressions to the configured query URL.
// Routes are registered only when benchmarks are enabled.
package benchmarks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rolloutInFlightPhases are the phases in which the stable and candidate revisions both take traffic.
var rolloutInFlightPhases = map[cappv1alpha1.RolloutPhase]bool{
	cappv1alpha1.RolloutPhaseProgressing: true,
	cappv1alpha1.RolloutPhaseHolding:     true,
	cappv1alpha1.RolloutPhasePromoting:   true,
}

// Handler implements resources.ResourceHandler for on-demand Capp benchmarks.
type Handler struct {
	cfg      config.BenchmarkConfig
	logger   *zap.Logger
	vmClient *http.Client
}

// New returns a benchmark Handler. The registry skips it when benchmarks are disabled.
func New(cfg config.BenchmarkConfig, logger *zap.Logger) *Handler {
	return &Handler{
		cfg:      cfg,
		logger:   logger,
		vmClient: newVMClient(),
	}
}

// Name returns the handler identifier used in config feature flags.
func (h *Handler) Name() string { return "benchmarks" }

// RegisterRoutes attaches benchmark and PromQL routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	bench := rg.Group("/namespaces/:namespace/capps/:name/benchmark")
	bench.POST("", h.trigger)
	bench.GET("", h.status)

	metrics := rg.Group("/namespaces/:namespace/capps/:name/metrics")
	metrics.GET("/dashboards", h.dashboards)
	metrics.GET("/query", h.query)
	metrics.GET("/query_range", h.queryRange)
}

// trigger handles POST .../benchmark.
func (h *Handler) trigger(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	cappName := c.Param("name")
	ctx := c.Request.Context()

	var req TriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}
	if !req.Type.valid() {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("unknown benchmark type %q", req.Type)))
		return
	}
	if err := req.validate(); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	// Checked before old Jobs are deleted, so a failure keeps the previous result.
	meta, err := clusterMeta(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	var saToken string
	if req.Type == TypeScale {
		if meta.BenchmarkSAToken == "" {
			apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("cluster %q has no benchmark SA token", meta.Name)))
			return
		}
		saToken = meta.BenchmarkSAToken
	}

	existing, err := listBenchmarkJobs(ctx, k8sClient, namespace, cappName)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	for i := range existing {
		if _, finished := jobOutcome(&existing[i]); !finished {
			apierrors.Respond(c, apierrors.NewConflict(
				fmt.Sprintf("benchmark is already running for capp %q", cappName),
			))
			return
		}
	}
	if err := deleteJobs(ctx, k8sClient, existing); err != nil {
		apierrors.Respond(c, err)
		return
	}

	capp := &cappv1alpha1.Capp{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cappName}, capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	var tokenSecretName string
	if saToken != "" {
		tokenSecretName = saTokenSecretName(jobName(cappName))
	}

	runID := newRunID()
	job, err := BuildJob(JobOptions{
		CappName:    cappName,
		CappUID:     capp.UID,
		Cluster:     meta.Name,
		Namespace:   namespace,
		Image:       h.cfg.Image,
		VMURLs:      h.cfg.VMURLs,
		Type:        req.Type,
		Path:        req.Path,
		Headers:     req.Headers,
		TriggeredBy: triggeredBy(c),
		RunID:       runID,
		Revision:    revisionLabel(capp),

		SATokenSecretName: tokenSecretName,
	})
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	if err := k8sClient.Create(ctx, job); err != nil {
		apierrors.Respond(c, err)
		return
	}
	if tokenSecretName != "" {
		if err := k8sClient.Create(ctx, saTokenSecret(job, tokenSecretName, saToken)); err != nil {
			// Without its Secret the pod never starts, so the Job would block later triggers.
			if delErr := deleteJobs(ctx, k8sClient, []batchv1.Job{*job}); delErr != nil {
				h.logger.Error("remove benchmark job after secret failure", zap.String("job", job.Name), zap.Error(delErr))
			}
			apierrors.Respond(c, err)
			return
		}
	}

	h.logger.Info("benchmark triggered",
		zap.String("capp", cappName),
		zap.String("namespace", namespace),
		zap.String("job", job.Name),
		zap.String("type", string(req.Type)),
		zap.String("runID", runID),
	)

	c.JSON(http.StatusCreated, TriggerResponse{
		CappName: cappName,
		JobName:  job.Name,
		Type:     req.Type,
		RunID:    runID,
	})
}

// saTokenSecret returns the Secret for one Job. The Job owns it, so it is removed with the Job.
func saTokenSecret(job *batchv1.Job, name, token string) *corev1.Secret {
	blockOwnerDeletion := false
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: job.Namespace,
			Labels:    map[string]string{LabelBenchmark: labelValueTrue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         batchv1.SchemeGroupVersion.String(),
				Kind:               "Job",
				Name:               job.Name,
				UID:                job.UID,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{saTokenSecretKey: []byte(token)},
	}
}

// status handles GET .../benchmark.
func (h *Handler) status(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	cappName := c.Param("name")

	jobs, err := listBenchmarkJobs(c.Request.Context(), k8sClient, namespace, cappName)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	resp := StatusResponse{}
	if job := currentBenchmark(jobs); job != nil {
		status, _ := jobOutcome(job)
		resp.JobName = job.Name
		resp.Status = status
		resp.RunID = job.Annotations[AnnotationRunID]
	}
	c.JSON(http.StatusOK, resp)
}

// newRunID returns an ID that is unique enough to tell two runs of the same Capp apart.
func newRunID() string {
	return fmt.Sprintf("%d-%s", time.Now().Unix(), utilrand.String(5))
}

// revisionLabel names the revision or revisions that serve traffic during the run.
// While a rollout is in flight, both the stable and the candidate revision take traffic.
func revisionLabel(capp *cappv1alpha1.Capp) string {
	if status := capp.Status.Rollout; status != nil && status.StableRevision != "" {
		if status.CandidateRevision != "" && rolloutInFlightPhases[status.Phase] {
			return status.StableRevision + "," + status.CandidateRevision
		}
		return status.StableRevision
	}
	// The operator still fills this deprecated field. The ServingReady condition does not name a revision.
	if latest := capp.Status.KnativeObjectStatus.LatestReadyRevisionName; latest != "" { //nolint:staticcheck // see above
		return latest
	}
	return "unknown"
}

// listBenchmarkJobs returns benchmark Jobs for one Capp in a namespace.
func listBenchmarkJobs(ctx context.Context, k8sClient client.Client, namespace, cappName string) ([]batchv1.Job, error) {
	var jobList batchv1.JobList
	if err := k8sClient.List(ctx, &jobList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelBenchmark: labelValueTrue},
	); err != nil {
		return nil, err
	}

	filtered := make([]batchv1.Job, 0)
	for i := range jobList.Items {
		if jobList.Items[i].Annotations[AnnotationCappName] == cappName {
			filtered = append(filtered, jobList.Items[i])
		}
	}
	return filtered, nil
}

// currentBenchmark prefers a Job that is still running.
func currentBenchmark(jobs []batchv1.Job) *batchv1.Job {
	for i := range jobs {
		if _, finished := jobOutcome(&jobs[i]); !finished {
			return &jobs[i]
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	return &jobs[0]
}

// jobOutcome reports running, completed, or failed.
// A Job with no Complete or Failed condition is still running, including one whose pods have not started.
func jobOutcome(job *batchv1.Job) (string, bool) {
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return StatusCompleted, true
		case batchv1.JobFailed:
			return StatusFailed, true
		}
	}
	return StatusRunning, false
}

func deleteJobs(ctx context.Context, k8sClient client.Client, jobs []batchv1.Job) error {
	bg := metav1.DeletePropagationBackground
	for i := range jobs {
		if err := k8sClient.Delete(ctx, &jobs[i], &client.DeleteOptions{
			PropagationPolicy: &bg,
		}); err != nil {
			return err
		}
	}
	return nil
}

// clusterMeta returns the requested cluster.
func clusterMeta(c *gin.Context) (cluster.ClusterMeta, error) {
	val, ok := c.Get(string(middleware.ClusterMetaKey))
	if !ok {
		return cluster.ClusterMeta{}, errors.New("ClusterMeta not found in context")
	}
	meta, ok := val.(cluster.ClusterMeta)
	if !ok {
		return cluster.ClusterMeta{}, errors.New("ClusterMeta has unexpected type in context")
	}
	return meta, nil
}

func triggeredBy(c *gin.Context) string {
	val, ok := c.Get(string(middleware.CredentialKey))
	if !ok {
		return ""
	}
	cred, ok := val.(auth.ClusterCredential)
	if !ok {
		return ""
	}
	return cred.ImpersonateUser
}
