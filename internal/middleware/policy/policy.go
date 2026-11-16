// Package policy enforces backend API authorization before resource execution.
package policy

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type contextKey string

const (
	backendPolicyKey  contextKey = "backendPolicy"
	policyDecisionKey contextKey = "policyDecision"
	policyDeniedKey   contextKey = "policyDenied"
)

const (
	auditOutcomeDenied     = "denied"
	auditOutcomeAllowed    = "allowed"
	auditAuthorityOperator = "operator"
	auditAuthorityPolicy   = "policy"
	auditAuthorityDeferred = "deferred"
	auditAuthorityNone     = "none"
)

const reasonPolicyUnavailable = "POLICY_UNAVAILABLE"

// Target holds a cluster's observed policy and backend execution client.
// The caller owns their lifecycle; neither may use end-user impersonation.
type Target struct {
	Store           *authorization.Store
	ExecutionClient client.Client
	OperatorUsers   []string
	OperatorGroups  []string
}

type policyDecision struct {
	identity                          auth.ClusterCredential
	authenticate                      func(string) (auth.ClusterCredential, error)
	request                           *http.Request
	method, route, cluster, namespace string
	category                          Category
	state                             authorization.NamespaceState
	operator                          bool
	namespaceAllowed                  func(string) bool
	discover                          func(string) ([]authorization.NamespaceState, bool, error)
	aggregate                         func(*gin.Context, client.ObjectList, ...client.ListOption) error
	prepareMigration                  func(string, string) (client.Client, client.Client, error)
}

