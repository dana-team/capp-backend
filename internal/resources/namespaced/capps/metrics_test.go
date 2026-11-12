package capps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	metricsBase      = "/clusters/prod/namespaces/team-a/capps/web/metrics"
	vectorResponse   = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"capp":"web"},"value":[1700000000,"0.5"]}]}}`
	matrixResponse   = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"capp":"web"},"values":[[1700000000,"1"],[1700000060,"2"]]}]}}`
	cpuQueryTemplate = `sum(rate(container_cpu_usage_seconds_total{$selector}[5m]))`
)

// fakePrometheus records the form values of each request it receives.
type fakePrometheus struct {
	*httptest.Server
	mu       sync.Mutex
	requests []url.Values
	paths    []string
}

func newFakePrometheus(t *testing.T, handler http.HandlerFunc) *fakePrometheus {
	t.Helper()
	fp := &fakePrometheus{}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fp.mu.Lock()
		fp.requests = append(fp.requests, r.Form)
		fp.paths = append(fp.paths, r.URL.Path)
		fp.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(fp.Close)
	return fp
}

func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (fp *fakePrometheus) calls() int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return len(fp.requests)
}

func (fp *fakePrometheus) last(t *testing.T) (string, url.Values) {
	t.Helper()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	require.NotEmpty(t, fp.requests, "prometheus was not called")
	return fp.paths[len(fp.paths)-1], fp.requests[len(fp.requests)-1]
}

func observabilityConfig(queryURL string) config.ObservabilityConfig {
	return config.ObservabilityConfig{
		Enabled:  true,
		QueryURL: queryURL,
		Labels:   map[string]string{"env": "prod", "app_version": "1.2.3"},
		Metrics: []config.MetricsQuery{
			{
				Name:           "cpu",
				Description:    "CPU usage",
				Query:          cpuQueryTemplate,
				Type:           "gauge",
				ClusterLabel:   "cluster",
				NamespaceLabel: "namespace",
				ObjectLabel:    "capp",
			},
			{
				Name: "latency",
				Type: "histogram",
				Unit: "seconds",
				Series: []config.MetricSeries{
					{Legend: "p50", Query: `histogram_quantile(0.5, rate(lat_bucket{$selector}[5m]))`},
					{Legend: "p95", Query: `histogram_quantile(0.95, rate(lat_bucket{$selector}[5m]))`},
				},
				NamespaceLabel: "namespace",
				ObjectLabel:    "capp",
			},
			{
				Name:           "requests",
				Query:          `sum(rate(http_requests_total{$selector}[1m]))`,
				Type:           "counter",
				NamespaceLabel: "ns",
				ObjectLabel:    "service",
			},
		},
	}
}

// metricsEngine mounts the handler under /clusters/:cluster like the real
// router, so the cluster name reaches the query.
func metricsEngine(t *testing.T, obs config.ObservabilityConfig, k8sClient client.Client) *gin.Engine {
	t.Helper()
	_, e := gin.CreateTestContext(httptest.NewRecorder())
	e.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), k8sClient)
		c.Next()
	})
	h, err := New(false, nil, nil, makeSizes(), obs)
	require.NoError(t, err)
	h.RegisterRoutes(e.Group("/clusters/:cluster"))
	return e
}

func getMetrics(e *gin.Engine, path string) *httptest.ResponseRecorder {
	return testutil.ServeHTTP(e, http.MethodGet, path, nil)
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error apierrors.APIError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body.Error.Code
}

func TestMetrics_Disabled(t *testing.T) {
	e := metricsEngine(t, config.ObservabilityConfig{}, testutil.FakeClient(t, makeCapp("web", "team-a")))

	for _, path := range []string{metricsBase, metricsBase + "/cpu"} {
		w := getMetrics(e, path)
		assert.Equal(t, http.StatusNotImplemented, w.Code, path)
	}
}

func TestListMetrics(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp MetricListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []MetricInfo{
		{Name: "cpu", Description: "CPU usage", Type: "gauge", Legends: []string{"cpu"}},
		{Name: "latency", Type: "histogram", Unit: "seconds", Legends: []string{"p50", "p95"}},
		{Name: "requests", Type: "counter", Legends: []string{"requests"}},
	}, resp.Items)
	assert.Zero(t, fp.calls())
}

