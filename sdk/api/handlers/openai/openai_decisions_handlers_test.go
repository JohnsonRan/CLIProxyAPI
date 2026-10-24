package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type decisionsCaptureExecutor struct {
	mu       sync.Mutex
	provider string
	authIDs  []string
	source   string
	// decisionsErr fails decisions requests; other requests still succeed.
	decisionsErr error
}

func (e *decisionsCaptureExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "openai-compatible-decisions"
}

func (e *decisionsCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.source = opts.SourceFormat.String()
	if e.decisionsErr != nil && e.source == handlers.OpenAIDecisionsEntryProtocol {
		return coreexecutor.Response{}, e.decisionsErr
	}
	return coreexecutor.Response{Payload: []byte(`{"object":"decision","answers":[]}`)}, nil
}

func (e *decisionsCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *decisionsCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *decisionsCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *decisionsCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newDecisionsRouter(t *testing.T, executor *decisionsCaptureExecutor, auths ...*coreauth.Auth) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register auth: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "decisions-model"}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/decisions", h.Decisions)
	return router
}

func postDecisions(router *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

const validDecisionsBody = `{"model":"decisions-model","input":"broken screen","questions":[{"type":"predicate","instructions":"Damaged?"}]}`

func TestDecisionsSkipsOAuthCredentials(t *testing.T) {
	executor := &decisionsCaptureExecutor{}
	oauth := &coreauth.Auth{ID: "decisions-oauth", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token", "refresh_token": "refresh"}}
	apiKey := &coreauth.Auth{ID: "decisions-apikey", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-test"}}
	router := newDecisionsRouter(t, executor, oauth, apiKey)

	for i := 0; i < 6; i++ {
		resp := postDecisions(router, validDecisionsBody)
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d body = %s", i, resp.Code, resp.Body.String())
		}
	}
	for _, id := range executor.authIDs {
		if id != apiKey.ID {
			t.Fatalf("selected auths = %v, want only %s", executor.authIDs, apiKey.ID)
		}
	}
	if executor.source != "openai-decisions" {
		t.Fatalf("source format = %q, want openai-decisions", executor.source)
	}
}

func TestDecisionsWithOnlyOAuthCredentialsDoesNotExecute(t *testing.T) {
	executor := &decisionsCaptureExecutor{}
	oauth := &coreauth.Auth{ID: "decisions-only-oauth", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token", "refresh_token": "refresh"}}
	router := newDecisionsRouter(t, executor, oauth)

	resp := postDecisions(router, validDecisionsBody)
	if resp.Code != http.StatusBadRequest || gjson.Get(resp.Body.String(), "error.code").String() != "model_not_supported" {
		t.Fatalf("status = %d body = %s, want 400 model_not_supported", resp.Code, resp.Body.String())
	}
	if len(executor.authIDs) != 0 {
		t.Fatalf("executor called with %v", executor.authIDs)
	}
}

func TestDecisionsValidatesRequest(t *testing.T) {
	executor := &decisionsCaptureExecutor{}
	router := newDecisionsRouter(t, executor)
	cases := map[string]string{
		"not object":      `[]`,
		"missing model":   `{"input":"x","questions":[{"type":"predicate","instructions":"i"}]}`,
		"missing input":   `{"model":"decisions-model","questions":[{"type":"predicate","instructions":"i"}]}`,
		"empty questions": `{"model":"decisions-model","input":"x","questions":[]}`,
		"object question": `{"model":"decisions-model","input":"x","questions":{"q":{"type":"predicate"}}}`,
		"stream":          `{"model":"decisions-model","input":"x","stream":true,"questions":[{"type":"predicate","instructions":"i"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := postDecisions(router, body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
			}
		})
	}
	if len(executor.authIDs) != 0 {
		t.Fatalf("executor called for invalid requests: %v", executor.authIDs)
	}
}

func TestDecisionsNeverSelectsOtherProviders(t *testing.T) {
	executor := &decisionsCaptureExecutor{provider: "claude"}
	apiKey := &coreauth.Auth{ID: "decisions-claude-key", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-ant-test"}}
	router := newDecisionsRouter(t, executor, apiKey)

	resp := postDecisions(router, validDecisionsBody)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400", resp.Code, resp.Body.String())
	}
	if len(executor.authIDs) != 0 {
		t.Fatalf("a non-Decisions provider was executed: %v", executor.authIDs)
	}
}

type decisionsStatusError struct{ code int }

func (e decisionsStatusError) Error() string {
	return `{"error":{"message":"Not Found","type":"not_found"}}`
}
func (e decisionsStatusError) StatusCode() int { return e.code }

// A provider without /decisions answers 404; that must not cool down the model
// the same credential serves for chat.
func TestDecisionsUpstreamNotFoundDoesNotCoolDownModel(t *testing.T) {
	executor := &decisionsCaptureExecutor{decisionsErr: helps.OpenAIDecisionsRequestScoped(decisionsStatusError{code: http.StatusNotFound})}
	apiKey := &coreauth.Auth{ID: "decisions-relay-key", Provider: executor.Identifier(), Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-relay"}}
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	if _, err := manager.Register(context.Background(), apiKey); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(apiKey.ID, apiKey.Provider, []*registry.ModelInfo{{ID: "decisions-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(apiKey.ID) })
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/decisions", h.Decisions)

	if resp := postDecisions(router, validDecisionsBody); resp.Code != http.StatusNotFound {
		t.Fatalf("decisions status = %d body = %s, want the upstream 404", resp.Code, resp.Body.String())
	}
	for i := 0; i < 3; i++ {
		if _, _, errMsg := h.ExecuteWithAuthManager(context.Background(), "openai", "decisions-model", []byte(`{"model":"decisions-model"}`), ""); errMsg != nil {
			t.Fatalf("chat request %d after a decisions 404: %v", i, errMsg.Error)
		}
	}
}