// New returns middleware that authenticates and prepares requests using the fixed API rules.
// Install globally after CORS, with Logging and Recovery, without
// Kubernetes-mode Auth/Cluster middleware. Discovery, aggregate and migration consumers
// must use their category-specific authorization before accessing resources.
func New(authMgr auth.AuthManager, authMode string, clusterMgr cluster.ClusterManager, routes *Routes, targets map[string]Target, logger *zap.Logger) gin.HandlerFunc {
	sharedIdentity := authMode == config.AuthModeLDAP || authMode == config.AuthModeOpenShift
	targets = maps.Clone(targets)
	for name, target := range targets {
		target.OperatorUsers = slices.Clone(target.OperatorUsers)
		target.OperatorGroups = slices.Clone(target.OperatorGroups)
		targets[name] = target
	}
	return func(c *gin.Context) {
		c.Set(string(backendPolicyKey), true)
		delete(c.Keys, string(middleware.K8sClientKey))
		delete(c.Keys, string(middleware.AdminK8sClientKey))
		delete(c.Keys, string(policyDecisionKey))
		delete(c.Keys, string(policyDeniedKey))
		delete(c.Keys, string(middleware.CredentialKey))
		delete(c.Keys, string(middleware.ClusterMetaKey))
		if c.FullPath() == "" {
			// Preserve the router's 404/405 without running fallback handlers.
			c.Abort()
			return
		}
		rule, _ := routes.Lookup(c.Request.Method, c.FullPath())
		if rule.Category == Public {
			c.Next()
			return
		}

		var caller string
		decision := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), category: rule.Category, cluster: c.Param("cluster"), namespace: c.Param("namespace")}
		outcome := auditOutcomeDenied
		defer func() {
			if c.GetBool(string(policyDeniedKey)) {
				outcome = auditOutcomeDenied
			}
			auditPolicy(logger, caller, decision, outcome)
		}()
		identities, ok := decision.prepareAuthentication(c, authMgr, clusterMgr, targets, sharedIdentity)
		if !ok {
			return
		}
		cred := decision.identity
		caller = cred.ImpersonateUser
		clusterName := decision.cluster
		if rule.Category == Denied {
			respondPolicyError(c, authorization.ErrDenied)
			return
		}

		if clusterName == "" && !decision.isGlobalDiscovery() {
			respondPolicyError(c, authorization.ErrDenied)
			return
		}

		target := targets[clusterName]
		decision.operator = target.isOperator(caller, cred.ImpersonateGroups)
		if rule.Category == Administration && !decision.operator {
			respondPolicyError(c, authorization.ErrDenied)
			return
		}
		if clusterName != "" {
			cc := preparePolicyCluster(c, clusterMgr)
			if cc == nil {
				return
			}
			decision.namespaceAllowed = func(namespace string) bool {
				return clusterMgr.IsNamespaceAllowed(cc, namespace)
			}
		}

		switch rule.Category {
		case Administration:
			decision.namespace = c.Param("namespace")
			if target.Store == nil {
				respondPolicyError(c, authorization.ErrUnavailable)
				return
			}
			if decision.namespace != "" || c.Request.Method != http.MethodPost {
				if err := target.Store.CheckTarget(decision.namespace); err != nil {
					respondPolicyError(c, err)
					return
				}
			}
		case Namespace, Migration:
			namespace := c.Param("namespace")
			if len(validation.ValidateNamespaceName(namespace, false)) != 0 {
				respondPolicyError(c, authorization.ErrDenied)
				return
			}
			decision.namespace = namespace
			if target.Store == nil {
				respondPolicyError(c, authorization.ErrUnavailable)
				return
			}

			namespaceDetail := decision.isNamespaceDetail()
			state, err := target.authorizeNamespace(namespace, caller, cred.ImpersonateGroups, rule.AllowedRoles, namespaceDetail && decision.operator)
			decision.state = state
			if err != nil {
				respondPolicyError(c, err)
				return
			}
			switch {
			case rule.Category == Migration:
				decision.prepareMigration = func(clusterName, namespace string) (client.Client, client.Client, error) {
					return migrationClients(clusterMgr, targets, rule.AllowedRoles, decision, clusterName, namespace, logger)
				}
			case !namespaceDetail:
				if target.ExecutionClient == nil {
					respondPolicyError(c, authorization.ErrUnavailable)
					return
				}
				c.Set(string(middleware.K8sClientKey), client.NewNamespacedClient(target.ExecutionClient, namespace))
			}

		case Discovery:
			decision.discover = func(name string) ([]authorization.NamespaceState, bool, error) {
				return identities.discover(clusterMgr, targets[name], rule.AllowedRoles, name, decision, logger)
			}
		case Aggregate:
			decision.aggregate = func(c *gin.Context, list client.ObjectList, options ...client.ListOption) error {
				return listAggregate(c, list, target, cred, rule.AllowedRoles, decision, logger, options...)
			}
		default:
			respondPolicyError(c, authorization.ErrDenied)
			return
		}
		c.Set(string(policyDecisionKey), decision)
		outcome = auditOutcomeAllowed
		c.Next()
	}
}

func (d policyDecision) isGlobalDiscovery() bool {
	return d.cluster == "" && d.category == Discovery && d.route == "/api/v1/clusters"
}

func (d *policyDecision) prepareAuthentication(c *gin.Context, mgr auth.AuthManager, clusters cluster.ClusterManager, targets map[string]Target, sharedIdentity bool) (*requestAuthentication, bool) {
	identities := &requestAuthentication{AuthManager: mgr, sharedIdentity: sharedIdentity, credentials: make(map[string]auth.ClusterCredential)}
	// Targetless protected routes still require verification before denial.
	// Only shared providers have one identity representing these requests.
	if d.cluster == "" {
		identities.targets = make([]string, 0, len(targets))
		for _, meta := range clusters.List() {
			identities.targets = append(identities.targets, meta.Name)
		}
		slices.Sort(identities.targets)
	}
	credential, ok := middleware.Authenticate(c, identities)
	if !ok {
		return nil, false
	}
	if d.cluster != "" && credential.ImpersonateUser == "" {
		apierrors.Respond(c, apierrors.NewUnauthorized(auth.ErrUnauthenticated.Error()))
		return nil, false
	}
	if d.cluster == "" && !sharedIdentity {
		delete(c.Keys, string(middleware.CredentialKey))
	}
	d.identity = credential
	request := c.Request
	d.authenticate = func(name string) (auth.ClusterCredential, error) {
		return identities.Authenticate(request.Context(), name, request)
	}
	return identities, true
}

