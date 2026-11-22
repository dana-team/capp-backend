package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/auth"
	"github.com/dana-team/capp-backend/internal/config"
	"github.com/dana-team/capp-backend/internal/middleware"
	requestpolicy "github.com/dana-team/capp-backend/internal/middleware/policy"
	"github.com/dana-team/capp-backend/internal/resources/namespaced/configmaps"
	"github.com/dana-team/capp-backend/pkg/clientversion"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/util/keyutil"
	"knative.dev/pkg/network"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBackendPolicyAudit(t *testing.T) {
	for _, mode := range []string{config.AuthModeLDAP, config.AuthModeOpenShift, config.AuthModePassthrough} {
		for _, scenario := range []string{"discovery", "aggregate", "migration", "destination rejected", "source unavailable", "discovery rejected", "discovery unavailable"} {
			if mode != config.AuthModePassthrough && (strings.Contains(scenario, "rejected") || strings.Contains(scenario, "unavailable")) {
				continue
			}
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				sourceStore, _, _ := policyStore(t, nil)
				destinationStore, _, _ := policyStore(t, nil)
				destinationUser := "editor"
				if mode == config.AuthModePassthrough {
					destinationUser = "destination-editor"
				}
				fixtureMode := mode
				if mode == config.AuthModeLDAP {
					fixtureMode = config.AuthModePassthrough
				}
				migration := &migrationExecution{targetCluster: "c2", targetNamespace: "team-a", allowed: scenario == "migration"}
				p := newPolicyProvider(t, fixtureMode, map[string]authv1.UserInfo{"c1": {Username: "editor"}, "c2": {Username: destinationUser, Groups: []string{"developers"}, Extra: map[string]authv1.ExtraValue{"diagnostic": {"private-review-body"}}}}, migration.handler(t))
				token := "caller-token"
				if mode == config.AuthModeLDAP {
					p.auth, token = auditLDAPIdentity(t)
				}
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				request.Header.Set("Authorization", "Bearer "+token)
				identities := make(map[string]auth.ClusterCredential)
				for _, target := range []string{"c1", "c2"} {
					identity, err := p.auth.Authenticate(t.Context(), target, request)
					if err != nil {
						t.Fatal(err)
					}
					identities[target] = identity
				}
				if mode != config.AuthModePassthrough {
					identities[""] = identities["c1"]
				}
				if scenario == "destination rejected" || scenario == "discovery rejected" {
					p.reviews["c2"].user.Store(nil)
				}
				if scenario == "source unavailable" || scenario == "discovery unavailable" {
					p.reviews["c1"].status.Store(http.StatusForbidden)
				}
				core, logs := observer.New(zap.InfoLevel)
				rules, err := requestpolicy.NewRoutes("")
				if err != nil {
					t.Fatal(err)
				}
				execution := aggregateClient(t, interceptor.Funcs{})
				targets := map[string]requestpolicy.Target{"c1": {Store: sourceStore, ExecutionClient: execution}, "c2": {Store: destinationStore, ExecutionClient: execution}}
				if scenario == "migration" || scenario == "destination rejected" {
					for name, target := range targets {
						target.ExecutionClient = p.clients[name].Resources
						targets[name] = target
					}
				}
				r := gin.New()
				r.Use(middleware.ClientVersion("v0.6.0", clientversion.Known, zap.New(core)))
				r.Use(requestpolicy.New(p.auth, mode, p.manager, rules, targets, zap.New(core)))
				r.GET("/api/v1/clusters", listClusters(p.manager))
				configmaps.New().RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
				newCappsHandler(t, true, &policyGitRecorder{}, config.ObservabilityConfig{}).RegisterRoutes(r.Group("/api/v1/clusters/:cluster"))
				method, path, status, count := http.MethodGet, "/api/v1/clusters", http.StatusOK, 3
				switch scenario {
				case "discovery rejected", "discovery unavailable":
					count = 2
				case "aggregate":
					path, count = "/api/v1/clusters/c1/configmaps", 2
				case "migration", "destination rejected":
					method, path, count = http.MethodPost, "/api/v1/clusters/c1/namespaces/team-a/capps/app/migrate", 2
					if scenario == "destination rejected" {
						status = http.StatusForbidden
					}
				case "source unavailable":
					path, status, count = "/api/v1/clusters/c1/configmaps", http.StatusServiceUnavailable, 1
				}
				req := httptest.NewRequest(method, path, strings.NewReader(`{"targetCluster":"c2","targetNamespace":"team-a"}`))
				req.Header.Set("Authorization", "Bearer "+token)
				if scenario == "discovery" {
					req.Header.Set(network.UserAgentKey, clientversion.Known[0].UserAgent("v0.5.0"))
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != status {
					t.Fatalf("request status = %d, want %d: %s", w.Code, status, w.Body.String())
				}
				entries := logs.FilterMessage("authorization decision").All()
				if len(entries) != count {
					t.Fatalf("audit count = %d, want %d: %v", len(entries), count, entries)
				}
				requireAuditIdentities(t, entries, identities, scenario, token)
				if scenario == "discovery" {
					requireOutdatedClientUser(t, logs, identities[""].ImpersonateUser)
				}
			})
		}
	}
}

func auditLDAPIdentity(t *testing.T) (auth.AuthManager, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	issuer := "https://user:url-password@issuer.example/auth?key=query-secret#fragment-secret"
	cfg := config.LDAPConfig{URL: "ldaps://directory.example", Issuer: issuer, Audience: "backend", SigningKeyID: "active", SigningPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: keyutil.PrivateKeyBlockType, Bytes: der})), TokenTTLSeconds: 900}
	manager, err := auth.New(&config.Config{Auth: config.AuthConfig{Mode: config.AuthModeLDAP, LDAP: cfg}}, auth.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"iss": issuer, "aud": "backend", "sub": "editor", "groups": []string{}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "token_use": "access"})
	token.Header["kid"] = "active"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return manager, signed
}

func requireAuditIdentities(t *testing.T, entries []observer.LoggedEntry, identities map[string]auth.ClusterCredential, scenario, token string) {
	t.Helper()
	for _, entry := range entries {
		fields := entry.ContextMap()
		target, _ := fields["cluster"].(string)
		identity := identities[target]
		if scenario == "destination rejected" && target == "c2" || scenario == "source unavailable" {
			identity = auth.ClusterCredential{}
		}
		for key, want := range map[string]string{"caller": identity.ImpersonateUser, "authProvider": identity.AuthProvider, "identityAuthority": identity.IdentityAuthority} {
			if fields[key] != want {
				t.Errorf("%s audit %s = %v, want %q", target, key, fields[key], want)
			}
		}
		for _, secret := range []string{token, "backend-token", "url-password", "query-secret", "fragment-secret", "TokenReview", "private-review-body"} {
			if strings.Contains(fmt.Sprint(fields), secret) {
				t.Errorf("audit exposed %q", secret)
			}
		}
	}
}

func requireOutdatedClientUser(t *testing.T, logs *observer.ObservedLogs, want string) {
	t.Helper()
	warnings := logs.FilterMessage("outdated client").All()
	if len(warnings) != 1 {
		t.Fatalf("outdated client log count = %d, want 1", len(warnings))
	}
	user, present := warnings[0].ContextMap()["user"]
	if want == "" {
		if present {
			t.Errorf("outdated client user = %v, want absent", user)
		}
		return
	}
	if user != want {
		t.Errorf("outdated client user = %v, want %q", user, want)
	}
}
