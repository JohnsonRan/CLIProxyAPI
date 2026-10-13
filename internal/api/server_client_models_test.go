package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestClientModelPermissions_ModelListFiltering(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	testModels := []*registry.ModelInfo{
		{ID: "gpt-4o"},
		{ID: "gpt-4o-mini"},
		{ID: "claude-3-5-sonnet"},
		{ID: "gemini-2.5-pro"},
	}
	modelRegistry.RegisterClient("client-perm-test", "codex", testModels)
	t.Cleanup(func() { modelRegistry.UnregisterClient("client-perm-test") })

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"key-openai-only", "key-claude-only", "key-all"},
			ClientKeys: []proxyconfig.ClientKeyConfig{
				{
					APIKey: "key-openai-only",
					Models: []string{"gpt-4o*"},
				},
				{
					APIKey: "key-claude-only",
					Models: []string{"claude-*"},
				},
				{
					APIKey: "key-all",
					Models: []string{"*"},
				},
			},
		},
	}

	configaccess.Register(&cfg.SDKConfig)
	t.Cleanup(func() { configaccess.Register(nil) })

	server := newTestServerWithConfig(t, cfg)
	server.accessManager.SetProviders(sdkaccess.RegisteredProviders())

	t.Run("OpenAI key sees only gpt-4o models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer key-openai-only")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(resp.Data) != 2 {
			t.Fatalf("expected 2 models, got %d: %v", len(resp.Data), resp.Data)
		}
		for _, m := range resp.Data {
			id := m["id"].(string)
			if !strings.HasPrefix(id, "gpt-4o") {
				t.Errorf("unexpected model %q in response", id)
			}
		}
	})

	t.Run("Claude key sees only claude models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer key-claude-only")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(resp.Data) != 1 {
			t.Fatalf("expected 1 model, got %d: %v", len(resp.Data), resp.Data)
		}
		if resp.Data[0]["id"] != "claude-3-5-sonnet" {
			t.Errorf("expected claude-3-5-sonnet, got %v", resp.Data[0]["id"])
		}
	})

	t.Run("Admin key sees all models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer key-all")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(resp.Data) < 4 {
			t.Fatalf("expected >= 4 models, got %d", len(resp.Data))
		}
	})

	t.Run("Single model detail endpoint respects permissions", func(t *testing.T) {
		// Allowed model
		reqOk := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o", nil)
		reqOk.Header.Set("Authorization", "Bearer key-openai-only")
		recOk := httptest.NewRecorder()
		server.engine.ServeHTTP(recOk, reqOk)
		if recOk.Code != http.StatusOK {
			t.Fatalf("expected 200 for allowed model, got %d: %s", recOk.Code, recOk.Body.String())
		}

		// Disallowed model
		reqForbidden := httptest.NewRequest(http.MethodGet, "/v1/models/claude-3-5-sonnet", nil)
		reqForbidden.Header.Set("Authorization", "Bearer key-openai-only")
		recForbidden := httptest.NewRecorder()
		server.engine.ServeHTTP(recForbidden, reqForbidden)
		if recForbidden.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for disallowed model detail, got %d: %s", recForbidden.Code, recForbidden.Body.String())
		}
	})
}

func TestClientModelPermissions_ExecutionForbidden(t *testing.T) {
	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"key-openai-only"},
			ClientKeys: []proxyconfig.ClientKeyConfig{
				{
					APIKey: "key-openai-only",
					Models: []string{"gpt-4o*"},
				},
			},
		},
	}

	configaccess.Register(&cfg.SDKConfig)
	t.Cleanup(func() { configaccess.Register(nil) })

	server := newTestServerWithConfig(t, cfg)
	server.accessManager.SetProviders(sdkaccess.RegisteredProviders())

	t.Run("ChatCompletions rejects unauthorized model with 403", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-openai-only")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "model_not_allowed") {
			t.Errorf("expected model_not_allowed error code in body: %s", rec.Body.String())
		}
	})

	t.Run("Claude Messages rejects unauthorized model with 403", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-openai-only")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Gemini GenerateContent rejects unauthorized model with 403", func(t *testing.T) {
		body := `{"contents":[{"parts":[{"text":"hello"}]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-openai-only")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Streaming ChatCompletions rejects unauthorized model with 403", func(t *testing.T) {
		body := `{"model":"claude-3-5-sonnet","stream":true,"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer key-openai-only")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for streaming, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Gemini single model detail rejects unauthorized model with 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-2.5-pro", nil)
		req.Header.Set("Authorization", "Bearer key-openai-only")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
