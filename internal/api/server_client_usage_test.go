package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientUsageRouteRequiresClientKey pins GET /v1/usage behind the client
// API key middleware and checks an authenticated key without a selection sees
// no credentials.
func TestClientUsageRouteRequiresClientKey(t *testing.T) {
	server := newTestServer(t)

	unauthorized := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d body=%s", unauthorized.Code, http.StatusUnauthorized, unauthorized.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	authorized := httptest.NewRecorder()
	server.engine.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d body=%s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
	if got := authorized.Body.String(); got != `{"credentials":[]}` {
		t.Fatalf("body = %s, want no credentials", got)
	}
}
