package functions

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultLogLines = 100
	maxLogLines     = 1000
	maxLogBytes     = 256 * 1024
	maxLogPods      = 20
)

// logs handles GET /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/logs
//
// It returns the end of the log of each pod that serves the function now.
// Fission takes an idle pod away from the function, and the log goes with
// the pod, so this shows recent runs only. Query: lines (default 100).
func (h *Handler) logs(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	readLog, ok := podLogs(c)
	if !ok {
		apierrors.Respond(c, apierrors.NewNotSupported("function logs in this authorization mode"))
		return
	}

	lines := int64(defaultLogLines)
	if raw := c.Query("lines"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > maxLogLines {
			apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("lines must be between 1 and %d", maxLogLines)))
			return
		}
		lines = parsed
	}

	fn, ok := h.function(c, k8sClient)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(fn.Namespace), client.MatchingLabels{
		fissionv1.FUNCTION_NAME:      fn.Name,
		fissionv1.FUNCTION_NAMESPACE: fn.Namespace,
	}); err != nil {
		apierrors.Respond(c, err)
		return
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	if len(pods.Items) > maxLogPods {
		pods.Items = pods.Items[:maxLogPods]
	}

	resp := FunctionLogsResponse{Pods: make([]FunctionPodLog, 0, len(pods.Items))}
	for _, pod := range pods.Items {
		// The runtime container of a pool pod is named after the pool.
		text, err := readLog(ctx, pod.Namespace, pod.Name, fn.Spec.Environment.Name, lines, maxLogBytes)
		// Fission removes idle pods all the time, so a pod can be gone, or
		// not started yet, between the list and this read. The pod stays in
		// the answer with no lines, so the caller sees that it exists.
		if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsBadRequest(err) {
			apierrors.Respond(c, err)
			return
		}
		entry := FunctionPodLog{Pod: pod.Name, Lines: []string{}}
		if trimmed := strings.TrimRight(text, "\n"); err == nil && trimmed != "" {
			entry.Lines = strings.Split(trimmed, "\n")
		}
		resp.Pods = append(resp.Pods, entry)
	}

	c.JSON(http.StatusOK, resp)
}

func podLogs(c *gin.Context) (cluster.PodLogs, bool) {
	value, exists := c.Get(string(middleware.PodLogsKey))
	if !exists {
		return nil, false
	}
	readLog, ok := value.(cluster.PodLogs)
	return readLog, ok && readLog != nil
}
