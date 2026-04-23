package namespaced

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dana-team/capp-backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func testGinContext(t *testing.T) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return w, c
}

// -- ExtractClient tests --

func TestExtractClient_Success(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()
	_, c := testGinContext(t)
	c.Set(string(middleware.K8sClientKey), fakeClient)

	result := ExtractClient(c)
	assert.NotNil(t, result)
}

func TestExtractClient_MissingKey(t *testing.T) {
	w, c := testGinContext(t)

	result := ExtractClient(c)
	assert.Nil(t, result)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestExtractClient_WrongType(t *testing.T) {
	w, c := testGinContext(t)
	c.Set(string(middleware.K8sClientKey), "not-a-client")

	result := ExtractClient(c)
	assert.Nil(t, result)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestExtractClient_ReturnsNilOnError(t *testing.T) {
	w, c := testGinContext(t)

	result := ExtractClient(c)
	assert.Nil(t, result)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	_, hasError := body["error"]
	assert.True(t, hasError)
}

// Verify interface compliance at compile time.
var _ client.Client = fake.NewClientBuilder().Build()
