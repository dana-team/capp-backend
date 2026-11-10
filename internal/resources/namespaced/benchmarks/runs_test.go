package benchmarks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vmSample is one series in a fake VictoriaMetrics instant response.
type vmSample struct {
	labels map[string]string
	value  string
}

// vmSeries returns the series a runner writes for one run, one per stat. Every
// run has start_seconds and end_seconds, 30 seconds apart.
func vmSeries(runID, typ, path, result string, started int64, stats map[string]float64) []vmSample {
	values := map[string]float64{
		"start_seconds": float64(started),
		"end_seconds":   float64(started + 30),
	}
	for stat, v := range stats {
		values[stat] = v
	}
	samples := make([]vmSample, 0, len(values))
	for stat, v := range values {
		samples = append(samples, vmSample{
			labels: map[string]string{
				"__name__":  "capp_benchmark_result",
				"run_id":    runID,
				"capp":      "my-capp",
				"namespace": "ns1",
				"cluster":   "east",
				"type":      typ,
				"path":      path,
				"revision":  "c-00007",
				"result":    result,
				"stat":      stat,
			},
			value: strconv.FormatFloat(v, 'f', -1, 64),
		})
	}
	return samples
}

// fakeVM stands in for VictoriaMetrics. Instant queries get samples, range
// queries get points, and every query it receives is recorded.
type fakeVM struct {
	url     string
	samples []vmSample
	points  [][2]any // the one series for query_range; no series when nil
	status  int      // when set, every request fails with this status

	queries    []string
	rangeCalls []url.Values
}

func newFakeVM(t *testing.T) *fakeVM {
	t.Helper()
	f := &fakeVM{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeVM) serve(w http.ResponseWriter, r *http.Request) {
	f.queries = append(f.queries, r.URL.Query().Get(promQLParam))
	if f.status != 0 {
		http.Error(w, "upstream down", f.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case vmQueryPath:
		_, _ = w.Write(instantBody(f.samples))
	case vmQueryRangePath:
		f.rangeCalls = append(f.rangeCalls, r.URL.Query())
		_, _ = w.Write(rangeBody(f.points))
	default:
		http.NotFound(w, r)
	}
}

// addRun adds the series of one run, as the runner would write them.
func (f *fakeVM) addRun(runID, typ, path string, started int64, stats map[string]float64) {
	f.samples = append(f.samples, vmSeries(runID, typ, path, "passed", started, stats)...)
}

func instantBody(samples []vmSample) []byte {
	result := make([]map[string]any, 0, len(samples))
	for _, s := range samples {
		result = append(result, map[string]any{"metric": s.labels, "value": []any{1700000000, s.value}})
	}
	return vmBody("vector", result)
}

func rangeBody(points [][2]any) []byte {
	result := []map[string]any{}
	if points != nil {
		result = append(result, map[string]any{"metric": map[string]string{}, "values": points})
	}
	return vmBody("matrix", result)
}

func vmBody(resultType string, result any) []byte {
	body, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": resultType, "result": result},
	})
	return body
}

// runsEngine serves the benchmark routes as cluster "east", with
// VictoriaMetrics reached at queryURL.
func runsEngine(t *testing.T, queryURL string, objects ...client.Object) *testutil.EngineHelper {
	t.Helper()
	cfg := testConfig()
	cfg.QueryURL = queryURL
	k8sClient := testutil.FakeClient(t, objects...)
	meta := cluster.ClusterMeta{Name: "east"}
	return testutil.NewEngineHelperWithAdmin(t, k8sClient, k8sClient, meta, New(cfg, zap.NewNop()))
}

func runsPath(name string) string {
	return benchmarkPath(name) + "/runs"
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v), w.Body.String())
	return v
}

func runsByID(runs []Run) map[string]Run {
	byID := make(map[string]Run, len(runs))
	for _, r := range runs {
		byID[r.RunID] = r
	}
	return byID
}

