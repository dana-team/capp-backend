package middleware

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AuthorizationState contains request-local policy and sanitized decision metadata.
// Its mutable fields are not safe for concurrent use; snapshot checks are.
//
// Handlers must finish authorization and filtering before accessing dependent
// resources or writing a response. Reuse Targets for operations on the same cluster.
type AuthorizationState struct {
	Username  string
	Mode      string
	Targets   map[string]*AuthorizationTarget
	Namespace *corev1.Namespace
	Decisions []AuthorizationDecision
}

// AuthorizationTarget pairs a policy snapshot with the service client and configuration
// for resource operations. Handlers may use it only for operations authorized by the snapshot.
type AuthorizationTarget struct {
	Snapshot *authorization.Snapshot
	Client   client.Client
	Config   config.ClusterAuthorizationConfig
}

// AuthorizationDecision describes one check for request auditing. Outcome is
// allowed, denied, or unavailable; Reason is a sanitized authorization reason.
type AuthorizationDecision struct {
	Cluster       string
	Namespace     string
	Resource      string
	Action        string
	PolicyVersion string
	Outcome       string
	Reason        string
}

type authorizationRoute struct {
	method string
	path   string
}

type routePermission struct {
	resource string
	action   string
}

const (
	policyDenied       = "POLICY_DENIED"
	protectedNamespace = "PROTECTED_NAMESPACE"
	policyUnavailable  = "POLICY_UNAVAILABLE"
	policyConflict     = "POLICY_CONFLICT"
	invalidMembership  = "INVALID_MEMBERSHIP"
)

const (
	outcomeAllowed     = "allowed"
	outcomeDenied      = "denied"
	outcomeUnavailable = "unavailable"
)

var errProtectedNamespace = errors.New("namespace is protected")

// Authorization enforces application policy after authentication and cluster
// resolution. It leaves legacy modes unchanged and must also wrap cluster discovery.
//
// Configuration must be validated and remain immutable. The server owns each
// target store's lifecycle; requests never start or refresh stores.
func Authorization(cfg *config.Config, mgr cluster.ClusterManager, stores map[string]*authorization.Store) gin.HandlerFunc {
	stores = maps.Clone(stores)
	routes := authorizationRoutes()
	return func(c *gin.Context) {
		if cfg.Authorization.Mode == "" || cfg.Authorization.Mode == config.AuthorizationModeKubernetes {
			c.Next()
			return
		}
		if isPublicAuthorizationRoute(cfg, c.Request.Method, c.FullPath()) {
			c.Next()
			return
		}
		credential, _ := c.Get(string(CredentialKey))
		cred, ok := credential.(auth.ClusterCredential)
		state := &AuthorizationState{
			Username: cred.ImpersonateUser, Mode: cfg.Authorization.Mode,
			Targets: make(map[string]*AuthorizationTarget),
		}
		c.Set(string(AuthorizationKey), state)
		if !ok || strings.TrimSpace(cred.ImpersonateUser) == "" || cfg.Authorization.Mode != config.AuthorizationModeApplication {
			RespondAuthorization(c, errors.New("authorization context is unavailable"))
			return
		}
		permissions, known := routes[authorizationRoute{method: c.Request.Method, path: c.FullPath()}]
		if !known {
			RespondAuthorization(c, nil)
			return
		}
		if err := prepareAuthorization(c, cfg, mgr, stores, cred, state); err != nil {
			var apiErr *apierrors.APIError
			if errors.As(err, &apiErr) {
				apierrors.Respond(c, apiErr)
			} else {
				RespondAuthorization(c, err)
			}
			return
		}
		target := state.Targets[c.Param("cluster")]
		for _, permission := range permissions {
			var allowed bool
			var err error
			if state.Namespace == nil {
				allowed, err = target.Snapshot.CheckCluster(permission.resource, permission.action)
			} else {
				allowed, err = target.Snapshot.CheckNamespace(state.Namespace, permission.resource, permission.action)
			}
			state.RecordDecision(c.Param("cluster"), c.Param("namespace"), permission.resource, permission.action, allowed, err)
			if err != nil || !allowed {
				RespondAuthorization(c, err)
				return
			}
		}
		c.Next()
	}
}

// RecordDecision retains a check's result without retaining its error or policy data.
// Handlers use it for body-dependent and aggregate checks against the same snapshots.
func (s *AuthorizationState) RecordDecision(clusterName, namespace, resource, action string, allowed bool, err error) {
	decision := AuthorizationDecision{
		Cluster: clusterName, Namespace: namespace, Resource: resource, Action: action,
		Outcome: outcomeAllowed,
	}
	if target := s.Targets[clusterName]; target != nil && target.Snapshot != nil {
		decision.PolicyVersion = target.Snapshot.ResourceVersion()
	}
	if err != nil {
		decision.Outcome = outcomeUnavailable
		decision.Reason = policyUnavailable
	} else if !allowed {
		decision.Outcome = outcomeDenied
		decision.Reason = policyDenied
	}
	s.Decisions = append(s.Decisions, decision)
}

