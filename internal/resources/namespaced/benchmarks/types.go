package benchmarks

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const (
	maxTargetPathLen     = 2048
	maxTargetHeaders     = 32
	maxTargetHeaderBytes = 8 << 10
)

// TriggerRequest selects which baked-in check to run and what it sends.
type TriggerRequest struct {
	Type Type `json:"type"`

	// Path is the path and optional query requested on the Capp. Default: "/".
	Path string `json:"path,omitempty"`

	// Headers are added to every benchmark request. They are stored in the Job
	// spec, so anyone who can read Jobs in the namespace can read them.
	Headers map[string]string `json:"headers,omitempty"`
}

// validate checks the target. Errors never include header values, which can be credentials.
func (r TriggerRequest) validate() error {
	if r.Path != "" {
		if err := validateTargetPath(r.Path); err != nil {
			return err
		}
	}
	return validateTargetHeaders(r.Headers)
}

// validateTargetPath accepts only an origin-form path, so the request stays on the Capp.
func validateTargetPath(path string) error {
	if len(path) > maxTargetPathLen {
		return fmt.Errorf("path must be at most %d bytes", maxTargetPathLen)
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("path must start with a single /")
	}
	for i := range len(path) {
		if c := path[i]; c <= ' ' || c >= 0x7f || c == '#' {
			return errors.New("path must be printable ASCII without spaces or a fragment; percent-encode other characters")
		}
	}
	if _, err := url.ParseRequestURI(path); err != nil {
		return errors.New("path is not a valid URL path")
	}
	return nil
}

func validateTargetHeaders(headers map[string]string) error {
	if len(headers) > maxTargetHeaders {
		return fmt.Errorf("at most %d headers are allowed", maxTargetHeaders)
	}
	size := 0
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid header name %q", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("invalid value for header %q", name)
		}
		// Host would route the request to another Service behind the same
		// gateway. The others are set by the HTTP client.
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Content-Length", "Transfer-Encoding", "Connection":
			return fmt.Errorf("header %q cannot be set", name)
		}
		size += len(name) + len(value)
	}
	if size > maxTargetHeaderBytes {
		return fmt.Errorf("headers must total at most %d bytes", maxTargetHeaderBytes)
	}
	return nil
}

// TriggerResponse is returned after a benchmark Job is created.
type TriggerResponse struct {
	CappName string `json:"cappName"`
	JobName  string `json:"jobName"`
	Type     Type   `json:"type"`
}

const (
	// StatusRunning means the Job has not finished.
	StatusRunning = "running"
	// StatusCompleted means the Job's Complete condition is true.
	StatusCompleted = "completed"
	// StatusFailed means the Job's Failed condition is true.
	StatusFailed = "failed"
)

// StatusResponse is returned by GET .../benchmark.
// JobName and Status are empty when no benchmark Job exists.
type StatusResponse struct {
	JobName string `json:"jobName"`
	Status  string `json:"status"`
}

// DashboardQuery is one PromQL series on a dashboard.
type DashboardQuery struct {
	Legend string `json:"legend"`
	Query  string `json:"query"`
}

// Dashboard is a preset chart for one Capp.
type Dashboard struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Unit        string           `json:"unit"`
	Queries     []DashboardQuery `json:"queries"`
}

// DashboardsResponse is returned by GET .../metrics/dashboards.
type DashboardsResponse struct {
	Dashboards []Dashboard `json:"dashboards"`
}
