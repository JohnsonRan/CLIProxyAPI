package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestModelCapabilitiesOpenAIModelsListsArchitecture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clientID := "openai-models-capabilities-test"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{
			ID:                        "capabilities-embedding",
			OwnedBy:                   "test",
			Type:                      "openai-compatibility",
			SupportedOutputModalities: []string{"embeddings"},
		},
		{
			ID:                        "capabilities-chat",
			OwnedBy:                   "test",
			Type:                      "openai-compatibility",
			ContextLength:             2048,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
		},
		{
			ID:      "capabilities-unknown",
			OwnedBy: "test",
			Type:    "openai-compatibility",
		},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	handler := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	handler.OpenAIModels(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	entries := make(map[string]map[string]any, len(body.Data))
	for _, entry := range body.Data {
		id, _ := entry["id"].(string)
		entries[id] = entry
	}

	outputOf := func(id string) []any {
		entry := entries[id]
		if entry == nil {
			t.Fatalf("model %s missing from /v1/models", id)
		}
		architecture, _ := entry["architecture"].(map[string]any)
		output, _ := architecture["output_modalities"].([]any)
		return output
	}
	if got := outputOf("capabilities-embedding"); len(got) != 1 || got[0] != "embeddings" {
		t.Fatalf("embedding output_modalities = %v, want [embeddings]", got)
	}
	if got := outputOf("capabilities-chat"); len(got) != 1 || got[0] != "text" {
		t.Fatalf("chat output_modalities = %v, want [text]", got)
	}
	if got := entries["capabilities-chat"]["context_length"]; got != float64(2048) {
		t.Fatalf("chat context_length = %v, want 2048", got)
	}
	if _, exists := entries["capabilities-unknown"]["architecture"]; exists {
		t.Fatalf("unknown model must not advertise architecture: %#v", entries["capabilities-unknown"])
	}
}