func TestMetrics_CappNotFound(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t))

	for _, path := range []string{metricsBase, metricsBase + "/cpu"} {
		w := getMetrics(e, path)
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
	assert.Zero(t, fp.calls())
}

func TestMetrics_CappForbidden(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return k8serrors.NewForbidden(schema.GroupResource{Group: "rcs.dana.io", Resource: "capps"}, "web", errors.New("denied"))
		},
	})
	e := metricsEngine(t, observabilityConfig(fp.URL), k8sClient)

	for _, path := range []string{metricsBase, metricsBase + "/cpu"} {
		w := getMetrics(e, path)
		assert.Equal(t, http.StatusForbidden, w.Code, path)
	}
	assert.Zero(t, fp.calls(), "prometheus must not be queried for a Capp the caller cannot read")
}

func TestQueryMetric_UnknownMetric(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/memory")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Zero(t, fp.calls())
}

func TestQueryMetric_Instant(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/cpu")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	path, form := fp.last(t)
	assert.Equal(t, "/api/v1/query", path)
	assert.Equal(t,
		`sum(rate(container_cpu_usage_seconds_total{namespace="team-a",capp="web",cluster="prod",app_version="1.2.3",env="prod"}[5m]))`,
		form.Get("query"))
	assert.NotEmpty(t, form.Get("time"))

	resp := decodeQueryResponse(t, w)
	assert.Equal(t, "cpu", resp.Name)
	assert.Equal(t, "gauge", resp.Type)
	assert.Equal(t, []string{"cpu"}, resp.Legends)
	require.Len(t, resp.Series, 1)
	assert.Equal(t, "cpu", resp.Series[0].Legend)
	assert.Equal(t, "vector", resp.Series[0].ResultType)
	require.Len(t, resp.Series[0].Result, 1)
	assert.JSONEq(t, `{"metric":{"capp":"web"},"value":[1700000000,"0.5"]}`, string(resp.Series[0].Result[0]))
}

type queryResponse struct {
	MetricInfo
	Series []struct {
		Legend     string            `json:"legend"`
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"series"`
	Warnings []string `json:"warnings"`
}

func decodeQueryResponse(t *testing.T, w *httptest.ResponseRecorder) queryResponse {
	t.Helper()
	var resp queryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

// seriesPrometheus answers a p95 query with value "95" and any other with
// "50", so a test can tell which response landed in which series. Queries
// containing fail get a Prometheus error.
func seriesPrometheus(t *testing.T, fail string) *fakePrometheus {
	t.Helper()
	return newFakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		if fail != "" && strings.Contains(q, fail) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"execution","error":"boom"}`))
			return
		}
		value := "50"
		if strings.Contains(q, "0.95") {
			value = "95"
			// Answer p95 later than p50 so order cannot come from timing.
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","warnings":["w-%s"],"data":{"resultType":"vector","result":[{"metric":{},"value":[1,%q]}]}}`, value, value)
	})
}

func TestQueryMetric_Series(t *testing.T) {
	fp := seriesPrometheus(t, "")
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/latency")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeQueryResponse(t, w)
	assert.Equal(t, "seconds", resp.Unit)
	assert.Equal(t, []string{"p50", "p95"}, resp.Legends)
	require.Len(t, resp.Series, 2)
	assert.Equal(t, "p50", resp.Series[0].Legend)
	assert.JSONEq(t, `{"metric":{},"value":[1,"50"]}`, string(resp.Series[0].Result[0]))
	assert.Equal(t, "p95", resp.Series[1].Legend)
	assert.JSONEq(t, `{"metric":{},"value":[1,"95"]}`, string(resp.Series[1].Result[0]))
	assert.ElementsMatch(t, []string{"w-50", "w-95"}, resp.Warnings)

	fp.mu.Lock()
	defer fp.mu.Unlock()
	require.Len(t, fp.requests, 2)
	queries := make([]string, 0, len(fp.requests))
	times := make([]string, 0, len(fp.requests))
	for _, form := range fp.requests {
		queries = append(queries, form.Get("query"))
		times = append(times, form.Get("time"))
	}
	assert.ElementsMatch(t, []string{
		`histogram_quantile(0.5, rate(lat_bucket{namespace="team-a",capp="web",app_version="1.2.3",env="prod"}[5m]))`,
		`histogram_quantile(0.95, rate(lat_bucket{namespace="team-a",capp="web",app_version="1.2.3",env="prod"}[5m]))`,
	}, queries)
	assert.Equal(t, times[0], times[1], "every series must be evaluated at the same instant")
}

