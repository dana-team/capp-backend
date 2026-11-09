package benchmarks

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboards_DifferByCluster(t *testing.T) {
	east := dashboardsFor("east", "ns1", "my-capp")
	west := dashboardsFor("west", "ns1", "my-capp")
	for i := range east {
		for j := range east[i].Queries {
			assert.Contains(t, east[i].Queries[j].Query, `cluster="east"`)
			assert.Contains(t, west[i].Queries[j].Query, `cluster="west"`)
		}
	}
}

// Without the cluster the charts would merge every cluster's data.
func TestDashboards_FailWithoutClusterMeta(t *testing.T) {
	k8sClient := testutil.FakeClient(t, testCapp("my-capp"))
	w := testutil.NewEngineHelper(t, k8sClient, newHandler()).Get(metricsPath("my-capp") + "/dashboards")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestDashboards_ForCapp(t *testing.T) {
	w := engine(t, testCapp("my-capp")).Get(metricsPath("my-capp") + "/dashboards")
	require.Equal(t, http.StatusOK, w.Code)

	var resp DashboardsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Dashboards, 3)

	byID := map[string]Dashboard{}
	for _, d := range resp.Dashboards {
		byID[d.ID] = d
	}

	sel := `cluster="c1",k8s_namespace_name="ns1",kn_service_name="my-capp"`
	latency := "kn_serving_invocation_duration_seconds_bucket{" + sel + "}"
	assert.Equal(t, Dashboard{
		ID:          "latency",
		Title:       "Latency",
		Description: "Request duration measured by the queue-proxy, excluding activator wait.",
		Unit:        "seconds",
		Queries: []DashboardQuery{
			{Legend: "p50", Query: "histogram_quantile(0.50, sum by (le) (rate(" + latency + "[5m])))"},
			{Legend: "p95", Query: "histogram_quantile(0.95, sum by (le) (rate(" + latency + "[5m])))"},
		},
	}, byID["latency"])

	assert.Equal(t, "requests/s", byID["throughput"].Unit)
	assert.Equal(t, []DashboardQuery{{
		Legend: "requests",
		Query:  "sum(rate(kn_serving_invocation_duration_seconds_count{" + sel + "}[1m]))",
	}}, byID["throughput"].Queries)

	assert.Equal(t, []DashboardQuery{
		{Legend: "desired", Query: "sum(kn_revision_pods_desired{" + sel + "})"},
		{Legend: "ready", Query: "sum(kn_revision_pods_count{" + sel + "})"},
	}, byID["autoscaling"].Queries)
}

func TestDashboards_CappNotFound(t *testing.T) {
	w := engine(t).Get(metricsPath("missing") + "/dashboards")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDashboardsFor_EscapesLabelValues(t *testing.T) {
	q := dashboardsFor(`c"1`, `ns"1`, `my\capp`)[0].Queries[0].Query
	assert.Contains(t, q, `cluster="c\"1"`)
	assert.Contains(t, q, `k8s_namespace_name="ns\"1"`)
	assert.Contains(t, q, `kn_service_name="my\\capp"`)
}
