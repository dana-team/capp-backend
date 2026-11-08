package middleware

import (
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// PolicyTarget holds a cluster's observed policy and backend execution client.
// The caller owns their lifecycle; neither may use end-user impersonation.
type PolicyTarget struct {
	Store           *authorization.Store
	ExecutionClient client.Client
	OperatorUsers   []string
	OperatorGroups  []string
}

type policyDecision struct {
	request                           *http.Request
	method, route, cluster, namespace string
	category                          PolicyCategory
	state                             authorization.NamespaceState
	operator                          bool
	namespaceAllowed                  func(string) bool
}

// BackendPolicy authenticates and prepares requests using the fixed API rules.
// Install globally after CORS, with Logging and Recovery, without
// legacy Auth/Cluster middleware. Discovery, aggregate and migration consumers
// must use their category-specific authorization before accessing resources.
func BackendPolicy(authMgr auth.AuthManager, clusterMgr cluster.ClusterManager, routes *PolicyRoutes, targets map[string]PolicyTarget, logger *zap.Logger) gin.HandlerFunc {
	targets = maps.Clone(targets)
	for name, target := range targets {
		target.OperatorUsers = slices.Clone(target.OperatorUsers)
		target.OperatorGroups = slices.Clone(target.OperatorGroups)
		targets[name] = target
	}
	return func(c *gin.Context) {
		c.Set(string(backendPolicyKey), true)
		delete(c.Keys, string(K8sClientKey))
		delete(c.Keys, string(AdminK8sClientKey))
		delete(c.Keys, string(policyDecisionKey))
		delete(c.Keys, string(policyDeniedKey))
		delete(c.Keys, string(CredentialKey))
		delete(c.Keys, string(ClusterMetaKey))
		if c.FullPath() == "" {
			// Preserve the router's 404/405 without running fallback handlers.
			c.Abort()
			return
		}
		rule, _ := routes.Lookup(c.Request.Method, c.FullPath())
		if rule.Category == PolicyPublic {
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
		cred, ok := authenticate(c, authMgr)
		if !ok {
			return
		}
		if cred.ImpersonateUser == "" {
			apierrors.Respond(c, apierrors.NewUnauthorized("authentication required"))
			return
		}
		caller = cred.ImpersonateUser
		if rule.Category == PolicyDenied {
			respondPolicyError(c, authorization.ErrDenied)
			return
		}

		clusterName := c.Param("cluster")
		if clusterName == "" && (rule.Category != PolicyDiscovery || c.FullPath() != "/api/v1/clusters") {
			respondPolicyError(c, authorization.ErrDenied)
			return
		}

		target := targets[clusterName]
		decision.operator = target.isOperator(caller, cred.ImpersonateGroups)
		if rule.Category == PolicyAdministration && !decision.operator {
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
		case PolicyAdministration:
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
		case PolicyNamespace, PolicyMigration:
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
			if rule.Category == PolicyNamespace && !namespaceDetail {
				if target.ExecutionClient == nil {
					respondPolicyError(c, authorization.ErrUnavailable)
					return
				}
				c.Set(string(K8sClientKey), client.NewNamespacedClient(target.ExecutionClient, namespace))
			}

		case PolicyDiscovery, PolicyAggregate:
			// These categories prepare identity/target only. Their consumers must
			// filter each namespace before exposing data or using an execution client.
		default:
			respondPolicyError(c, authorization.ErrDenied)
			return
		}
		c.Set(string(policyDecisionKey), decision)
		outcome = auditOutcomeAllowed
		c.Next()
	}
}

// RequireExecution validates the request-local namespace decision before a
// resource handler consumes its execution client. Legacy requests pass through.
func RequireExecution(c *gin.Context) bool {
	if !c.GetBool(string(backendPolicyKey)) {
		return true
	}
	decision, ok := requestPolicyDecision(c)
	if !ok || decision.category != PolicyNamespace || decision.route == namespacePath {
		respondPolicyError(c, authorization.ErrDenied)
		return false
	}
	return true
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
	c.Set(string(ClusterMetaKey), meta)
	return cc
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
		case PolicyAdministration:
			authority = auditAuthorityOperator
		case PolicyNamespace, PolicyMigration:
			authority = auditAuthorityPolicy
			if decision.operator && decision.isNamespaceDetail() {
				authority = auditAuthorityOperator
			}
		case PolicyDiscovery, PolicyAggregate:
			authority = auditAuthorityDeferred
		}
	}
	logger.Info("authorization decision",
		zap.String("caller", caller), zap.String("method", decision.method), zap.String("route", decision.route),
		zap.String("cluster", decision.cluster), zap.String("namespace", decision.namespace),
		zap.String("outcome", outcome), zap.Bool("operator", decision.operator), zap.String("authority", authority),
		zap.String("policyUID", string(decision.state.PolicyUID)), zap.String("policyVersion", decision.state.PolicyResourceVersion),
	)
}

func (d policyDecision) isNamespaceDetail() bool {
	return d.method == http.MethodGet && d.route == namespacePath
}

func (t PolicyTarget) authorizeNamespace(namespace, caller string, groups, roles []string, operatorRead bool) (authorization.NamespaceState, error) {
	if operatorRead {
		return authorization.NamespaceState{}, t.Store.CheckTarget(namespace)
	}
	return t.Store.Authorize(namespace, caller, groups, roles)
}

func (t PolicyTarget) isOperator(username string, groups []string) bool {
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
// A missing decision never falls back to legacy authorization.
func RequireNamespaceAccess(c *gin.Context) (bool, authorization.NamespaceState, bool) {
	decision, ok := requestPolicyDecision(c)
	validCategory := decision.category == PolicyAdministration ||
		(decision.category == PolicyNamespace && decision.isNamespaceDetail()) ||
		(decision.category == PolicyDiscovery && c.FullPath() == namespaceCollectionPath)
	if !c.GetBool(string(backendPolicyKey)) || !ok || !validCategory || (decision.category == PolicyAdministration && !decision.operator) {
		respondPolicyError(c, authorization.ErrDenied)
		return false, authorization.NamespaceState{}, false
	}
	return decision.operator, decision.state, true
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

// UsesBackendPolicy reports whether the global policy chain selected this mode.
func UsesBackendPolicy(c *gin.Context) bool { return c.GetBool(string(backendPolicyKey)) }
