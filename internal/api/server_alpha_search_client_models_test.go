package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexAlphaSearchEnforcesClientModelRules(t *testing.T) {
	server := newTestServer(t)
	server.cfg.ClientExcludedModels = []string{"gpt-5*"}
	executor := &codexSearchCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	credential := &auth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "codex-token", "account_id": "account-123"},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register Codex auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"query":"q","model":"gpt-5.6"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if executor.request != nil {
		t.Fatal("excluded model reached the Codex upstream")
	}
}
