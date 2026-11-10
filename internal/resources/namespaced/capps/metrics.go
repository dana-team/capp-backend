package capps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"golang.org/x/sync/errgroup"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// metricsQueryTimeout bounds one Prometheus round trip, including reading
	// the body.
	metricsQueryTimeout = 15 * time.Second

	// maxMetricsResponseBytes caps the Prometheus response body.
	maxMetricsResponseBytes = 4 << 20

	// maxRangePoints matches Prometheus' own per-series limit for
	// query_range, so a request is rejected here before it reaches the server.
	maxRangePoints = 11000
)

var errMetricsResponseTooLarge = fmt.Errorf("prometheus response exceeds %d bytes", maxMetricsResponseBytes)

// MetricInfo describes one configured metric.
type MetricInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`
	Unit        string `json:"unit,omitempty"`
	// Legends lists the metric's lines in the order Series is returned.
	Legends []string `json:"legends"`
}

// MetricListResponse is returned by GET .../capps/:name/metrics.
type MetricListResponse struct {
	Items []MetricInfo `json:"items"`
}

// MetricSeriesResult is one line of a metric. Result is the Prometheus data
// in its standard JSON shape: a list of {metric, value} for "vector" and of
// {metric, values} for "matrix".
type MetricSeriesResult struct {
	Legend     string      `json:"legend"`
	ResultType string      `json:"resultType"`
	Result     model.Value `json:"result"`
}

// MetricQueryResponse is returned by GET .../capps/:name/metrics/:metric.
// Series follows the order of Legends. Warnings and Infos are collected from
// every series.
type MetricQueryResponse struct {
	MetricInfo
	Series   []MetricSeriesResult `json:"series"`
	Warnings promv1.Warnings      `json:"warnings,omitempty"`
	Infos    promv1.Infos         `json:"infos,omitempty"`
}

// newPrometheusAPI builds the Prometheus client once at startup. It returns
// nil when observability is disabled.
func newPrometheusAPI(cfg config.ObservabilityConfig) (promv1.API, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	c, err := api.NewClient(api.Config{
		Address: cfg.QueryURL,
		Client:  newPrometheusHTTPClient(http.DefaultTransport),
	})
	if err != nil {
		// url.Parse errors quote the URL, which can carry a token.
		return nil, errors.New("invalid Prometheus query URL")
	}
	return promv1.NewAPI(c), nil
}

// newPrometheusHTTPClient returns a client that does not follow redirects,
// so a token in the query URL is never sent to another host, and that fails
// reads past maxMetricsResponseBytes.
func newPrometheusHTTPClient(base http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   metricsQueryTimeout,
		Transport: limitedTransport{base: base},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// listMetrics handles GET .../namespaces/:namespace/capps/:name/metrics.
func (h *Handler) listMetrics(c *gin.Context) {
	if !h.observability.Enabled {
		apierrors.Respond(c, apierrors.NewNotSupported("metrics"))
		return
	}
	if !h.authorizeCapp(c) {
		return
	}

	items := make([]MetricInfo, 0, len(h.observability.Metrics))
	for _, m := range h.observability.Metrics {
		items = append(items, metricInfo(m))
	}
	c.JSON(http.StatusOK, MetricListResponse{Items: items})
}

// queryMetric handles GET .../namespaces/:namespace/capps/:name/metrics/:metric.
// Without start/end/step it runs an instant query at the current time; with
// all three it runs a range query.
func (h *Handler) queryMetric(c *gin.Context) {
	if !h.observability.Enabled {
		apierrors.Respond(c, apierrors.NewNotSupported("metrics"))
		return
	}
	if !h.authorizeCapp(c) {
		return
	}

	metricName := c.Param("metric")
	metric, ok := h.findMetric(metricName)
	if !ok {
		apierrors.Respond(c, apierrors.NewNotFound("Metric", metricName))
		return
	}

	rng, isRange, err := parseRange(c)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), metricsQueryTimeout)
	defer cancel()
	// Up to MaxMetricSeries parallel queries share one byte budget, so the
	// per-series cap enforced by limitedTransport can't be multiplied into a
	// much larger total for one request.
	ctx = withResponseBudget(ctx, maxMetricsResponseBytes)

	// One instant for every series, so instant results line up.
	now := time.Now()
	series := metric.AllSeries()
	results := make([]seriesOutcome, len(series))
	g, gctx := errgroup.WithContext(ctx)
	for i, s := range series {
		query := renderQuery(metric, s.Query, h.observability.Labels, c.Param("cluster"), c.Param("namespace"), c.Param("name"))
		g.Go(func() error {
			var out seriesOutcome
			var err error
			if isRange {
				out.result, out.warnings, out.infos, err = h.prometheus.QueryRange(gctx, query, rng)
			} else {
				out.result, out.warnings, out.infos, err = h.prometheus.Query(gctx, query, now)
			}
			if err == nil && out.result == nil {
				err = errors.New("prometheus returned no result")
			}
			results[i] = out
			return err
		})
	}
	if err := g.Wait(); err != nil {
		switch {
		case c.Request.Context().Err() != nil:
			// The caller disconnected (e.g. navigated away). There is no
			// client left to answer, and this is not a Prometheus failure,
			// so skip the error response instead of logging a false alarm.
			c.Abort()
			return
		case errors.Is(err, context.DeadlineExceeded):
			apierrors.Respond(c, apierrors.NewPrometheusQueryTimeout(withoutURL(err)))
			return
		default:
			apierrors.Respond(c, apierrors.NewPrometheusQueryFailed(withoutURL(err)))
			return
		}
	}

	resp := MetricQueryResponse{MetricInfo: metricInfo(metric), Series: make([]MetricSeriesResult, len(series))}
	for i, out := range results {
		resp.Series[i] = MetricSeriesResult{
			Legend:     series[i].Legend,
			ResultType: out.result.Type().String(),
			Result:     out.result,
		}
		resp.Warnings = append(resp.Warnings, out.warnings...)
		resp.Infos = append(resp.Infos, out.infos...)
	}
	c.JSON(http.StatusOK, resp)
}

// seriesOutcome is what one Prometheus call returned.
type seriesOutcome struct {
	result   model.Value
	warnings promv1.Warnings
	infos    promv1.Infos
}

// authorizeCapp reads the Capp with the caller's scoped client. Metrics come
// from a shared Prometheus, so this read is what limits a caller to Capps
// their Kubernetes RBAC lets them see. It writes the error response and
// returns false when the caller may not proceed.
func (h *Handler) authorizeCapp(c *gin.Context) bool {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return false
	}
	name := c.Param("name")
	var capp cappv1alpha1.Capp
	if err := k8sClient.Get(c.Request.Context(), client.ObjectKey{Namespace: c.Param("namespace"), Name: name}, &capp); err != nil {
		if k8serrors.IsNotFound(err) {
			apierrors.Respond(c, apierrors.NewNotFound("Capp", name))
			return false
		}
		apierrors.Respond(c, err)
		return false
	}
	return true
}

func (h *Handler) findMetric(name string) (config.MetricsQuery, bool) {
	for _, m := range h.observability.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return config.MetricsQuery{}, false
}

func metricInfo(m config.MetricsQuery) MetricInfo {
	series := m.AllSeries()
	legends := make([]string, len(series))
	for i, s := range series {
		legends[i] = s.Legend
	}
	return MetricInfo{Name: m.Name, Description: m.Description, Type: m.Type, Unit: m.Unit, Legends: legends}
}

// renderQuery replaces the selector placeholder in query with matchers for
// the Capp's namespace and name, the cluster when the metric has a cluster
// label, and the extra labels in sorted order.
func renderQuery(m config.MetricsQuery, query string, extra map[string]string, cluster, namespace, name string) string {
	matchers := []string{
		matcher(m.NamespaceLabel, namespace),
		matcher(m.ObjectLabel, name),
	}
	if m.ClusterLabel != "" {
		matchers = append(matchers, matcher(m.ClusterLabel, cluster))
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		matchers = append(matchers, matcher(k, extra[k]))
	}
	return strings.ReplaceAll(query, config.MetricsSelectorPlaceholder, strings.Join(matchers, ","))
}

// matcher returns label="value". PromQL string literals use Go escaping, so
// strconv.Quote keeps any value from closing the string early.
func matcher(label, value string) string {
	return label + "=" + strconv.Quote(value)
}

// parseRange reads start, end and step. It returns isRange=false when none
// is set, and an error when only some are set or they are out of bounds.
func parseRange(c *gin.Context) (promv1.Range, bool, error) {
	startRaw, endRaw, stepRaw := c.Query("start"), c.Query("end"), c.Query("step")
	if startRaw == "" && endRaw == "" && stepRaw == "" {
		return promv1.Range{}, false, nil
	}
	if startRaw == "" || endRaw == "" || stepRaw == "" {
		return promv1.Range{}, false, apierrors.NewBadRequest("start, end and step must be set together")
	}

	start, err := parseTime(startRaw)
	if err != nil {
		return promv1.Range{}, false, apierrors.NewBadRequest("start must be an RFC 3339 time or a Unix timestamp")
	}
	end, err := parseTime(endRaw)
	if err != nil {
		return promv1.Range{}, false, apierrors.NewBadRequest("end must be an RFC 3339 time or a Unix timestamp")
	}
	step, err := parseStep(stepRaw)
	if err != nil {
		return promv1.Range{}, false, apierrors.NewBadRequest("step must be a positive duration, such as 30s, or a number of seconds")
	}
	if end.Before(start) {
		return promv1.Range{}, false, apierrors.NewBadRequest("end must not be before start")
	}
	if end.Sub(start)/step > maxRangePoints {
		return promv1.Range{}, false, apierrors.NewBadRequest(
			fmt.Sprintf("range is too long for step; at most %d points are allowed", maxRangePoints))
	}
	return promv1.Range{Start: start, End: end, Step: step}, true, nil
}

// parseTime accepts the same forms as the Prometheus HTTP API.
func parseTime(s string) (time.Time, error) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC(), nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// parseStep accepts a number of seconds or a Prometheus duration ("30s",
// "1m", "1d"). model.ParseDuration (unlike time.ParseDuration) supports the
// "d", "w" and "y" units Prometheus' own query_range endpoint accepts.
func parseStep(s string) (time.Duration, error) {
	var d time.Duration
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		d = time.Duration(f * float64(time.Second))
	} else if md, err := model.ParseDuration(s); err == nil {
		d = time.Duration(md)
	} else {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("step must be positive")
	}
	return d, nil
}

// withoutURL drops the request URL from an HTTP client error, because the
// URL can carry a token.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// responseBudgetKey is the context key for a request's shared byte budget.
type responseBudgetKey struct{}

// withResponseBudget attaches a byte budget to ctx, shared by every HTTP
// response read while that context (or a context derived from it, e.g. by
// errgroup.WithContext) is in use. A single request can fan out into several
// concurrent Prometheus calls (one per series); without a shared budget each
// call would get its own maxMetricsResponseBytes allowance, multiplying the
// real cap by the number of series.
func withResponseBudget(ctx context.Context, max int64) context.Context {
	budget := &atomic.Int64{}
	budget.Store(max)
	return context.WithValue(ctx, responseBudgetKey{}, budget)
}

func responseBudgetFrom(ctx context.Context) *atomic.Int64 {
	budget, _ := ctx.Value(responseBudgetKey{}).(*atomic.Int64)
	return budget
}

// limitedTransport wraps every response body in limitedBody.
type limitedTransport struct {
	base http.RoundTripper
}

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	budget := responseBudgetFrom(req.Context())
	if budget == nil {
		// No shared budget was attached to this context (e.g. a call made
		// outside queryMetric); fall back to an independent per-response cap.
		budget = &atomic.Int64{}
		budget.Store(maxMetricsResponseBytes)
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, budget: budget}
	return resp, nil
}

// limitedBody returns errMetricsResponseTooLarge instead of silently
// truncating, so a cut-off body is never decoded as a partial result.
type limitedBody struct {
	io.ReadCloser
	budget *atomic.Int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	remaining := b.budget.Load()
	if remaining <= 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errMetricsResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.budget.Add(-int64(n))
	return n, err
}