// RespondAuthorization aborts with a sanitized authorization error. A nil error
// means no grant; protected namespaces and membership errors retain their contracts.
// Other errors mean unavailable policy. Use apierrors.Respond for business and
// authentication errors.
func RespondAuthorization(c *gin.Context, err error) {
	reason := policyUnavailable
	switch {
	case err == nil:
		reason = policyDenied
	case errors.Is(err, errProtectedNamespace):
		reason = protectedNamespace
	case errors.Is(err, authorization.ErrMembershipConflict):
		reason = policyConflict
	case errors.Is(err, authorization.ErrInvalidMembership):
		reason = invalidMembership
	}
	respondAuthorizationReason(c, reason, c.Param("cluster"), c.Param("namespace"))
}

// RespondProtectedNamespace rejects a protected namespace and records its target.
// Pass the resolved cluster and namespace, including targets supplied in the body.
func RespondProtectedNamespace(c *gin.Context, clusterName, namespace string) {
	respondAuthorizationReason(c, protectedNamespace, clusterName, namespace)
}

func prepareAuthorization(c *gin.Context, cfg *config.Config, mgr cluster.ClusterManager, stores map[string]*authorization.Store, cred auth.ClusterCredential, state *AuthorizationState) error {
	clusterName := c.Param("cluster")
	for _, targetConfig := range cfg.Clusters {
		if clusterName != "" && targetConfig.Name != clusterName {
			continue
		}
		cc, err := mgr.Get(targetConfig.Name)
		if err != nil {
			state.RecordDecision(targetConfig.Name, c.Param("namespace"), c.FullPath(), c.Request.Method, false, err)
			return err
		}
		if namespace := c.Param("namespace"); namespace != "" {
			if namespace == targetConfig.Authorization.PolicyNamespace || slices.Contains(targetConfig.Authorization.ProtectedNamespaces, namespace) {
				return errProtectedNamespace
			}
			if !mgr.IsNamespaceAllowed(cc, namespace) {
				state.RecordDecision(targetConfig.Name, namespace, c.FullPath(), c.Request.Method, false, nil)
				return apierrors.NewNamespaceDenied(namespace, targetConfig.Name)
			}
		}
		serviceClient, err := mgr.ServiceClientFor(cc)
		if err != nil {
			state.RecordDecision(targetConfig.Name, c.Param("namespace"), c.FullPath(), c.Request.Method, false, err)
			return err
		}
		if serviceClient == nil {
			err := errors.New("authorization service client is unavailable")
			state.RecordDecision(targetConfig.Name, c.Param("namespace"), c.FullPath(), c.Request.Method, false, err)
			return err
		}
		store := stores[targetConfig.Name]
		if store == nil {
			state.RecordDecision(targetConfig.Name, c.Param("namespace"), c.FullPath(), c.Request.Method, false, authorization.ErrPolicyUnavailable)
			return authorization.ErrPolicyUnavailable
		}
		snapshot, err := store.Snapshot(cred)
		if err != nil {
			state.RecordDecision(targetConfig.Name, c.Param("namespace"), c.FullPath(), c.Request.Method, false, err)
			return err
		}
		state.Targets[targetConfig.Name] = &AuthorizationTarget{
			Snapshot: snapshot, Client: serviceClient, Config: targetConfig.Authorization,
		}
	}
	if clusterName == "" {
		return nil
	}
	target := state.Targets[clusterName]
	if target == nil {
		return apierrors.NewClusterNotFound(clusterName)
	}
	if namespace := c.Param("namespace"); namespace != "" {
		ns, err := target.Snapshot.Namespace(namespace)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return apierrors.NewNotFound("Namespace", namespace)
			}
			return err
		}
		state.Namespace = ns
	}
	return nil
}

func respondAuthorizationReason(c *gin.Context, reason, clusterName, namespace string) {
	apiErr := &apierrors.APIError{Details: map[string]any{"reason": reason}}
	switch reason {
	case policyDenied:
		apiErr.Status, apiErr.Code, apiErr.Message = http.StatusForbidden, apierrors.CodeForbidden, "operation is not permitted"
	case protectedNamespace:
		apiErr.Status, apiErr.Code, apiErr.Message = http.StatusForbidden, apierrors.CodeNamespaceDenied, "namespace is not accessible"
	case policyConflict:
		apiErr.Status, apiErr.Code, apiErr.Message = http.StatusConflict, apierrors.CodeConflict, "membership changed; reload and retry"
	case invalidMembership:
		apiErr.Status, apiErr.Code, apiErr.Message = http.StatusBadRequest, apierrors.CodeBadRequest, "invalid namespace membership"
	default:
		apiErr.Status, apiErr.Code, apiErr.Message = http.StatusServiceUnavailable, apierrors.CodeInternal, "authorization policy is unavailable"
	}
	apierrors.Respond(c, apiErr)
	value, _ := c.Get(string(AuthorizationKey))
	state, ok := value.(*AuthorizationState)
	if !ok {
		return
	}
	// Preserve the action and target when a check already recorded this failure.
	if n := len(state.Decisions); n > 0 && state.Decisions[n-1].Reason == reason {
		return
	}
	state.RecordDecision(clusterName, namespace, c.FullPath(), c.Request.Method, false, nil)
	last := &state.Decisions[len(state.Decisions)-1]
	last.Reason = reason
	if apiErr.Status >= http.StatusInternalServerError {
		last.Outcome = outcomeUnavailable
	}
}

