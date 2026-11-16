package functions

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/resources/namespaced"
	"github.com/gin-gonic/gin"
)

const (
	maxTestRequestBytes  = 1 << 20
	maxTestResponseBytes = 1 << 20

	// The router ends a call at the function's timeout; the margin lets its
	// own timeout answer arrive before the backend gives up.
	testTimeoutMargin = 10 * time.Second
)

// newTestClient returns the HTTP client of the test route. It does not
// follow redirects: the backend calls the router only, and a function must
// not be able to send it anywhere else.
func newTestClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// test handles POST /api/v1/clusters/:cluster/namespaces/:namespace/cfunc/functions/:name/test
//
// It sends one request through an http trigger of the function and returns
// what the function answered. The target is always the configured router
// and a path of this function, never an address from the request. Fission's
// direct function path accepts only requests signed with a key the backend
// does not hold, so a function with no http trigger cannot be called.
func (h *Handler) test(c *gin.Context) {
	k8sClient := namespaced.ExtractClient(c)
	if k8sClient == nil {
		return
	}
	routerURL := h.routerURLs[c.Param("cluster")]
	if routerURL == "" {
		apierrors.Respond(c, apierrors.NewNotSupported("function test calls (no cfuncRouterURL is configured for this cluster)"))
		return
	}

	var req FunctionTestRequest
	if c.Request.ContentLength != 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTestRequestBytes)
		if err := c.ShouldBindJSON(&req); err != nil {
			apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("invalid request body: %s", err)))
			return
		}
	}

	objects, ok := h.load(c, k8sClient)
	if !ok {
		return
	}
	resp := FromK8s(objects, routerURL, h.settings)

	trigger, method, err := testTarget(resp.Triggers, req)
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(err.Error()))
		return
	}

	outgoing, err := http.NewRequestWithContext(c.Request.Context(), method, trigger.URL, strings.NewReader(req.Body))
	if err != nil {
		apierrors.Respond(c, apierrors.NewBadRequest(fmt.Sprintf("cannot build the test request: %s", err)))
		return
	}
	for name, value := range req.Headers {
		outgoing.Header.Set(name, value)
	}

	client := *h.testClient
	client.Timeout = time.Duration(resp.TimeoutSeconds)*time.Second + testTimeoutMargin
	extendWriteDeadline(c, client.Timeout+writeMargin)
	start := time.Now()
	answer, err := client.Do(outgoing)
	if err != nil {
		apierrors.Respond(c, apierrors.NewCfuncRouterFailed(err))
		return
	}
	defer func() { _ = answer.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(answer.Body, maxTestResponseBytes+1))
	if err != nil {
		apierrors.Respond(c, apierrors.NewCfuncRouterFailed(err))
		return
	}
	result := FunctionTestResponse{
		URL:        trigger.URL,
		Method:     method,
		StatusCode: answer.StatusCode,
		Headers:    make(map[string]string, len(answer.Header)),
		DurationMs: time.Since(start).Milliseconds(),
	}
	if len(body) > maxTestResponseBytes {
		body = body[:maxTestResponseBytes]
		result.Truncated = true
	}
	result.Body = string(body)
	for name := range answer.Header {
		result.Headers[name] = answer.Header.Get(name)
	}

	c.JSON(http.StatusOK, result)
}

// testTarget picks the http trigger and the method of a test call.
func testTarget(triggers []FunctionTrigger, req FunctionTestRequest) (FunctionTrigger, string, error) {
	var candidates []FunctionTrigger
	for _, trigger := range triggers {
		if trigger.Type == TriggerTypeHTTP && (req.Trigger == "" || req.Trigger == trigger.Name) {
			candidates = append(candidates, trigger)
		}
	}
	switch {
	case len(candidates) == 0 && req.Trigger != "":
		return FunctionTrigger{}, "", fmt.Errorf("the function has no http trigger named %q", req.Trigger)
	case len(candidates) == 0:
		return FunctionTrigger{}, "", errors.New("the function has no http trigger; a test call goes through one")
	case len(candidates) > 1:
		return FunctionTrigger{}, "", errors.New("the function has more than one http trigger; name one in \"trigger\"")
	}

	trigger := candidates[0]
	if strings.ContainsAny(trigger.FullPath, "{}") {
		return FunctionTrigger{}, "", fmt.Errorf("the path of trigger %q has a variable, which a test call cannot fill", trigger.Name)
	}
	method := strings.ToUpper(req.Method)
	if method == "" && len(trigger.Methods) > 0 {
		method = trigger.Methods[0]
	}
	if !slices.Contains(trigger.Methods, method) {
		return FunctionTrigger{}, "", fmt.Errorf("trigger %q does not serve %s; its methods are %s",
			trigger.Name, method, strings.Join(trigger.Methods, ", "))
	}
	return trigger, method, nil
}