// requestAuthentication retains identities only for the current HTTP request.
// Discovery workers write separate slots and join before publishing the map.
type requestAuthentication struct {
	auth.AuthManager
	sharedIdentity bool
	targets        []string
	credentials    map[string]auth.ClusterCredential
}

func (a *requestAuthentication) Authenticate(ctx context.Context, name string, r *http.Request) (auth.ClusterCredential, error) {
	if a.targets != nil {
		if err := a.authenticateTargets(ctx, r); err != nil {
			return auth.ClusterCredential{}, err
		}
		if a.sharedIdentity {
			return a.credentials[a.targets[0]], nil
		}
		return auth.ClusterCredential{}, nil
	}
	if credential, ok := a.credentials[name]; ok {
		return credential, nil
	}
	for _, credential := range a.credentials {
		if credential.AuthProvider == config.AuthModeLDAP || credential.AuthProvider == config.AuthModeOpenShift {
			a.credentials[name] = credential
			return credential, nil
		}
	}
	credential, err := a.AuthManager.Authenticate(ctx, name, r)
	if err == nil {
		a.credentials[name] = credential
	}
	return credential, err
}

func (a *requestAuthentication) authenticateTargets(ctx context.Context, r *http.Request) error {
	if len(a.targets) == 0 {
		return auth.ErrUnauthenticated
	}
	// Shared providers need one verified identity, even when the home reviewer
	// becomes unavailable during discovery. Passthrough continues per target.
	first, err := a.AuthManager.Authenticate(ctx, a.targets[0], r)
	if err != nil && !errors.Is(err, auth.ErrUnauthenticated) {
		return err
	}
	if ctx.Err() != nil {
		return auth.ErrIdentityUnavailable
	}
	if a.sharedIdentity {
		if err != nil {
			return err
		}
		for _, name := range a.targets {
			a.credentials[name] = first
		}
		return nil
	}
	group, reviewCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	credentials := make([]auth.ClusterCredential, len(a.targets))
	if err == nil {
		credentials[0] = first
	}
	for i, name := range a.targets[1:] {
		group.Go(func() error {
			if reviewCtx.Err() != nil {
				return auth.ErrIdentityUnavailable
			}
			credential, err := a.AuthManager.Authenticate(reviewCtx, name, r)
			if errors.Is(err, auth.ErrUnauthenticated) {
				return nil
			}
			if err != nil {
				return err
			}
			credentials[i+1] = credential
			return nil
		})
		if reviewCtx.Err() != nil {
			break
		}
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return auth.ErrIdentityUnavailable
	}
	for i, credential := range credentials {
		if credential.ImpersonateUser != "" {
			a.credentials[a.targets[i]] = credential
		}
	}
	if len(a.credentials) == 0 {
		return auth.ErrUnauthenticated
	}
	return nil
}

func (a *requestAuthentication) discover(mgr cluster.ClusterManager, target Target, roles []string, name string, decision policyDecision, logger *zap.Logger) ([]authorization.NamespaceState, bool, error) {
	verified, ok := a.credentials[name]
	if !ok {
		return nil, false, nil
	}
	states, operator, err := discoverNamespaces(mgr, target, verified, roles, name)
	if decision.isGlobalDiscovery() {
		decision.cluster, decision.identity, decision.operator = name, verified, operator
		outcome := auditOutcomeDenied
		if err == nil {
			outcome = auditOutcomeAllowed
		}
		auditPolicy(logger, verified.ImpersonateUser, decision, outcome)
	}
	return states, operator, err
}

// RequireExecution validates the request-local namespace decision before a
// resource handler consumes its execution client. Kubernetes-mode requests pass through.
func RequireExecution(c *gin.Context) bool {
	if !c.GetBool(string(backendPolicyKey)) {
		return true
	}
	decision, ok := requestPolicyDecision(c)
	if !ok || decision.category != Namespace || decision.route == namespacePath {
		respondPolicyError(c, authorization.ErrDenied)
		return false
	}
	return true
}

