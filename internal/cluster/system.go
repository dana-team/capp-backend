package cluster

import (
	"errors"
	"fmt"
	"net/http"

	"k8s.io/client-go/kubernetes"
	authenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SystemClients holds reusable clients using the backend's cluster credential.
// Before handling user requests for namespaced resources, authorize access
// and wrap the resource client with namespace scoping.
type SystemClients struct {
	Kubernetes kubernetes.Interface
	Resources  client.Client
	// Reviewer has independent limits for per-request identity verification.
	Reviewer authenticationv1.AuthenticationV1Interface
}

// NewSystemClients builds clients from cluster credentials without impersonation.
// The caller owns their lifecycle; request credentials are never accepted here.
func NewSystemClients(cluster *ClusterClient) (*SystemClients, error) {
	if cluster == nil || cluster.RestConfig == nil || cluster.Scheme == nil {
		return nil, errors.New("system clients require cluster configuration and scheme")
	}
	cfg := rest.CopyConfig(cluster.RestConfig)
	cfg.Impersonate = rest.ImpersonationConfig{}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building system transport: %w", err)
	}
	// Keep redirect policy local even when HTTPClientFor returns the default client.
	copyClient := *httpClient
	httpClient = &copyClient
	// Reviews carry the caller token in their body; neither credential may
	// follow a redirect, including a redirect to another configured target.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	typed, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building system Kubernetes client: %w", err)
	}
	const (
		reviewerQPS   = 50
		reviewerBurst = 100
	)
	reviewerCfg := rest.CopyConfig(cfg)
	// Keep review traffic independent of resource traffic, including when the
	// base configuration supplies a shared limiter.
	reviewerCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(reviewerQPS, reviewerBurst)
	reviewer, err := authenticationv1.NewForConfigAndClient(reviewerCfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building system token reviewer: %w", err)
	}
	resources, err := client.New(cfg, client.Options{Scheme: cluster.Scheme, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("building system resource client: %w", err)
	}
	return &SystemClients{Kubernetes: typed, Resources: resources, Reviewer: reviewer}, nil
}
