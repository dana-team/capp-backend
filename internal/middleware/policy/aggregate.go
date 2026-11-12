package policy

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/authorization"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ListAggregate fills list from namespaces permitted by the prepared API rule.
// It follows upstream pages and withholds results until access and identities
// have been rechecked. On failure it writes the HTTP error and returns false.
func ListAggregate(c *gin.Context, list client.ObjectList, options ...client.ListOption) bool {
	decision, ok := requestPolicyDecision(c)
	if !Enabled(c) || !ok || decision.category != Aggregate || decision.aggregate == nil {
		respondPolicyError(c, authorization.ErrDenied)
		return false
	}
	if err := decision.aggregate(c, list, options...); err != nil {
		respondPolicyOperationError(c, err)
		return false
	}
	return true
}

func listAggregate(c *gin.Context, list client.ObjectList, target Target, cred auth.ClusterCredential, roles []string, decision policyDecision, logger *zap.Logger, options ...client.ListOption) error {
	if target.Store == nil {
		return authorization.ErrUnavailable
	}
	states, synced := target.Store.Eligible(cred.ImpersonateUser, cred.ImpersonateGroups, roles)
	if !synced {
		return authorization.ErrUnavailable
	}
	type batch struct {
		state authorization.NamespaceState
		items []client.Object
		audit aggregateAudit
	}
	batches := make([]batch, 0, len(states))
	for _, state := range states {
		if !decision.namespaceAllowed(state.Namespace.Name) {
			continue
		}
		// Only this namespace-bound client reaches the resource LIST loop.
		if target.ExecutionClient == nil {
			return authorization.ErrUnavailable
		}
		reader := client.NewNamespacedClient(target.ExecutionClient, state.Namespace.Name)
		var items []client.Object
		var audit aggregateAudit
		continuation := ""
		for {
			allowed, err := recheckAggregate(target, cred, roles, state, decision, logger, &audit)
			if err != nil {
				return err
			}
			if !allowed {
				items = nil
				break
			}
			page, ok := list.DeepCopyObject().(client.ObjectList)
			if !ok {
				return fmt.Errorf("unsupported aggregate list type %T", list)
			}
			pageOptions := append(slices.Clone(options), client.Continue(continuation))
			if err := reader.List(c.Request.Context(), page, pageOptions...); err != nil {
				return err
			}
			objects, err := meta.ExtractList(page)
			if err != nil {
				return err
			}
			for _, object := range objects {
				item, ok := object.(client.Object)
				if !ok {
					return fmt.Errorf("unsupported aggregate item type %T", object)
				}
				items = append(items, item)
			}
			continuation = page.GetContinue()
			if continuation == "" {
				break
			}
		}
		batches = append(batches, batch{state: state, items: items, audit: audit})
	}

	var items []client.Object
	for _, batch := range batches {
		allowed, err := recheckAggregate(target, cred, roles, batch.state, decision, logger, &batch.audit)
		if err != nil {
			return err
		}
		if allowed {
			items = append(items, batch.items...)
		}
	}
	slices.SortFunc(items, func(a, b client.Object) int {
		if order := cmp.Compare(a.GetNamespace(), b.GetNamespace()); order != 0 {
			return order
		}
		return cmp.Compare(a.GetName(), b.GetName())
	})
	objects := make([]runtime.Object, len(items))
	for i, item := range items {
		objects[i] = item
	}
	return meta.SetList(list, objects)
}

type aggregateAudit struct {
	outcome       string
	policyUID     string
	policyVersion string
}

func recheckAggregate(target Target, cred auth.ClusterCredential, roles []string, original authorization.NamespaceState, decision policyDecision, logger *zap.Logger, lastAudit *aggregateAudit) (bool, error) {
	state, err := target.Store.Authorize(original.Namespace.Name, cred.ImpersonateUser, cred.ImpersonateGroups, roles)
	if state.Reason == authorization.CacheNotReady {
		return false, authorization.ErrUnavailable
	}
	allowed := err == nil && decision.namespaceAllowed(original.Namespace.Name) &&
		state.Namespace.UID == original.Namespace.UID && state.PolicyUID == original.PolicyUID
	decision.namespace, decision.state = original.Namespace.Name, state
	decision.category = Namespace
	outcome := auditOutcomeDenied
	if allowed {
		outcome = auditOutcomeAllowed
	}
	audit := aggregateAudit{outcome: outcome, policyUID: string(state.PolicyUID), policyVersion: state.PolicyResourceVersion}
	// Recheck every time, but only audit changes within this request's namespace batch.
	if audit != *lastAudit {
		auditPolicy(logger, cred.ImpersonateUser, decision, outcome)
		*lastAudit = audit
	}
	return allowed, nil
}