func discoverNamespaces(mgr cluster.ClusterManager, target Target, cred auth.ClusterCredential, roles []string, name string) ([]authorization.NamespaceState, bool, error) {
	cc, err := mgr.Get(name)
	if err != nil {
		return nil, false, authorization.ErrUnavailable
	}
	if target.isOperator(cred.ImpersonateUser, cred.ImpersonateGroups) {
		return nil, true, nil
	}
	if target.Store == nil {
		return nil, false, authorization.ErrUnavailable
	}
	states, synced := target.Store.Eligible(cred.ImpersonateUser, cred.ImpersonateGroups, roles)
	if !synced {
		return nil, false, authorization.ErrUnavailable
	}
	states = slices.DeleteFunc(states, func(state authorization.NamespaceState) bool {
		return !mgr.IsNamespaceAllowed(cc, state.Namespace.Name)
	})
	return states, false, nil
}

func preparePolicyCluster(c *gin.Context, mgr cluster.ClusterManager) *cluster.ClusterClient {
	name := c.Param("cluster")
	cc, err := mgr.Get(name)
	if err != nil {
		if errors.Is(err, cluster.ErrClusterNotFound) {
			apierrors.Respond(c, apierrors.NewClusterNotFound(name))
			return nil
		}
		apierrors.Respond(c, apierrors.NewInternal(err))
		return nil
	}
	// Cluster metadata discovery may expose health; resource routes require it.
	if c.FullPath() != clusterPath && !cc.IsHealthy() {
		apierrors.Respond(c, apierrors.NewClusterUnhealthy(name))
		return nil
	}
	if namespace := c.Param("namespace"); namespace != "" && !mgr.IsNamespaceAllowed(cc, namespace) {
		respondPolicyError(c, authorization.ErrDenied)
		return nil
	}
	meta := cc.Meta
	meta.Healthy = cc.IsHealthy()
	c.Set(string(middleware.ClusterMetaKey), meta)
	return cc
}

func respondPolicyOperationError(c *gin.Context, err error) {
	if errors.Is(err, authorization.ErrDenied) || errors.Is(err, authorization.ErrUnavailable) {
		respondPolicyError(c, err)
		return
	}
	apierrors.Respond(c, err)
}

func respondPolicyError(c *gin.Context, err error) {
	c.Set(string(policyDeniedKey), true)
	response := &apierrors.APIError{
		Code: apierrors.CodeForbidden, Status: http.StatusForbidden,
		Message: authorization.ErrDenied.Error(), Details: map[string]any{"reason": apierrors.ReasonPolicyDenied},
	}
	if errors.Is(err, authorization.ErrUnavailable) {
		response.Code, response.Status, response.Message = apierrors.CodeInternal, http.StatusServiceUnavailable, authorization.ErrUnavailable.Error()
		response.Details["reason"] = reasonPolicyUnavailable
	}
	apierrors.Respond(c, response)
}

func auditPolicy(logger *zap.Logger, caller string, decision policyDecision, outcome string) {
	authority := auditAuthorityNone
	if outcome == auditOutcomeAllowed {
		switch decision.category {
		case Administration:
			authority = auditAuthorityOperator
		case Namespace, Migration:
			authority = auditAuthorityPolicy
			if decision.operator && decision.isNamespaceDetail() {
				authority = auditAuthorityOperator
			}
		case Discovery, Aggregate:
			authority = auditAuthorityDeferred
		}
	}
	logger.Info("authorization decision",
		zap.String("authProvider", decision.identity.AuthProvider), zap.String("identityAuthority", decision.identity.IdentityAuthority),
		zap.String("caller", caller), zap.String("method", decision.method), zap.String("route", decision.route),
		zap.String("cluster", decision.cluster), zap.String("namespace", decision.namespace),
		zap.String("outcome", outcome), zap.Bool("operator", decision.operator), zap.String("authority", authority),
		zap.String("policyUID", string(decision.state.PolicyUID)), zap.String("policyVersion", decision.state.PolicyResourceVersion),
	)
}

func (d policyDecision) isNamespaceDetail() bool {
	return d.method == http.MethodGet && d.route == namespacePath
}

