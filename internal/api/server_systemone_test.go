package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestSystemOneRouteForwardsToOpenAICompatProvider(t *testing.T) {
	const upstreamResponse = `{"model":"jev-1.13.0","answers":{"department":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81}},"usage":{"input_tokens":318,"output_tokens":34}}`
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamResponse))
	}))
	defer upstream.Close()

	server := newTestServer(t)
	const provider = "systemone-route-test"
	server.handlers.AuthManager.RegisterExecutor(executor.NewOpenAICompatExecutor(provider, server.cfg))
	credential := &auth.Auth{
		ID:         "systemone-route-auth",
		Provider:   provider,
		Status:     auth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL + "/provider/v1", "api_key": "upstream-key"},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(credential.ID, provider, []*registry.ModelInfo{{ID: "typesafe/jev"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(credential.ID) })

	body := `{"model":"typesafe/jev","state":"My payments have failed for three days.","questions":{"department":{"type":"choice","instructions":"Which team?","criteria":{"billing":"payments","technical":"bugs"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/provider/v1/systemone" {
		t.Fatalf("upstream path = %q, want /provider/v1/systemone", gotPath)
	}
	if gotAuth != "Bearer upstream-key" {
		t.Fatalf("upstream Authorization = %q", gotAuth)
	}
	if gjson.Get(gotBody, "model").String() != "typesafe/jev" || gjson.Get(gotBody, "questions.department.criteria.billing").String() != "payments" {
		t.Fatalf("upstream body = %s", gotBody)
	}
	if got := gjson.GetBytes(rr.Body.Bytes(), "answers.department.choice").String(); got != "billing" {
		t.Fatalf("response body = %s", rr.Body.String())
	}
}

func TestSystemOneRoutePreservesUpstreamValidationStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"Score criteria must have at least two levels"}`))
	}))
	defer upstream.Close()

	server := newTestServer(t)
	const provider = "systemone-route-error-test"
	server.handlers.AuthManager.RegisterExecutor(executor.NewOpenAICompatExecutor(provider, server.cfg))
	credential := &auth.Auth{
		ID:         "systemone-route-error-auth",
		Provider:   provider,
		Status:     auth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL, "api_key": "upstream-key"},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(credential.ID, provider, []*registry.ModelInfo{{ID: "jev-error-test"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(credential.ID) })

	body := `{"model":"jev-error-test","state":"s","questions":{"q":{"type":"score","instructions":"i","criteria":["only"]}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "at least two levels") {
		t.Fatalf("body = %s, want upstream validation detail", rr.Body.String())
	}
}

func TestSystemOneRouteRequiresClientAuth(t *testing.T) {
	server := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}
