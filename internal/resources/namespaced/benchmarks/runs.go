package benchmarks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// runLookback is how far back runs are read.
	runLookback = "30d"
	// maxListedRuns caps the run list. The detail view still searches every run.
	maxListedRuns = 50
	// metricK6P95 and metricK6Requests are the k6 series the latency and throughput charts read.
	// They are unverified against a live k6 remote write. Correct them here if the names differ.
	metricK6P95      = "k6_http_req_duration_p95_seconds"
	metricK6Requests = "k6_http_reqs_total"

	// metricRunResult is the one series per stat that the runner writes for each run.
	metricRunResult = "capp_benchmark_result"
	labelRunID      = "run_id"
	labelStat       = "stat"
	statStart       = "start_seconds"
	statEnd         = "end_seconds"
	// revisionSeparator joins the two revisions of a run made during a staged rollout.
	revisionSeparator = ","
	// minChartStep is the smallest query_range step, in seconds.
	minChartStep = 15
	// chartSteps is roughly how many points a run chart has.
	chartSteps = 60
)

// listRuns handles GET .../benchmark/runs.
func (h *Handler) listRuns(c *gin.Context) {
	clusterName, namespace, cappName, ok := h.runScope(c)
	if !ok {
		return
	}
	runs, err := h.fetchRuns(c.Request.Context(), clusterName, namespace, cappName)
	if err != nil {
		h.failQuery(c, cappName, namespace, err)
		return
	}
	if len(runs) > maxListedRuns {
		runs = runs[:maxListedRuns]
	}
	c.JSON(http.StatusOK, RunListResponse{Items: runs})
}

// getRun handles GET .../benchmark/runs/:run.
func (h *Handler) getRun(c *gin.Context) {
	clusterName, namespace, cappName, ok := h.runScope(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	runs, err := h.fetchRuns(ctx, clusterName, namespace, cappName)
	if err != nil {
		h.failQuery(c, cappName, namespace, err)
		return
	}

	runID := c.Param("run")
	i := slices.IndexFunc(runs, func(r Run) bool { return r.RunID == runID })
	if i < 0 {
		apierrors.Respond(c, apierrors.NewNotFound("benchmark run", runID))
		return
	}
	run := runs[i]

	chart, err := h.runChart(ctx, clusterName, namespace, cappName, run)
	if err != nil {
		h.failQuery(c, cappName, namespace, err)
		return
	}
	c.JSON(http.StatusOK, RunDetailResponse{
		Run:      run,
		Previous: previousRun(runs, run),
		Chart:    chart,
	})
}

// runScope checks that the Capp exists and returns the cluster, namespace and
// Capp name for a run request. On failure it writes the response and returns false.
func (h *Handler) runScope(c *gin.Context) (clusterName, namespace, cappName string, ok bool) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return "", "", "", false
	}
	meta, err := clusterMeta(c)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return "", "", "", false
	}
	namespace = c.Param("namespace")
	cappName = c.Param("name")
	capp := &cappv1alpha1.Capp{}
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: namespace, Name: cappName}, capp); err != nil {
		apierrors.Respond(c, err)
		return "", "", "", false
	}
	return meta.Name, namespace, cappName, true
}

// fetchRuns returns the Capp's runs from the lookback window, newest first.
// A run whose start or end time is missing is left out, because it is partial.
func (h *Handler) fetchRuns(ctx context.Context, clusterName, namespace, cappName string) ([]Run, error) {
	query := fmt.Sprintf("last_over_time(%s{%s}[%s])",
		metricRunResult, summarySelector(clusterName, namespace, cappName), runLookback)
	status, body, err := h.fetchPromQL(ctx, vmQueryPath, url.Values{promQLParam: {query}})
	if err != nil {
		return nil, err
	}
	raw, err := promResult(status, body)
	if err != nil {
		return nil, err
	}
	var samples []vectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return nil, fmt.Errorf("decode run summaries: %w", err)
	}
	runs := collectRuns(samples)
	slices.SortFunc(runs, func(a, b Run) int {
		if c := cmp.Compare(b.StartedAt, a.StartedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.RunID, b.RunID)
	})
	return runs, nil
}

// runChart returns the run's series over the run's own window. A run with no
// data gets an empty chart, not an error.
func (h *Handler) runChart(ctx context.Context, clusterName, namespace, cappName string, run Run) (RunChart, error) {
	chart := RunChart{Points: [][2]float64{}}
	var query string
	switch run.Type {
	case TypeLatency:
		chart.Title, chart.Unit = "Latency p95", "seconds"
		query = fmt.Sprintf("%s{%s}", metricK6P95, runSelector(clusterName, namespace, cappName, run.RunID))
	case TypeThroughput:
		chart.Title, chart.Unit = "Requests", "requests/s"
		query = fmt.Sprintf("sum(rate(%s{%s}[1m]))", metricK6Requests, runSelector(clusterName, namespace, cappName, run.RunID))
	case TypeScale:
		chart.Title, chart.Unit = "Ready pods", "pods"
		query = fmt.Sprintf("sum(%s{%s})", metricPodsReady, cappSelector(clusterName, namespace, cappName))
	default:
		return chart, nil
	}

	step := max(int64(minChartStep), (run.EndedAt-run.StartedAt)/chartSteps)
	values := url.Values{
		promQLParam: {query},
		"start":     {strconv.FormatInt(run.StartedAt, 10)},
		"end":       {strconv.FormatInt(run.EndedAt, 10)},
		"step":      {fmt.Sprintf("%ds", step)},
	}
	status, body, err := h.fetchPromQL(ctx, vmQueryRangePath, values)
	if err != nil {
		return RunChart{}, err
	}
	raw, err := promResult(status, body)
	if err != nil {
		return RunChart{}, err
	}
	points, err := firstSeriesPoints(raw)
	if err != nil {
		return RunChart{}, err
	}
	chart.Points = points
	return chart, nil
}

