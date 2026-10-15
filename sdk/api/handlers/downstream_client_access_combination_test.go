package handlers

// Downstream integration test owned by the ci branch. It needs both
// patch/client-api-key-models (client-keys model rules) and
// patch/client-key-provider-scope (client.key-scopes) and checks that a key
// restricted by both mechanisms gets their intersection.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

const (
	comboKey          = "sk-combo-abc"
	comboClaudeModel  = "combo-claude-model"
	comboCodexModel   = "combo-codex-model"
	comboCodexHidden  = "combo-codex-hidden"
	comboSharedModel  = "combo-shared-model"
	comboClaudeAuthID = "combo-claude-auth"
	comboCodexAuthID  = "combo-codex-auth"
)

type comboExecutor struct {
	provider string
	mu       *sync.Mutex
	calls    *[]string
}

func (e *comboExecutor) Identifier() string { return e.provider }

func (e *comboExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	*e.calls = append(*e.calls, e.provider)
	e.mu.Unlock()
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *comboExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *comboExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *comboExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *comboExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestClientModelRulesAndKeyScopesIntersect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var calls []string
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&comboExecutor{provider: "claude", mu: &mu, calls: &calls})
	manager.RegisterExecutor(&comboExecutor{provider: "codex", mu: &mu, calls: &calls})
	modelRegistry := registry.GetGlobalRegistry()
	for _, credential := range []struct {
		id, provider string
		models       []*registry.ModelInfo
	}{
		{comboClaudeAuthID, "claude", []*registry.ModelInfo{{ID: comboClaudeModel}, {ID: comboSharedModel}}},
		{comboCodexAuthID, "codex", []*registry.ModelInfo{{ID: comboCodexModel}, {ID: comboCodexHidden}, {ID: comboSharedModel}}},
	} {
		if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: credential.id, Provider: credential.provider, Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
		modelRegistry.RegisterClient(credential.id, credential.provider, credential.models)
		id := credential.id
		t.Cleanup(func() { modelRegistry.UnregisterClient(id) })
	}

	cfg := &sdkconfig.SDKConfig{
		ClientKeys: []config.ClientKeyConfig{{APIKey: comboKey, ExcludedModels: []string{"combo-codex-hidden"}}},
	}
	cfg.Client.KeyScopes = &[]config.ClientKeyScope{{KeyPrefix: "sk-combo-", Providers: []string{"codex"}}}
	handler := NewBaseAPIHandlers(cfg, manager)

	// Model list: the scope removes the Claude-only model, the client rule
	// removes the hidden Codex model.
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Request = c.Request.WithContext(coreusage.WithAPIKey(c.Request.Context(), comboKey))
	c.Set("userApiKey", comboKey)
	handler.WriteModelListResponse(c, "openai", gin.H{"object": "list", "data": []gin.H{
		{"id": comboClaudeModel}, {"id": comboCodexModel}, {"id": comboCodexHidden}, {"id": comboSharedModel},
	}})
	var listed []string
	for _, item := range gjson.GetBytes(recorder.Body.Bytes(), "data").Array() {
		listed = append(listed, item.Get("id").String())
	}
	if len(listed) != 2 || listed[0] != comboCodexModel || listed[1] != comboSharedModel {
		t.Fatalf("listed models = %v, want [%s %s]", listed, comboCodexModel, comboSharedModel)
	}

	// Execution: both checks run before a credential is chosen, and the shared
	// model is served only by the scoped provider.
	execCtx := context.WithValue(coreusage.WithAPIKey(context.Background(), comboKey), "gin", c)
	execute := func(model string) int {
		_, _, errMsg := handler.ExecuteWithAuthManager(execCtx, "openai", model, []byte(`{"model":"`+model+`"}`), "")
		if errMsg == nil {
			return http.StatusOK
		}
		return errMsg.StatusCode
	}
	if status := execute(comboClaudeModel); status == http.StatusOK {
		t.Fatalf("%s outside the key scope was served", comboClaudeModel)
	}
	if status := execute(comboCodexHidden); status == http.StatusOK {
		t.Fatalf("%s excluded by the client rule was served", comboCodexHidden)
	}
	mu.Lock()
	if len(calls) != 0 {
		t.Fatalf("refused requests reached executors: %v", calls)
	}
	mu.Unlock()
	for i := 0; i < 4; i++ {
		if status := execute(comboSharedModel); status != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", comboSharedModel, status)
		}
	}
	if status := execute(comboCodexModel); status != http.StatusOK {
		t.Fatalf("%s: status %d, want 200", comboCodexModel, status)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, provider := range calls {
		if provider != "codex" {
			t.Fatalf("executor calls = %v, want only codex", calls)
		}
	}
}
