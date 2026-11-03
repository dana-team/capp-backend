package cluster

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func TestNewSystemClients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer system-token" {
			t.Error("system credential missing")
		}
		for key := range r.Header {
			if len(key) >= len("Impersonate-") && key[:len("Impersonate-")] == "Impersonate-" {
				t.Errorf("unexpected impersonation header %q", key)
			}
		}
		w.Header().Set("Content-Type", runtime.ContentTypeJSON)
		var body any
		switch r.URL.Path {
		case "/api":
			body = metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions", APIVersion: "v1"}, Versions: []string{"v1"}}
		case "/apis":
			body = metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}}
		case "/api/v1":
			body = metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "namespaces", Kind: "Namespace"}}}
		case "/api/v1/namespaces/team-a":
			body = corev1.Namespace{TypeMeta: metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	cfg := &rest.Config{Host: server.URL, BearerToken: "system-token", Impersonate: rest.ImpersonationConfig{UserName: "alice", UID: "alice-uid", Groups: []string{"developers"}, Extra: map[string][]string{"key": {"value"}}}}
	clients, err := NewSystemClients(&ClusterClient{RestConfig: cfg, Scheme: scheme})
	require.NoError(t, err)
	_, err = clients.Kubernetes.CoreV1().Namespaces().Get(t.Context(), "team-a", metav1.GetOptions{})
	require.NoError(t, err)
	var ns corev1.Namespace
	require.NoError(t, clients.Resources.Get(t.Context(), types.NamespacedName{Name: "team-a"}, &ns))
	require.Equal(t, "alice", cfg.Impersonate.UserName)
	require.Equal(t, []string{"developers"}, cfg.Impersonate.Groups)
	require.Equal(t, "system-token", cfg.BearerToken)
}

func TestNewSystemClientsInvalidConfiguration(t *testing.T) {
	for _, cluster := range []*ClusterClient{nil, {}, {RestConfig: &rest.Config{}}} {
		_, err := NewSystemClients(cluster)
		require.Error(t, err)
	}
}
