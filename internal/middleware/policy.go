package middleware

import (
	"errors"
	"maps"
	"net/http"

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
	auditOutcomeDenied  = "denied"
	auditOutcomeAllowed = "allowed"
)

const reasonPolicyUnavailable = "POLICY_UNAVAILABLE"

// PolicyTarget holds a cluster's observed policy and backend execution client.
// The caller owns their lifecycle; neither may use end-user impersonation.
type PolicyTarget struct {
	Store           *authorization.Store
	ExecutionClient client.Client
}

type policyDecision struct {
	request                           *http.Request
	method, route, cluster, namespace string
	category                          PolicyCategory
	state                             authorization.NamespaceState
}

// BackendPolicy authenticates and prepares requests using the fixed API rules.
// Install globally after CORS, with Logging and Recovery, without
// legacy Auth/Cluster middleware. Discovery, aggregate and migration consumers
// must use their category-specific authorization before accessing resources.
func BackendPolicy(authMgr auth.AuthManager, clusterMgr cluster.ClusterManager, routes *PolicyRoutes, targets map[string]PolicyTarget, logger *zap.Logger) gin.HandlerFunc {
	targets = maps.Clone(targets)
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
		decision := policyDecision{request: c.Request, method: c.Request.Method, route: c.FullPath(), category: rule.Category, cluster: c.Param("cluster")}
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
		if clusterName != "" {
			cc := preparePolicyCluster(c, clusterMgr)
			if cc == nil {
				return
			}
		}

		switch rule.Category {
		case PolicyNamespace, PolicyMigration:
			namespace := c.Param("namespace")
			if len(validation.ValidateNamespaceName(namespace, false)) != 0 {
				respondPolicyError(c, authorization.ErrDenied)
				return
			}
			decision.namespace = namespace
			target := targets[clusterName]
			if target.Store == nil {
				respondPolicyError(c, authorization.ErrUnavailable)
				return
			}
			state, err := target.Store.Authorize(namespace, caller, cred.ImpersonateGroups, rule.AllowedRoles)
			decision.state = state
			if err != nil {
				respondPolicyError(c, err)
				return
			}
			if rule.Category == PolicyNamespace {
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
	value, _ := c.Get(string(policyDecisionKey))
	decision, ok := value.(policyDecision)
	if !ok || decision.request != c.Request || decision.method != c.Request.Method ||
		decision.route != c.FullPath() || decision.cluster != c.Param("cluster") ||
		decision.namespace != c.Param("namespace") || decision.category != PolicyNamespace {
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
	if c.FullPath() != "/api/v1/clusters/:cluster" && !cc.IsHealthy() {
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
	logger.Info("authorization decision",
		zap.String("caller", caller), zap.String("method", decision.method), zap.String("route", decision.route),
		zap.String("cluster", decision.cluster), zap.String("namespace", decision.namespace),
		zap.String("outcome", outcome),
		zap.String("policyUID", string(decision.state.PolicyUID)), zap.String("policyVersion", decision.state.PolicyResourceVersion),
	)
}
