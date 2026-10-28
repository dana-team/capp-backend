package benchmarks

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Prometheus names for the Knative serving metrics on this platform.
// OpenTelemetry dots become underscores. kn.service.name and
// k8s.namespace.name are the attributes those metrics carry.
const (
	metricInvocation  = "kn_serving_invocation_duration_seconds"
	metricPodsDesired = "kn_revision_pods_desired"
	metricPodsReady   = "kn_revision_pods_count"
	labelNamespace    = "k8s_namespace_name"
	labelService      = "kn_service_name"
)

// dashboards handles GET .../metrics/dashboards.
func (h *Handler) dashboards(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	cappName := c.Param("name")
	capp := &cappv1alpha1.Capp{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: cappName}, capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	c.JSON(http.StatusOK, DashboardsResponse{Dashboards: dashboardsFor(namespace, cappName)})
}

// dashboardsFor returns the preset charts for one Capp. The expressions are
// meant for query_range. The Capp name is the Knative service name.
func dashboardsFor(namespace, cappName string) []Dashboard {
	sel := fmt.Sprintf(`%s="%s",%s="%s"`,
		labelNamespace, promQLString(namespace),
		labelService, promQLString(cappName),
	)
	latency := metricInvocation + "_bucket{" + sel + "}"
	requests := metricInvocation + "_count{" + sel + "}"
	return []Dashboard{
		{
			ID:          "latency",
			Title:       "Latency",
			Description: "Request duration measured by the queue-proxy, excluding activator wait.",
			Unit:        "seconds",
			Queries: []DashboardQuery{
				{Legend: "p50", Query: histogramQuantile(0.50, latency)},
				{Legend: "p95", Query: histogramQuantile(0.95, latency)},
			},
		},
		{
			ID:          "throughput",
			Title:       "Throughput",
			Description: "Requests per second served by the Capp.",
			Unit:        "requests/s",
			Queries: []DashboardQuery{
				{Legend: "requests", Query: fmt.Sprintf("sum(rate(%s[1m]))", requests)},
			},
		},
		{
			ID:          "autoscaling",
			Title:       "Autoscaling",
			Description: "Pods the autoscaler wants compared with pods that are ready.",
			Unit:        "pods",
			Queries: []DashboardQuery{
				{Legend: "desired", Query: fmt.Sprintf("sum(%s{%s})", metricPodsDesired, sel)},
				{Legend: "ready", Query: fmt.Sprintf("sum(%s{%s})", metricPodsReady, sel)},
			},
		},
	}
}

func histogramQuantile(q float64, bucketSelector string) string {
	return fmt.Sprintf("histogram_quantile(%.2f, sum by (le) (rate(%s[5m])))", q, bucketSelector)
}

func promQLString(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `"`, `\"`)
}
