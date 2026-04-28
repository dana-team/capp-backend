// Package k8s provides thin helper utilities over the Kubernetes client
// machinery used throughout capp-backend.
//
// These helpers centralise two responsibilities that every component needs:
//  1. Building a runtime.Scheme that includes all API types capp-backend works with.
//  2. Detecting whether the target cluster runs OpenShift.
package k8s

import (
	"context"
	"fmt"
	"net/http"

	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// BuildScheme constructs and returns a *runtime.Scheme that has the following
// API type groups registered:
//
//   - k8s.io/api (core, apps, batch, …) via clientgoscheme
//   - github.com/dana-team/container-app-operator/api/v1alpha1 (Capp, CappRevision, CappConfig)
//
// All resource handlers obtain their scheme from this function so the set of
// registered types is consistent across the application.
func BuildScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("k8s: registering client-go scheme: %w", err)
	}

	if err := cappv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("k8s: registering rcs.dana.io/v1alpha1 scheme: %w", err)
	}

	return s, nil
}

// IsOpenShift probes the cluster at restCfg to determine whether it exposes
// the OpenShift route.openshift.io API group. It does this by issuing a single
// GET /apis/route.openshift.io request and checking the HTTP status code.
//
// Returns (true, nil) on a 200 response, (false, nil) on a 404, and
// (false, err) for any other outcome (network error, auth failure, etc.).
//
// This check is used at startup to decide whether to register OpenShift-specific
// schemes and resource managers.
func IsOpenShift(ctx context.Context, restCfg *rest.Config) (bool, error) {
	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return false, fmt.Errorf("k8s: building HTTP client for OpenShift probe: %w", err)
	}

	url := restCfg.Host + "/apis/route.openshift.io"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("k8s: building OpenShift probe request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("k8s: OpenShift probe request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("k8s: OpenShift probe returned unexpected status %d", resp.StatusCode)
	}
}

// CoreV1GroupVersion is a convenience reference to the core/v1 group version
// used when building ObjectLists that target core resources.
var CoreV1GroupVersion = corev1.SchemeGroupVersion

// FilterAnnotations returns a copy of the annotation map with internal
// Kubernetes annotations (kubectl.kubernetes.io/*) removed. Returns nil
// when the input is empty or all entries are filtered out.
func FilterAnnotations(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if len(k) >= 20 && k[:20] == "kubectl.kubernetes.i" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