func (t Target) authorizeNamespace(namespace, caller string, groups, roles []string, operatorRead bool) (authorization.NamespaceState, error) {
	if operatorRead {
		return authorization.NamespaceState{}, t.Store.CheckTarget(namespace)
	}
	return t.Store.Authorize(namespace, caller, groups, roles)
}

func (t Target) isOperator(username string, groups []string) bool {
	if username == "" {
		return false
	}
	if slices.Contains(t.OperatorUsers, username) {
		return true
	}
	for _, group := range groups {
		if group != "" && slices.Contains(t.OperatorGroups, group) {
			return true
		}
	}
	return false
}

// RequireNamespaceAccess verifies the prepared namespace administration, detail,
// or discovery decision. It returns operator authority and the read observation.
// A missing decision never falls back to Kubernetes-mode authorization.
func RequireNamespaceAccess(c *gin.Context) (bool, authorization.NamespaceState, bool) {
	decision, ok := requestPolicyDecision(c)
	validCategory := decision.category == Administration ||
		(decision.category == Namespace && decision.isNamespaceDetail()) ||
		(decision.category == Discovery && c.FullPath() == namespaceCollectionPath)
	if !c.GetBool(string(backendPolicyKey)) || !ok || !validCategory || (decision.category == Administration && !decision.operator) {
		respondPolicyError(c, authorization.ErrDenied)
		return false, authorization.NamespaceState{}, false
	}
	return decision.operator, decision.state, true
}

// RequireDiscovery returns eligible namespace observations or operator authority
// for a discovery request. Scoped requests cannot inspect another cluster.
// Operator authority permits administrative discovery, not resource access.
// An empty cluster name only validates a global discovery request.
// Global discovery omits targets with unavailable policy observations; scoped
// discovery reports that unavailability. Identity verification precedes both.
func RequireDiscovery(c *gin.Context, clusterName string) ([]authorization.NamespaceState, bool, bool) {
	if c.Request.Context().Err() != nil {
		c.Set(string(policyDeniedKey), true)
		apierrors.Respond(c, &apierrors.APIError{Code: apierrors.CodeInternal, Status: http.StatusServiceUnavailable, Message: auth.ErrIdentityUnavailable.Error()})
		return nil, false, false
	}
	decision, ok := requestPolicyDecision(c)
	if !Enabled(c) || !ok || decision.category != Discovery || decision.discover == nil ||
		(decision.cluster != "" && decision.cluster != clusterName) {
		respondPolicyError(c, authorization.ErrDenied)
		return nil, false, false
	}
	if clusterName == "" {
		return nil, false, true
	}
	states, operator, err := decision.discover(clusterName)
	if decision.cluster == "" && errors.Is(err, authorization.ErrUnavailable) {
		return nil, false, true
	}
	if err != nil {
		respondPolicyError(c, err)
		return nil, false, false
	}
	return states, operator, true
}

// RequireNamespaceAllowed checks a body-supplied namespace against the prepared
// cluster's configured boundary. It does not grant resource or operator authority.
func RequireNamespaceAllowed(c *gin.Context, namespace string) bool {
	if !NamespaceAllowed(c, namespace) {
		respondPolicyError(c, authorization.ErrDenied)
		return false
	}
	return true
}

// NamespaceAllowed checks the prepared cluster boundary without writing a
// response, so discovery can omit excluded namespaces before reading their data.
// It does not grant resource or operator authority.
func NamespaceAllowed(c *gin.Context, namespace string) bool {
	decision, ok := requestPolicyDecision(c)
	return ok && decision.namespaceAllowed != nil && decision.namespaceAllowed(namespace)
}

func requestPolicyDecision(c *gin.Context) (policyDecision, bool) {
	value, _ := c.Get(string(policyDecisionKey))
	decision, ok := value.(policyDecision)
	return decision, ok && decision.request == c.Request && decision.method == c.Request.Method &&
		decision.route == c.FullPath() && decision.cluster == c.Param("cluster") && decision.namespace == c.Param("namespace")
}

// Enabled reports whether the global policy chain selected this mode.
func Enabled(c *gin.Context) bool { return c.GetBool(string(backendPolicyKey)) }