func TestQueryMetric_SeriesRange(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(matrixResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/latency?start=1000&end=2000&step=60")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp := decodeQueryResponse(t, w)
	require.Len(t, resp.Series, 2)
	for _, s := range resp.Series {
		assert.Equal(t, "matrix", s.ResultType)
	}
	assert.Equal(t, 2, fp.calls())
}

func TestQueryMetric_SeriesFailureFailsRequest(t *testing.T) {
	fp := seriesPrometheus(t, "0.95")
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/latency")

	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Equal(t, apierrors.CodePrometheusQueryFailed, errorCode(t, w))
}

// TestQueryMetric_ClientDisconnectSkipsErrorResponse checks that a canceled
// client request context (navigating away, a closed connection) is not
// reported as a Prometheus failure: there is no client left to answer, and
// treating it as PROMETHEUS_QUERY_FAILED would fill logs with false alarms
// every time a dashboard's caller moves on before a slow query returns.
func TestQueryMetric_ClientDisconnectSkipsErrorResponse(t *testing.T) {
	release := make(chan struct{})
	fp := newFakePrometheus(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, metricsBase+"/cpu", nil).WithContext(ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	e.ServeHTTP(w, req)

	assert.Empty(t, w.Body.String(), "no error body should be written once the client has disconnected")
}

func TestQueryMetric_EmptySuccessIsFailure(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(`{"status":"success","data":{}}`))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/cpu")

	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
}

func TestQueryMetric_NoClusterLabel(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	obs := observabilityConfig(fp.URL)
	obs.Labels = nil
	e := metricsEngine(t, obs, testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/requests")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, form := fp.last(t)
	assert.Equal(t, `sum(rate(http_requests_total{ns="team-a",service="web"}[1m]))`, form.Get("query"))
}

func TestQueryMetric_Range(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(matrixResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	end := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC).Unix()
	w := getMetrics(e, metricsBase+fmt.Sprintf("/cpu?start=2026-11-10T10:00:00Z&end=%d&step=1m", end))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	path, form := fp.last(t)
	assert.Equal(t, "/api/v1/query_range", path)
	assert.Equal(t, fmt.Sprint(time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC).Unix()), form.Get("start"))
	assert.Equal(t, fmt.Sprint(end), form.Get("end"))
	assert.Equal(t, "60", form.Get("step"))

	resp := decodeQueryResponse(t, w)
	require.Len(t, resp.Series, 1)
	assert.Equal(t, "matrix", resp.Series[0].ResultType)
}

// TestQueryMetric_RangeEndEqualsStart checks that end == start is accepted,
// matching Prometheus' own query_range endpoint (which returns a single
// point for it), rather than rejected as an invalid range.
func TestQueryMetric_RangeEndEqualsStart(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(matrixResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/cpu?start=1000&end=1000&step=60")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestQueryMetric_StepAcceptsDayUnit checks that step accepts the "d"/"w"/"y"
// units Prometheus' own query_range endpoint accepts, which time.ParseDuration
// alone does not support.
func TestQueryMetric_StepAcceptsDayUnit(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(matrixResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/cpu?start=0&end=172800&step=1d")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, form := fp.last(t)
	assert.Equal(t, "86400", form.Get("step"))
}

func TestQueryMetric_InvalidRange(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(matrixResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	tests := map[string]string{
		"only start":      "start=1000",
		"missing step":    "start=1000&end=2000",
		"bad start":       "start=yesterday&end=2000&step=60",
		"bad end":         "start=1000&end=tomorrow&step=60",
		"bad step":        "start=1000&end=2000&step=often",
		"zero step":       "start=1000&end=2000&step=0",
		"negative step":   "start=1000&end=2000&step=-1s",
		"end before":      "start=2000&end=1000&step=60",
		"too many points": "start=0&end=100000&step=1",
	}
	for name, query := range tests {
		t.Run(name, func(t *testing.T) {
			w := getMetrics(e, metricsBase+"/cpu?"+query)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
	assert.Zero(t, fp.calls())
}

func TestQueryMetric_KeepsQueryURLParameters(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	e := metricsEngine(t, observabilityConfig(fp.URL+"/select/0/prometheus?apikey=secret"), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/cpu")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	path, form := fp.last(t)
	assert.Equal(t, "/select/0/prometheus/api/v1/query", path)
	assert.Equal(t, "secret", form.Get("apikey"))
}

func TestQueryMetric_PrometheusFailures(t *testing.T) {
	redirectTarget := newFakePrometheus(t, respondJSON(vectorResponse))

	tests := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"unavailable","error":"down"}`))
		},
		"bad query": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>login</html>"))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectTarget.URL+r.URL.Path, http.StatusFound)
		},
		"body too large": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"x":"`))
			_, _ = w.Write([]byte(strings.Repeat("a", maxMetricsResponseBytes)))
			_, _ = w.Write([]byte(`"},"value":[1,"1"]}]}}`))
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			fp := newFakePrometheus(t, handler)
			e := metricsEngine(t, observabilityConfig(fp.URL+"?apikey=secret"), testutil.FakeClient(t, makeCapp("web", "team-a")))

			w := getMetrics(e, metricsBase+"/cpu")

			assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			assert.Equal(t, apierrors.CodePrometheusQueryFailed, errorCode(t, w))
			assert.NotContains(t, w.Body.String(), "secret")
		})
	}
	assert.Zero(t, redirectTarget.calls(), "redirects must not be followed")
}

// TestQueryMetric_SharesByteBudgetAcrossSeries checks that the response-size
// cap is a total budget for one request, not a per-series allowance: two
// series each under the per-series cap, but together over it, must still
// fail the request. Without a shared budget, a request with MaxMetricSeries
// concurrent series could buffer many times maxMetricsResponseBytes.
func TestQueryMetric_SharesByteBudgetAcrossSeries(t *testing.T) {
	const perSeries = maxMetricsResponseBytes/2 + 1024 // under the cap alone, over it combined
	fp := newFakePrometheus(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"x":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", perSeries)))
		_, _ = w.Write([]byte(`"},"value":[1,"1"]}]}}`))
	})
	e := metricsEngine(t, observabilityConfig(fp.URL), testutil.FakeClient(t, makeCapp("web", "team-a")))

	w := getMetrics(e, metricsBase+"/latency")

	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Equal(t, apierrors.CodePrometheusQueryFailed, errorCode(t, w))
}

func TestQueryMetric_UnreachableErrorDropsURL(t *testing.T) {
	fp := newFakePrometheus(t, respondJSON(vectorResponse))
	queryURL := fp.URL + "?apikey=secret"
	fp.Close()

	var logged []error
	_, e := gin.CreateTestContext(httptest.NewRecorder())
	e.Use(func(c *gin.Context) {
		c.Set(string(middleware.K8sClientKey), testutil.FakeClient(t, makeCapp("web", "team-a")))
		c.Next()
		for _, ginErr := range c.Errors {
			logged = append(logged, ginErr.Err)
		}
	})
	h, err := New(false, nil, nil, makeSizes(), observabilityConfig(queryURL))
	require.NoError(t, err)
	h.RegisterRoutes(e.Group("/clusters/:cluster"))

	w := getMetrics(e, metricsBase+"/cpu")

	assert.Equal(t, http.StatusBadGateway, w.Code)
	require.NotEmpty(t, logged)
	for _, err := range logged {
		var apiErr *apierrors.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.NotContains(t, fmt.Sprint(errors.Unwrap(apiErr)), "secret")
	}
}

func TestRenderQuery_EscapesValues(t *testing.T) {
	m := config.MetricsQuery{
		NamespaceLabel: "namespace",
		ObjectLabel:    "capp",
	}

	got := renderQuery(m, `a{$selector} / b{$selector}`, map[string]string{"env": `x"} or vector(1) #`}, "", "team-a", `we"b\`)

	assert.Equal(t,
		`a{namespace="team-a",capp="we\"b\\",env="x\"} or vector(1) #"} / b{namespace="team-a",capp="we\"b\\",env="x\"} or vector(1) #"}`,
		got)
}

func TestWithoutURL(t *testing.T) {
	inner := errors.New("connection refused")
	err := &url.Error{Op: "Post", URL: "http://vm?apikey=secret", Err: inner}

	assert.Equal(t, inner, withoutURL(fmt.Errorf("wrapped: %w", err)))
	assert.Equal(t, inner, withoutURL(inner))
}
