package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/dana-team/capp-backend/internal/resources/namespaced/boundcapps"
	"github.com/dana-team/capp-backend/internal/testutil"
	fissionv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func functionReading(secret string) *fissionv1.Function {
	return &fissionv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns1"},
		Spec:       fissionv1.FunctionSpec{Secrets: []fissionv1.SecretReference{{Namespace: "ns1", Name: secret}}},
	}
}

func TestDelete_RefusedWhileAFunctionReadsTheSecret(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedSecret("s1", "ns1"), managedSecret("s2", "ns1"), functionReading("s1"))
	h := testutil.NewEngineHelper(t, k8sClient, New().WithFunctionGuard(true))

	w := h.Delete("/namespaces/ns1/secrets/s1")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "still in use by functions: shop")

	assert.Equal(t, http.StatusNoContent, h.Delete("/namespaces/ns1/secrets/s2").Code)
}

func TestDelete_FunctionGuardIsOffByDefault(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedSecret("s1", "ns1"), functionReading("s1"))

	w := testutil.NewEngineHelper(t, k8sClient, New()).Delete("/namespaces/ns1/secrets/s1")

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestBound_ListsFunctions(t *testing.T) {
	k8sClient := testutil.FakeClient(t, managedSecret("s1", "ns1"), functionReading("s1"))

	w := testutil.NewEngineHelper(t, k8sClient, New().WithFunctionGuard(true)).Get("/namespaces/ns1/secrets/s1/bound")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp boundcapps.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []string{"shop"}, resp.BoundFunctions)
	assert.Empty(t, resp.BoundCapps)
}

// A Role that grants this resource but nothing of Fission worked before
// functions existed. The guard must not turn its requests into a 403.
func TestFunctionGuard_SkippedForACallerWhoMayNotListFunctions(t *testing.T) {
	k8sClient := testutil.FakeClientWithInterceptors(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isFunctions := list.(*fissionv1.FunctionList); isFunctions {
				return k8serrors.NewForbidden(schema.GroupResource{Group: "fission.io", Resource: "functions"}, "", errors.New("no"))
			}
			return c.List(ctx, list, opts...)
		},
	}, managedSecret("s1", "ns1"))
	h := testutil.NewEngineHelper(t, k8sClient, New().WithFunctionGuard(true))

	bound := h.Get("/namespaces/ns1/secrets/s1/bound")
	require.Equal(t, http.StatusOK, bound.Code, bound.Body.String())
	assert.JSONEq(t, `{"boundCapps":[],"toReload":[],"boundFunctions":[]}`, bound.Body.String())

	assert.Equal(t, http.StatusNoContent, h.Delete("/namespaces/ns1/secrets/s1").Code)
}
