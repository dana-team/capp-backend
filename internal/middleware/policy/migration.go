package policy

import (
	"errors"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrepareMigration authorizes the destination of a prepared migration request.
// It returns namespace-bound execution clients only after both endpoints pass.
// On failure it writes the HTTP error and returns false.
func PrepareMigration(c *gin.Context, clusterName, namespace string) (client.Client, client.Client, bool) {
	decision, ok := requestPolicyDecision(c)
	if !Enabled(c) || !ok || decision.category != Migration || decision.prepareMigration == nil {
		respondPolicyError(c, authorization.ErrDenied)
		return nil, nil, false
	}
	if clusterName == "" || len(validation.ValidateNamespaceName(namespace, false)) != 0 {
		apierrors.Respond(c, apierrors.NewBadRequest("invalid migration destination"))
		return nil, nil, false
	}
	if clusterName == decision.cluster && namespace == decision.namespace {
		apierrors.Respond(c, apierrors.NewBadRequest("cannot migrate to the same cluster and namespace"))
		return nil, nil, false
	}
	source, destination, err := decision.prepareMigration(clusterName, namespace)
	if err != nil {
		respondPolicyOperationError(c, err)
		return nil, nil, false
	}
	return source, destination, true
}

func migrationClients(mgr cluster.ClusterManager, targets map[string]Target, cred auth.ClusterCredential, roles []string, source policyDecision, clusterName, namespace string, logger *zap.Logger) (client.Client, client.Client, error) {
	destination := source
	destination.cluster, destination.namespace = clusterName, namespace
	destination.state = authorization.NamespaceState{}
	outcome := auditOutcomeDenied
	defer func() { auditPolicy(logger, cred.ImpersonateUser, destination, outcome) }()
	// Until destination authorization succeeds, conceal existence and readiness
	// behind the same denial, including when policy observations are unavailable.
	target := targets[clusterName]
	if target.Store == nil {
		return nil, nil, authorization.ErrDenied
	}
	var err error
	destination.state, err = target.Store.Authorize(namespace, cred.ImpersonateUser, cred.ImpersonateGroups, roles)
	if err != nil {
		return nil, nil, authorization.ErrDenied
	}
	cc, err := mgr.Get(clusterName)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) {
			return nil, nil, authorization.ErrDenied
		}
		return nil, nil, apierrors.NewInternal(err)
	}
	if !mgr.IsNamespaceAllowed(cc, namespace) {
		return nil, nil, authorization.ErrDenied
	}
	if !cc.IsHealthy() {
		return nil, nil, apierrors.NewClusterUnhealthy(clusterName)
	}
	sourceClient := targets[source.cluster].ExecutionClient
	if sourceClient == nil || target.ExecutionClient == nil {
		return nil, nil, authorization.ErrUnavailable
	}
	outcome = auditOutcomeAllowed
	return client.NewNamespacedClient(sourceClient, source.namespace), client.NewNamespacedClient(target.ExecutionClient, destination.state.Namespace.Name), nil
}