// An empty permission list delegates filtering or body-dependent checks to the
// handler; it never exempts the route from capturing valid policy.
func authorizationRoutes() map[authorizationRoute][]routePermission {
	const clusters = "/api/v1/clusters"
	const target = clusters + "/:cluster"
	const namespaces = target + "/namespaces"
	const namespace = namespaces + "/:namespace"
	const capp = namespace + "/capps/:name"

	routes := map[authorizationRoute][]routePermission{
		{method: http.MethodGet, path: clusters}:                      nil,
		{method: http.MethodGet, path: target}:                        nil,
		{method: http.MethodGet, path: namespaces}:                    nil,
		{method: http.MethodPost, path: namespaces}:                   {{resource: authorization.ResourceNamespaces, action: authorization.ActionCreate}},
		{method: http.MethodGet, path: namespace}:                     {{resource: authorization.ResourceNamespaces, action: authorization.ActionGet}},
		{method: http.MethodPut, path: namespace}:                     {{resource: authorization.ResourceNamespaces, action: authorization.ActionUpdate}},
		{method: http.MethodPatch, path: namespace}:                   {{resource: authorization.ResourceCappUserRole, action: authorization.ActionGrant}},
		{method: http.MethodDelete, path: namespace}:                  {{resource: authorization.ResourceNamespaces, action: authorization.ActionDelete}},
		{method: http.MethodGet, path: capp + "/revisions"}:           {{resource: authorization.ResourceCappRevisions, action: authorization.ActionList}},
		{method: http.MethodGet, path: capp + "/revisions/:revision"}: {{resource: authorization.ResourceCappRevisions, action: authorization.ActionGet}},
		{method: http.MethodPost, path: capp + "/revisions/:revision/rollback"}: {
			{resource: authorization.ResourceCappRevisions, action: authorization.ActionGet},
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
			{resource: authorization.ResourceCapps, action: authorization.ActionUpdate},
		},
		{method: http.MethodPost, path: capp + "/sync"}: {
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
			{resource: authorization.ResourceCapps, action: authorization.ActionSync},
		},
		{method: http.MethodDelete, path: capp + "/sync"}: {
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
			{resource: authorization.ResourceCapps, action: authorization.ActionUnsync},
		},
		{method: http.MethodGet, path: capp + "/benchmark"}: {
			{resource: authorization.ResourceBenchmarks, action: authorization.ActionGet},
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
		},
		{method: http.MethodPost, path: capp + "/benchmark"}: {
			{resource: authorization.ResourceBenchmarks, action: authorization.ActionCreate},
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
		},
		{method: http.MethodPost, path: capp + "/migrate"}: {
			{resource: authorization.ResourceCapps, action: authorization.ActionMigrate},
			{resource: authorization.ResourceCapps, action: authorization.ActionGet},
			{resource: authorization.ResourceSecrets, action: authorization.ActionList},
			{resource: authorization.ResourceConfigMaps, action: authorization.ActionList},
		},
	}
	for _, resource := range []string{authorization.ResourceCapps, authorization.ResourceSecrets, authorization.ResourceConfigMaps} {
		collection := namespace + "/" + resource
		routes[authorizationRoute{method: http.MethodGet, path: target + "/" + resource}] = nil
		routes[authorizationRoute{method: http.MethodGet, path: collection}] = []routePermission{{resource: resource, action: authorization.ActionList}}
		routes[authorizationRoute{method: http.MethodPost, path: collection}] = []routePermission{{resource: resource, action: authorization.ActionCreate}}
		routes[authorizationRoute{method: http.MethodGet, path: collection + "/:name"}] = []routePermission{{resource: resource, action: authorization.ActionGet}}
		routes[authorizationRoute{method: http.MethodPut, path: collection + "/:name"}] = []routePermission{{resource: resource, action: authorization.ActionUpdate}}
		routes[authorizationRoute{method: http.MethodDelete, path: collection + "/:name"}] = []routePermission{{resource: resource, action: authorization.ActionDelete}}
		if resource != authorization.ResourceCapps {
			routes[authorizationRoute{method: http.MethodGet, path: collection + "/names"}] = []routePermission{{resource: resource, action: authorization.ActionList}}
		}
	}
	return routes
}

func isPublicAuthorizationRoute(cfg *config.Config, method, path string) bool {
	if method == http.MethodGet {
		switch path {
		case "/healthz", "/readyz", "/docs", "/ui/scalar.js", "/openapi.yaml", "/api/v1/sizes", "/api/v1/auth/mode", "/api/v1/auth/openshift/authorize":
			return true
		default:
			return cfg.Metrics.Enabled && path != "" && path == cfg.Metrics.Path
		}
	}
	if method == http.MethodPost {
		switch path {
		case "/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/openshift/callback":
			return true
		}
	}
	return false
}
