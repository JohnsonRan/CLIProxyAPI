package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type inputTokensCountExecutor struct {
	payload      []byte
	sourceFormat string
	request      []byte
	calls        int
}

func (e *inputTokensCountExecutor) Identifier() string { return "input-tokens-provider" }

func (e *inputTokensCountExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *inputTokensCountExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *inputTokensCountExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *inputTokensCountExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.sourceFormat = opts.SourceFormat.String()
	e.request = append([]byte(nil), req.Payload...)
	return coreexecutor.Response{Payload: e.payload}, nil
}

func (e *inputTokensCountExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newInputTokensRouter(t *testing.T, executor *inputTokensCountExecutor) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "input-tokens-auth-" + t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "input-tokens-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses/input_tokens", h.InputTokens)
	return router
}

func TestResponsesInputTokensNormalizesExecutorCountShapes(t *testing.T) {
	cases := map[string]string{
		"claude":             `{"input_tokens":11}`,
		"codex":              `{"response":{"usage":{"input_tokens":11,"output_tokens":0,"total_tokens":11}}}`,
		"openai-compatible":  `{"usage":{"prompt_tokens":11,"completion_tokens":0,"total_tokens":11}}`,
		"gemini":             `{"totalTokens":11}`,
		"devin":              `{"total_tokens":11,"input_tokens":11}`,
		"responses-usage":    `{"usage":{"input_tokens":11}}`,
		"plain-total-tokens": `{"total_tokens":11}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			executor := &inputTokensCountExecutor{payload: []byte(payload)}
			router := newInputTokensRouter(t, executor)

			req := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"input-tokens-model","input":"hello","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.Code, resp.Body.String())
			}
			if got := strings.TrimSpace(resp.Body.String()); got != `{"object":"response.input_tokens","input_tokens":11}` {
				t.Fatalf("body = %s", got)
			}
			if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q", ct)
			}
			if executor.sourceFormat != "openai-response" {
				t.Fatalf("source format = %q, want openai-response", executor.sourceFormat)
			}
			if gjson.GetBytes(executor.request, "stream").Exists() {
				t.Fatalf("stream must be removed before counting: %s", executor.request)
			}
		})
	}
}

func TestResponsesInputTokensKeepsZeroCount(t *testing.T) {
	executor := &inputTokensCountExecutor{payload: []byte(`{"input_tokens":0}`)}
	router := newInputTokensRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"input-tokens-model","input":[]}`))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK || strings.TrimSpace(resp.Body.String()) != `{"object":"response.input_tokens","input_tokens":0}` {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
}

func TestResponsesInputTokensRejectsUnrecognizedCount(t *testing.T) {
	executor := &inputTokensCountExecutor{payload: []byte(`{"tokens":"many"}`)}
	router := newInputTokensRouter(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"input-tokens-model","input":"hello"}`))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", resp.Code, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
}
