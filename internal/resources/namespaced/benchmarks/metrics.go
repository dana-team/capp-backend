package benchmarks

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	vmQueryTimeout     = 15 * time.Second
	maxVMResponseBytes = 4 << 20
	vmQueryPath        = "/api/v1/query"
	vmQueryRangePath   = "/api/v1/query_range"
	promQLParam        = "query"
)

// query handles GET .../metrics/query.
func (h *Handler) query(c *gin.Context) {
	values, err := instantParams(c)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	h.proxyPromQL(c, vmQueryPath, values)
}

// queryRange handles GET .../metrics/query_range.
func (h *Handler) queryRange(c *gin.Context) {
	values, err := rangeParams(c)
	if err != nil {
		apierrors.Respond(c, err)
		return
	}
	h.proxyPromQL(c, vmQueryRangePath, values)
}

func instantParams(c *gin.Context) (url.Values, error) {
	q := c.Query(promQLParam)
	if q == "" {
		return nil, apierrors.NewBadRequest("query is required")
	}
	values := url.Values{}
	values.Set(promQLParam, q)
	if t := c.Query("time"); t != "" {
		values.Set("time", t)
	}
	return values, nil
}

func rangeParams(c *gin.Context) (url.Values, error) {
	q := c.Query(promQLParam)
	start := c.Query("start")
	end := c.Query("end")
	step := c.Query("step")
	if q == "" || start == "" || end == "" || step == "" {
		return nil, apierrors.NewBadRequest("query, start, end, and step are required")
	}
	values := url.Values{}
	values.Set(promQLParam, q)
	values.Set("start", start)
	values.Set("end", end)
	values.Set("step", step)
	return values, nil
}

// proxyPromQL forwards one PromQL request to the configured query URL.
func (h *Handler) proxyPromQL(c *gin.Context, path string, values url.Values) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}

	namespace := c.Param("namespace")
	cappName := c.Param("name")
	ctx := c.Request.Context()

	capp := &cappv1alpha1.Capp{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cappName}, capp); err != nil {
		apierrors.Respond(c, err)
		return
	}

	endpoint, err := metricsEndpoint(h.cfg.QueryURL, path, values)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		apierrors.Respond(c, apierrors.NewInternal(withoutURL(err)))
		return
	}

	resp, err := h.vmHTTP().Do(req)
	if err != nil {
		h.failQuery(c, cappName, namespace, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVMResponseBytes+1))
	if err != nil {
		h.failQuery(c, cappName, namespace, err)
		return
	}
	if len(body) > maxVMResponseBytes {
		h.failQuery(c, cappName, namespace, fmt.Errorf("response exceeds %d bytes", maxVMResponseBytes))
		return
	}
	if resp.StatusCode >= 500 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		h.failQuery(c, cappName, namespace, fmt.Errorf("status %d", resp.StatusCode))
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, body)
}

func (h *Handler) failQuery(c *gin.Context, cappName, namespace string, err error) {
	err = withoutURL(err)
	h.logger.Error("prometheus query failed",
		zap.String("capp", cappName),
		zap.String("namespace", namespace),
		zap.Error(err),
	)
	apierrors.Respond(c, apierrors.NewPrometheusQueryFailed(err))
}

func (h *Handler) vmHTTP() *http.Client {
	if h.vmClient != nil {
		return h.vmClient
	}
	return newVMClient()
}

func newVMClient() *http.Client {
	return &http.Client{
		Timeout: vmQueryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// metricsEndpoint adds path and values to queryURL, keeping any query
// parameters it already has, such as an API key.
func metricsEndpoint(queryURL, path string, values url.Values) (string, error) {
	base, err := url.Parse(strings.TrimSpace(queryURL))
	if err != nil || base.Host == "" {
		// url.Parse errors quote the URL, which can carry a token.
		return "", errors.New("invalid Prometheus query URL")
	}
	endpoint := base.JoinPath(path)
	query := endpoint.Query()
	for key, value := range values {
		query[key] = value
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

// withoutURL drops the request URL from an HTTP client error, because the URL
// can carry a token.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
