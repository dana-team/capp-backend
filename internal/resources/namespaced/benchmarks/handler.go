// Package benchmarks implements the benchmark resource handler.
// Trigger creates one Job for a Capp. A Job that is still running blocks
// another trigger. A finished Job is replaced. Status reports that Job.
// Dashboards returns the preset PromQL for a Capp. Query and query_range
// proxy those expressions to the first configured Prometheus URL.
// Routes are registered only when benchmarks are enabled.
package benchmarks

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

	job, err := BuildJob(JobOptions{
		CappName:    cappName,
		CappUID:     capp.UID,
		Namespace:   namespace,
		Image:       h.cfg.Image,
		VMURLs:      h.cfg.VMURLs,
		Type:        req.Type,
		TriggeredBy: triggeredBy(c),
	})
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	if err := k8sClient.Create(ctx, job); err != nil {
		apierrors.Respond(c, err)
		return
	}

	h.logger.Info("benchmark triggered",
		zap.String("capp", cappName),
		zap.String("namespace", namespace),
		zap.String("job", job.Name),
		zap.String("type", string(req.Type)),
	)

	c.JSON(http.StatusCreated, TriggerResponse{
		CappName: cappName,
		JobName:  job.Name,
		Type:     req.Type,
	})
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
	}
	c.JSON(http.StatusOK, resp)
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
