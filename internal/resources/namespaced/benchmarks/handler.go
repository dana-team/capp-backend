// Package benchmarks implements the benchmark resource handler.
// It renders the capp-monitoring Helm chart, applies the resulting objects
// to the cluster, and provides status and cleanup endpoints.
package benchmarks

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/helm"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// chartRenderer is the interface the handler needs from the helm package.
// Defined here (consumer side) per Go convention.
type chartRenderer interface {
	Render(ctx context.Context, opts helm.RenderOpts) ([]unstructured.Unstructured, error)
}

// Handler implements resources.ResourceHandler for benchmarks.
type Handler struct {
	renderer chartRenderer
	logger   *zap.Logger
}

// New returns a benchmark Handler. Pass nil for renderer when benchmarks
// are disabled — the handler will not be registered.
func New(renderer chartRenderer, logger *zap.Logger) *Handler {
	return &Handler{renderer: renderer, logger: logger}
}

// Name returns the handler identifier used in config feature flags.
func (h *Handler) Name() string { return "benchmarks" }

// RegisterRoutes attaches benchmark routes to the cluster router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	ns := rg.Group("/namespaces/:namespace/capps/:name/benchmark")
	ns.POST("", h.trigger)
	ns.GET("", h.status)
	ns.DELETE("", h.cleanup)
}

// BenchmarkRequest is the body for POST .../benchmark.
type BenchmarkRequest struct {
	Values map[string]interface{} `json:"values"`
}

// BenchmarkResponse is returned after a benchmark is triggered.
type BenchmarkResponse struct {
	Message  string `json:"message"`
	CappName string `json:"cappName"`
	Objects  int    `json:"objects"`
}

// StatusResponse is returned by GET .../benchmark.
type StatusResponse struct {
	CappName  string `json:"cappName"`
	JobName   string `json:"jobName,omitempty"`
	Active    int32  `json:"active"`
	Succeeded int32  `json:"succeeded"`
	Failed    int32  `json:"failed"`
	Found     bool   `json:"found"`
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

	var req BenchmarkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
		return
	}

	existing, err := listBenchmarkJobs(ctx, k8sClient, namespace, cappName)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	if len(existing) > 0 {
		apierrors.Respond(c, apierrors.NewConflict(
			fmt.Sprintf("benchmark already exists for capp %q; delete it before triggering a new one", cappName),
		))
		return
	}

	releaseName := "bench-" + cappName

	objects, err := h.renderer.Render(ctx, helm.RenderOpts{
		ReleaseName: releaseName,
		Namespace:   namespace,
		Values:      req.Values,
	})
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(fmt.Errorf("render chart: %w", err)))
		return
	}

	helm.SetBenchmarkMeta(objects, cappName)

	for i := range objects {
		if err := k8sClient.Create(ctx, &objects[i]); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	h.logger.Info("benchmark triggered",
		zap.String("capp", cappName),
		zap.String("namespace", namespace),
		zap.Int("objects", len(objects)),
	)

	c.JSON(http.StatusCreated, BenchmarkResponse{
		Message:  "benchmark triggered",
		CappName: cappName,
		Objects:  len(objects),
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

	resp := StatusResponse{CappName: cappName}
	if len(jobs) > 0 {
		resp.Found = true
		resp.JobName = jobs[0].Name
		resp.Active = jobs[0].Status.Active
		resp.Succeeded = jobs[0].Status.Succeeded
		resp.Failed = jobs[0].Status.Failed
	}

	c.JSON(http.StatusOK, resp)
}

// cleanup handles DELETE .../benchmark.
func (h *Handler) cleanup(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	cappName := c.Param("name")
	ctx := c.Request.Context()

	jobs, err := listBenchmarkJobs(ctx, k8sClient, namespace, cappName)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	bg := metav1.DeletePropagationBackground
	for i := range jobs {
		if err := k8sClient.Delete(ctx, &jobs[i], &client.DeleteOptions{
			PropagationPolicy: &bg,
		}); err != nil {
			apierrors.Respond(c, err)
			return
		}
	}

	h.logger.Info("benchmark cleaned up",
		zap.String("capp", cappName),
		zap.String("namespace", namespace),
		zap.Int("deleted", len(jobs)),
	)

	c.Status(http.StatusNoContent)
}

// listBenchmarkJobs returns all benchmark Jobs for a given capp in a namespace.
func listBenchmarkJobs(ctx context.Context, k8sClient client.Client, namespace, cappName string) ([]batchv1.Job, error) {
	var jobList batchv1.JobList
	if err := k8sClient.List(ctx, &jobList,
		client.InNamespace(namespace),
		client.MatchingLabels{helm.LabelBenchmark: "true"},
	); err != nil {
		return nil, err
	}

	var filtered []batchv1.Job
	for i := range jobList.Items {
		if jobList.Items[i].Annotations[helm.AnnotationCappName] == cappName {
			filtered = append(filtered, jobList.Items[i])
		}
	}
	return filtered, nil
}
