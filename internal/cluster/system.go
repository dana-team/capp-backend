package cluster

import (
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SystemClients holds reusable clients using the backend's cluster credential.
// Before handling user requests for namespaced resources, authorize access
// and wrap the resource client with namespace scoping.
type SystemClients struct {
	Kubernetes kubernetes.Interface
	Resources  client.Client
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
	typed, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building system Kubernetes client: %w", err)
	}
	resources, err := client.New(cfg, client.Options{Scheme: cluster.Scheme, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("building system resource client: %w", err)
	}
	return &SystemClients{Kubernetes: typed, Resources: resources}, nil
}