// previousRun returns the most recent run before run with the same type and
// path, or nil when there is none. It searches every run passed in.
func previousRun(runs []Run, run Run) *Run {
	var prev *Run
	for i := range runs {
		candidate := &runs[i]
		if candidate.RunID == run.RunID || candidate.Type != run.Type || candidate.Path != run.Path ||
			candidate.StartedAt >= run.StartedAt {
			continue
		}
		if prev == nil || candidate.StartedAt > prev.StartedAt {
			prev = candidate
		}
	}
	return prev
}

// summarySelector selects one Capp's run summaries.
func summarySelector(clusterName, namespace, cappName string) string {
	return fmt.Sprintf(`cluster="%s",namespace="%s",capp="%s"`,
		promQLString(clusterName), promQLString(namespace), promQLString(cappName))
}

// runSelector selects one run's k6 series. The runner tags every k6 sample
// with the run, Capp, namespace and cluster.
func runSelector(clusterName, namespace, cappName, runID string) string {
	return fmt.Sprintf(`%s="%s",cluster="%s",namespace="%s",capp="%s"`,
		labelRunID, promQLString(runID), promQLString(clusterName),
		promQLString(namespace), promQLString(cappName))
}

// vectorSample is one series in an instant query response.
type vectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  samplePair        `json:"value"`
}

// samplePair is a Prometheus [timestamp, value] pair. The timestamp is a JSON
// number and the value is a JSON string.
type samplePair [2]json.RawMessage

// number parses element i, which may be a JSON number or a quoted string.
func (p samplePair) number(i int) (float64, error) {
	raw := string(p[i])
	var s string
	if err := json.Unmarshal(p[i], &s); err == nil {
		raw = s
	}
	return strconv.ParseFloat(raw, 64)
}

// promResult checks a Prometheus API response and returns its result.
// Errors never include the query URL.
func promResult(status int, body []byte) (json.RawMessage, error) {
	if status != http.StatusOK {
		return nil, fmt.Errorf("status %d", status)
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "success" {
		return nil, errors.New("query did not succeed")
	}
	return envelope.Data.Result, nil
}

// collectRuns groups summary series by run ID, one stat per series. It skips
// runs that lack a start or end time.
func collectRuns(samples []vectorSample) []Run {
	type partial struct {
		run      Run
		hasStart bool
		hasEnd   bool
	}
	byID := map[string]*partial{}
	for _, s := range samples {
		id, stat := s.Metric[labelRunID], s.Metric[labelStat]
		if id == "" || stat == "" {
			continue
		}
		v, err := s.Value.number(1)
		if err != nil || !finite(v) {
			continue
		}
		p, ok := byID[id]
		if !ok {
			revision := s.Metric["revision"]
			p = &partial{run: Run{
				RunID:    id,
				Type:     Type(s.Metric["type"]),
				Path:     s.Metric["path"],
				Revision: revision,
				Mixed:    strings.Contains(revision, revisionSeparator),
				Result:   s.Metric["result"],
				Reason:   s.Metric["reason"],
				Stats:    map[string]float64{},
			}}
			byID[id] = p
		}
		switch stat {
		case statStart:
			p.run.StartedAt, p.hasStart = int64(v), true
		case statEnd:
			p.run.EndedAt, p.hasEnd = int64(v), true
		default:
			p.run.Stats[stat] = v
		}
	}

	runs := make([]Run, 0, len(byID))
	for _, p := range byID {
		if p.hasStart && p.hasEnd {
			runs = append(runs, p.run)
		}
	}
	return runs
}

// firstSeriesPoints returns the points of the first series in a range query
// result. Points that are not finite numbers are skipped, because JSON cannot encode them.
func firstSeriesPoints(raw json.RawMessage) ([][2]float64, error) {
	var series []struct {
		Values []samplePair `json:"values"`
	}
	if err := json.Unmarshal(raw, &series); err != nil {
		return nil, fmt.Errorf("decode range result: %w", err)
	}
	points := [][2]float64{}
	if len(series) == 0 {
		return points, nil
	}
	for _, p := range series[0].Values {
		ts, err := p.number(0)
		if err != nil {
			return nil, fmt.Errorf("decode range point: %w", err)
		}
		v, err := p.number(1)
		if err != nil {
			return nil, fmt.Errorf("decode range point: %w", err)
		}
		if finite(v) {
			points = append(points, [2]float64{ts, v})
		}
	}
	return points, nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
