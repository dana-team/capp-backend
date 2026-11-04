package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/capps/revisions"
	"github.com/dana-team/capp-backend/internal/testutil"
	"github.com/dana-team/capp-backend/pkg/k8s"
	cappv1alpha1 "github.com/dana-team/container-app-operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBackendPolicyGitOperations(t *testing.T) {
	store, _, _ := policyStore(t, nil)
	operations := []struct {
		name, method, suffix, body string
		status                     int
		gitFirst                   bool
	}{
		{"sync", http.MethodPost, "/sync", "", http.StatusOK, true},
		{"unsync", http.MethodDelete, "/sync", "", http.StatusOK, true},
		{"update", http.MethodPut, "", `{"name":"app","image":"nginx:2"}`, http.StatusOK, false},
		{"delete", http.MethodDelete, "", "", http.StatusNoContent, false},
		{"rollback", http.MethodPost, "/revisions/1/rollback", "", http.StatusOK, false},
	}
	for _, op := range operations {
		scenarios := []string{"viewer", "unassigned", "editor"}
		// Cover both effect orderings here; handler tests own the Git failure matrix.
		switch op.name {
		case "sync", "update":
			scenarios = append(scenarios, "git disabled", "kubernetes failure")
		case "rollback":
			scenarios = append(scenarios, "kubernetes failure")
		}
		for _, scenario := range scenarios {
			t.Run(op.name+"/"+scenario, func(t *testing.T) {
				var spec cappv1alpha1.CappSpec
				spec.ConfigurationSpec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "nginx:1"}}
				capp := &cappv1alpha1.Capp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a", Labels: map[string]string{k8s.LabelBackupToGit: "true"}}, Spec: spec}
				rev := &cappv1alpha1.CappRevision{
					ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "team-a", Labels: map[string]string{k8s.LabelCappName: "app"}},
					Spec:       cappv1alpha1.CappRevisionSpec{RevisionNumber: 1, CappTemplate: cappv1alpha1.CappTemplate{Spec: spec}},
				}
				reads, writes := 0, 0
				var writeErr error
				if scenario == "kubernetes failure" {
					writeErr = errors.New("private-kubernetes-failure")
				}
				execution := fake.NewClientBuilder().WithScheme(testutil.TestScheme(t)).WithRESTMapper(policyExecutionMapper()).WithObjects(capp, rev).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						reads++
						return c.Get(ctx, key, obj, opts...)
					},
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						reads++
						return c.List(ctx, list, opts...)
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						writes++
						if writeErr != nil {
							return writeErr
						}
						return c.Update(ctx, obj, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						writes++
						if writeErr != nil {
							return writeErr
						}
						return c.Delete(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						writes++
						if writeErr != nil {
							return writeErr
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()
				git := &policyGitRecorder{}
				user := "editor"
				if scenario == "viewer" {
					user = "alice"
				}
				if scenario == "unassigned" {
					user = "bob"
				}
				mgr := policyManager(t, nil)
				r := policyRouter(t, policyIdentity(user), mgr, middleware.PolicyTarget{Store: store, ExecutionClient: execution}, zap.NewNop())
				group := r.Group("/api/v1/clusters/:cluster")
				enabled := scenario != "git disabled"
				capps.New(enabled, git, mgr, config.CappSizes{}).RegisterRoutes(group)
				revisions.New(enabled, git, config.CappSizes{}).RegisterRoutes(group)
				w := policyRequest(r, op.method, "/api/v1/clusters/c1/namespaces/team-a/capps/app"+op.suffix, op.body)
				status, wantWrites, wantGit := op.status, 1, 1
				switch scenario {
				case "viewer", "unassigned":
					status, wantWrites, wantGit = http.StatusForbidden, 0, 0
				case "git disabled":
					wantGit = 0
					if op.gitFirst {
						status, wantWrites = http.StatusNotImplemented, 0
					}
				case "kubernetes failure":
					status = http.StatusInternalServerError
					if !op.gitFirst {
						wantGit = 0
					}
				}
				require.Equal(t, status, w.Code, w.Body.String())
				require.Equal(t, wantWrites, writes, "resource mutations")
				require.Equal(t, wantGit, git.calls, "Git effects")
				if status == http.StatusForbidden || status == http.StatusNotImplemented {
					require.Zero(t, reads, "denied or disabled operation must not read resources")
				}
				if status == http.StatusForbidden {
					requirePolicyError(t, w, apierrors.CodeForbidden, "POLICY_DENIED")
				}
				require.NotContains(t, w.Body.String(), "private-")
			})
		}
	}
}

type policyGitRecorder struct {
	calls int
}

func (g *policyGitRecorder) SyncValues(context.Context, string, string, string, []byte) (string, error) {
	g.calls++
	return "commit", nil
}
func (g *policyGitRecorder) DeleteValues(context.Context, string, string, string) (string, error) {
	g.calls++
	return "commit", nil
}
func (*policyGitRecorder) BuildRelPath(cluster, namespace, name string) string {
	return cluster + "/" + namespace + "/" + name
}
