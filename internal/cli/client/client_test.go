package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dana-team/capp-backend/internal/cli/client"
)

func TestGetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer testtoken", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/clusters", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []string{}}) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.New(srv.URL, "testtoken", false)
	var result map[string]any
	require.NoError(t, c.Get(context.Background(), "/api/v1/clusters", &result))
	assert.Contains(t, result, "items")
}

func TestAPIErrorParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"error": map[string]any{
				"code":    "CAPP_NOT_FOUND",
				"message": `capp "foo" not found`,
				"status":  404,
			},
		})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "testtoken", false)
	var result any
	err := c.Get(context.Background(), "/api/v1/clusters/x/namespaces/default/capps/foo", &result)
	require.Error(t, err)

	var apiErr *client.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "CAPP_NOT_FOUND", apiErr.Code)
	assert.Contains(t, apiErr.Message, "foo")
}

func TestPostSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "myapp", body["name"])
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", false)
	var result map[string]any
	require.NoError(t, c.Post(context.Background(), "/api/v1/clusters/x/namespaces/default/capps", map[string]any{"name": "myapp"}, &result))
	assert.Equal(t, "myapp", result["name"])
}

func TestPutSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": "myapp"}) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", false)
	var result map[string]any
	require.NoError(t, c.Put(context.Background(), "/api/v1/clusters/x/namespaces/default/capps/myapp", map[string]any{"name": "myapp"}, &result))
}

func TestDeleteNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := client.New(srv.URL, "tok", false)
	require.NoError(t, c.Delete(context.Background(), "/api/v1/clusters/x/namespaces/default/capps/myapp"))
}

func TestWithToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{}) //nolint:errcheck
	}))
	defer srv.Close()

	c := client.New(srv.URL, "oldtoken", false)
	c2 := c.WithToken("newtoken")

	var result any
	require.NoError(t, c2.Get(context.Background(), "/", &result))
	assert.Equal(t, "Bearer newtoken", gotAuth)
}
