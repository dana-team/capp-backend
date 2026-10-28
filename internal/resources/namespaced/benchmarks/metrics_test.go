package benchmarks

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestQuery_ProxiesToFirstVM(t *testing.T) {
	var gotPath string
	var got url.Values
	var gotAuth string
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		got = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(vm.Close)

	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("second VictoriaMetrics URL must not be queried")
	}))
	t.Cleanup(other.Close)

	e := metricsEngine(t, vm.URL, other.URL)
	promql := `rate(k6_http_req_duration{capp="my-capp"}[1m])`
	w := e.Get(metricsPath("my-capp") + "/query?query=" + url.QueryEscape(promql) + "&time=1700000000&extra=nope")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/api/v1/query", gotPath)
	assert.Equal(t, promql, got.Get("query"))
	assert.Equal(t, "1700000000", got.Get("time"))
	assert.Empty(t, got.Get("extra"))
	assert.Equal(t, "Bearer vm-secret", gotAuth)
	assert.JSONEq(t, `{"status":"success","data":{"resultType":"vector","result":[]}}`, w.Body.String())
}

func TestQuery_OmitsAuthorizationWhenKeyUnset(t *testing.T) {
	var gotAuth string
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(vm.Close)

	cfg := testConfig()
	cfg.VMURLs = []string{vm.URL}
	cfg.APIKey = ""
	e := testutil.NewEngineHelper(t, testutil.FakeClient(t, testCapp("my-capp")), New(cfg, zap.NewNop()))
	w := e.Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, gotAuth)
}

func TestQuery_PassesVMClientError(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"bad query"}`))
	}))
	t.Cleanup(vm.Close)

	w := metricsEngine(t, vm.URL).Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "bad_data")
	assert.NotContains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
}

func TestQuery_UpstreamUnavailable(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	vm.Close()

	w := metricsEngine(t, vm.URL).Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
	assert.NotContains(t, w.Body.String(), "127.0.0.1")
}

func TestQuery_Upstream5xx(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "vm internal http://vm.monitoring.svc:8428", http.StatusInternalServerError)
	}))
	t.Cleanup(vm.Close)

	w := metricsEngine(t, vm.URL).Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
	assert.NotContains(t, w.Body.String(), "vm.monitoring.svc")
}

func TestQuery_ResponseTooLarge(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxVMResponseBytes+1)))
	}))
	t.Cleanup(vm.Close)

	w := metricsEngine(t, vm.URL).Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
}

func TestQuery_DoesNotFollowRedirect(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/api/v1/query", http.StatusFound)
	}))
	t.Cleanup(vm.Close)

	w := metricsEngine(t, vm.URL).Get(metricsPath("my-capp") + "/query?query=up")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
	assert.NotContains(t, w.Body.String(), "example.com")
}

func TestQuery_MissingQuery(t *testing.T) {
	w := metricsEngine(t, "http://127.0.0.1:1").Get(metricsPath("my-capp") + "/query")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "query is required")
}

func TestQuery_CappNotFound(t *testing.T) {
	called := false
	vm := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(vm.Close)

	cfg := testConfig()
	cfg.VMURLs = []string{vm.URL}
	e := testutil.NewEngineHelper(t, testutil.FakeClient(t), New(cfg, zap.NewNop()))
	w := e.Get(metricsPath("missing") + "/query?query=up")
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.False(t, called)
}

func TestQueryRange_Proxies(t *testing.T) {
	var got url.Values
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/query_range", r.URL.Path)
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	t.Cleanup(vm.Close)

	path := metricsPath("my-capp") + "/query_range?query=" + url.QueryEscape("up") +
		"&start=1700000000&end=1700000300&step=15s"
	w := metricsEngine(t, vm.URL).Get(path)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "up", got.Get("query"))
	assert.Equal(t, "1700000000", got.Get("start"))
	assert.Equal(t, "1700000300", got.Get("end"))
	assert.Equal(t, "15s", got.Get("step"))
	assert.JSONEq(t, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, w.Body.String())
}

func TestQueryRange_MissingParams(t *testing.T) {
	w := metricsEngine(t, "http://127.0.0.1:1").Get(metricsPath("my-capp") + "/query_range?query=up&start=1")
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "query, start, end, and step are required")
}

func metricsEngine(t *testing.T, vmURLs ...string) *testutil.EngineHelper {
	t.Helper()
	cfg := testConfig()
	cfg.VMURLs = vmURLs
	return testutil.NewEngineHelper(t, testutil.FakeClient(t, testCapp("my-capp")), New(cfg, zap.NewNop()))
}
