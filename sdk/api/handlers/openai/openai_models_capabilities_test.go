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

func TestModelCapabilitiesOpenAIModelsListsLimitsReasoningAndName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clientID := "openai-models-reasoning-test"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{
			ID:                        "reasoning-chat",
			OwnedBy:                   "test",
			Type:                      "openai-compatibility",
			DisplayName:               "Reasoning Chat",
			ContextLength:             400000,
			MaxCompletionTokens:       128000,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &registry.ThinkingSupport{Levels: []string{"none", "low", "medium", "high", "xhigh", "ultra"}},
		},
		{
			ID:          "reasoning-unknown",
			OwnedBy:     "test",
			Type:        "openai-compatibility",
			UserDefined: true,
		},
		{
			ID:              "reasoning-default",
			OwnedBy:         "test",
			Type:            "openai-compatibility",
			DefaultThinking: true,
			Thinking:        &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
		},
		{
			ID:                        "reasoning-embedding",
			OwnedBy:                   "test",
			Type:                      "openai-compatibility",
			SupportedOutputModalities: []string{"embeddings"},
		},
		{
			ID:                        "reasoning-rerank",
			OwnedBy:                   "test",
			Type:                      "openai-compatibility",
			SupportedOutputModalities: []string{"rerank"},
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
	entryOf := func(id string) map[string]any {
		entry := entries[id]
		if entry == nil {
			t.Fatalf("model %s missing from /v1/models", id)
		}
		return entry
	}

	chat := entryOf("reasoning-chat")
	if got := chat["name"]; got != "Reasoning Chat" {
		t.Fatalf("chat name = %v, want Reasoning Chat", got)
	}
	topProvider, _ := chat["top_provider"].(map[string]any)
	if got := topProvider["max_completion_tokens"]; got != float64(128000) {
		t.Fatalf("chat top_provider.max_completion_tokens = %v, want 128000", got)
	}
	reasoning, _ := chat["reasoning"].(map[string]any)
	if got := reasoning["supported"]; got != true {
		t.Fatalf("chat reasoning.supported = %v, want true", got)
	}
	levels, _ := reasoning["levels"].([]any)
	wantLevels := []any{"none", "low", "medium", "high", "xhigh", "ultra"}
	if len(levels) != len(wantLevels) {
		t.Fatalf("chat reasoning.levels = %v, want %v", levels, wantLevels)
	}
	for i := range wantLevels {
		if levels[i] != wantLevels[i] {
			t.Fatalf("chat reasoning.levels = %v, want %v", levels, wantLevels)
		}
	}
	for _, internal := range []string{"display_name", "max_completion_tokens", "type"} {
		if _, exists := chat[internal]; exists {
			t.Fatalf("chat must not expose %s: %#v", internal, chat)
		}
	}

	for _, id := range []string{"reasoning-unknown", "reasoning-default"} {
		entry := entryOf(id)
		for _, field := range []string{"name", "top_provider", "reasoning", "context_length", "architecture"} {
			if _, exists := entry[field]; exists {
				t.Fatalf("%s must not advertise %s: %#v", id, field, entry)
			}
		}
	}

	for id, output := range map[string]string{"reasoning-embedding": "embeddings", "reasoning-rerank": "rerank"} {
		entry := entryOf(id)
		architecture, _ := entry["architecture"].(map[string]any)
		outputs, _ := architecture["output_modalities"].([]any)
		if len(outputs) != 1 || outputs[0] != output {
			t.Fatalf("%s output_modalities = %v, want [%s]", id, outputs, output)
		}
		reasoning, _ := entry["reasoning"].(map[string]any)
		if got, exists := reasoning["supported"]; !exists || got != false {
			t.Fatalf("%s reasoning = %#v, want supported=false", id, entry["reasoning"])
		}
	}
}