func TestRuns_ListNewestFirst(t *testing.T) {
	vm := newFakeVM(t)
	vm.addRun("r-old", "latency", "/api", 1700000000, map[string]float64{"p95_seconds": 0.31})
	vm.addRun("r-new", "latency", "/api", 1700100000, map[string]float64{"p95_seconds": 0.39})
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeBody[RunListResponse](t, w)
	require.Len(t, list.Items, 2)
	newest := list.Items[0]
	assert.Equal(t, "r-new", newest.RunID)
	assert.Equal(t, int64(1700100000), newest.StartedAt)
	assert.Equal(t, int64(1700100030), newest.EndedAt)
	assert.Equal(t, map[string]float64{"p95_seconds": 0.39}, newest.Stats, "start and end are not stats")
	assert.Equal(t, "r-old", list.Items[1].RunID)
}

func TestRuns_FilteredToClusterNamespaceCapp(t *testing.T) {
	vm := newFakeVM(t)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotEmpty(t, vm.queries)
	for _, want := range []string{
		`cluster="east"`,
		`namespace="ns1"`,
		`capp="my-capp"`,
		`capp_benchmark_result`,
		`[30d]`,
	} {
		assert.Contains(t, vm.queries[0], want)
	}
}

func TestRuns_EmptyIsAnEmptyList(t *testing.T) {
	vm := newFakeVM(t)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"items":[]`)
}

func TestRuns_SkipsSeriesWithoutStart(t *testing.T) {
	vm := newFakeVM(t)
	for _, s := range vmSeries("r-partial", "latency", "/api", "failed", 1700000000, map[string]float64{"p95_seconds": 0.5}) {
		if s.labels["stat"] != "start_seconds" {
			vm.samples = append(vm.samples, s)
		}
	}
	vm.addRun("r-full", "latency", "/api", 1700000500, map[string]float64{"p95_seconds": 0.4})
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeBody[RunListResponse](t, w)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "r-full", list.Items[0].RunID)
}

func TestRuns_CapsList(t *testing.T) {
	vm := newFakeVM(t)
	for i := range 60 {
		vm.addRun(fmt.Sprintf("r-%02d", i), "latency", "/", 1700000000+int64(i)*100, nil)
	}
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeBody[RunListResponse](t, w)
	require.Len(t, list.Items, maxListedRuns)
	assert.Equal(t, "r-59", list.Items[0].RunID)
	assert.Equal(t, "r-10", list.Items[maxListedRuns-1].RunID)
}

func TestRuns_MixedRevision(t *testing.T) {
	vm := newFakeVM(t)
	mixed := vmSeries("r-mixed", "latency", "/", "passed", 1700000000, nil)
	for i := range mixed {
		mixed[i].labels["revision"] = "c-00006,c-00007"
	}
	vm.samples = append(vm.samples, mixed...)
	vm.addRun("r-single", "latency", "/", 1700000100, nil)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	byID := runsByID(decodeBody[RunListResponse](t, w).Items)
	require.Contains(t, byID, "r-mixed")
	assert.True(t, byID["r-mixed"].Mixed)
	assert.Equal(t, "c-00006,c-00007", byID["r-mixed"].Revision)
	assert.False(t, byID["r-single"].Mixed)
}

func TestRuns_UpstreamFailureIs502(t *testing.T) {
	vm := newFakeVM(t)
	vm.status = http.StatusInternalServerError
	// The API key travels in the query URL and must not reach the response.
	e := runsEngine(t, vm.url+"?apikey=abc123", testCapp("my-capp"))

	w := e.Get(runsPath("my-capp"))
	require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "PROMETHEUS_QUERY_FAILED")
	assert.NotContains(t, w.Body.String(), "abc123")
	assert.NotContains(t, w.Body.String(), vm.url)
}

func TestRuns_CappMustExist(t *testing.T) {
	vm := newFakeVM(t)
	e := runsEngine(t, vm.url)

	w := e.Get(runsPath("missing"))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Empty(t, vm.queries, "VictoriaMetrics must not be queried for a missing Capp")
}

func TestRunDetail_NotFound(t *testing.T) {
	vm := newFakeVM(t)
	vm.addRun("r1", "latency", "/", 1700000000, nil)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp") + "/missing")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestRunDetail_PreviousIsSameTypeAndPath(t *testing.T) {
	vm := newFakeVM(t)
	vm.addRun("r1", "latency", "/a", 1700000000, nil)
	vm.addRun("r2", "throughput", "/a", 1700000100, nil)
	vm.addRun("r3", "latency", "/b", 1700000200, nil)
	vm.addRun("r4", "latency", "/a", 1700000300, nil)
	vm.addRun("r5", "latency", "/a", 1700000400, nil)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp") + "/r4")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	detail := decodeBody[RunDetailResponse](t, w)
	assert.Equal(t, "r4", detail.RunID)
	require.NotNil(t, detail.Previous)
	assert.Equal(t, "r1", detail.Previous.RunID)
}

func TestRunDetail_PreviousBeyondListCap(t *testing.T) {
	vm := newFakeVM(t)
	for i := range 60 {
		vm.addRun(fmt.Sprintf("r-%02d", i), "latency", "/", 1700000000+int64(i)*100, nil)
	}
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	// r-10 is the oldest run in the list. Its previous run, r-09, is not.
	w := e.Get(runsPath("my-capp") + "/r-10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	detail := decodeBody[RunDetailResponse](t, w)
	require.NotNil(t, detail.Previous)
	assert.Equal(t, "r-09", detail.Previous.RunID)
}

func TestRunDetail_NoPreviousIsOmitted(t *testing.T) {
	vm := newFakeVM(t)
	vm.addRun("r1", "latency", "/", 1700000000, nil)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp") + "/r1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), `"previous"`)
}

func TestRunDetail_ChartQueries(t *testing.T) {
	tests := []struct {
		name      string
		typ       Type
		wantQuery []string
		unit      string
	}{
		{
			name:      "latency",
			typ:       TypeLatency,
			wantQuery: []string{metricK6P95 + "{", `run_id="r1"`, `cluster="east"`, `namespace="ns1"`, `capp="my-capp"`},
			unit:      "seconds",
		},
		{
			name:      "throughput",
			typ:       TypeThroughput,
			wantQuery: []string{"rate(" + metricK6Requests + "{", `run_id="r1"`, `cluster="east"`, `namespace="ns1"`, `capp="my-capp"`, "[1m]"},
			unit:      "requests/s",
		},
		{
			name: "scale",
			typ:  TypeScale,
			wantQuery: []string{
				`sum(kn_revision_pods_count{cluster="east",k8s_namespace_name="ns1",kn_service_name="my-capp"})`,
			},
			unit: "pods",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := newFakeVM(t)
			vm.points = [][2]any{{1700000000, "0.31"}, {1700000015, "0.39"}}
			vm.addRun("r1", string(tt.typ), "/api", 1700000000, nil)
			e := runsEngine(t, vm.url, testCapp("my-capp"))

			w := e.Get(runsPath("my-capp") + "/r1")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			detail := decodeBody[RunDetailResponse](t, w)

			require.Len(t, vm.rangeCalls, 1)
			call := vm.rangeCalls[0]
			for _, want := range tt.wantQuery {
				assert.Contains(t, call.Get(promQLParam), want)
			}
			assert.Equal(t, "1700000000", call.Get("start"))
			assert.Equal(t, "1700000030", call.Get("end"))
			assert.Equal(t, "15s", call.Get("step"), "a 30 second run uses the 15 second minimum step")
			assert.Equal(t, tt.unit, detail.Chart.Unit)
			assert.NotEmpty(t, detail.Chart.Title)
			assert.Equal(t, [][2]float64{{1700000000, 0.31}, {1700000015, 0.39}}, detail.Chart.Points)
		})
	}
}

func TestRunDetail_NoChartDataStillReturnsRun(t *testing.T) {
	vm := newFakeVM(t)
	vm.addRun("r1", "latency", "/", 1700000000, nil)
	e := runsEngine(t, vm.url, testCapp("my-capp"))

	w := e.Get(runsPath("my-capp") + "/r1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	detail := decodeBody[RunDetailResponse](t, w)
	assert.Equal(t, "r1", detail.RunID)
	assert.Equal(t, [][2]float64{}, detail.Chart.Points)
	assert.Contains(t, w.Body.String(), `"points":[]`)
}
