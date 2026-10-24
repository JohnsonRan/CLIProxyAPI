package models

import "testing"

func TestModelCapabilitiesCodexCatalogHidesNonChatModels(t *testing.T) {
	resp := BuildResponse([]map[string]any{
		{"id": "chat-model", "architecture": map[string]any{"output_modalities": []string{"text"}}},
		{"id": "unknown-model"},
		{"id": "speech-model", "architecture": map[string]any{"output_modalities": []any{"audio"}}},
		{"id": "embedding-model", "architecture": map[string]any{"output_modalities": []string{"embeddings"}}},
		{"id": "rerank-model", "architecture": map[string]any{"output_modalities": []any{"rerank"}}},
		{"id": "decision-model", "architecture": map[string]any{"output_modalities": []string{"decisions"}}},
		{"id": "gpt-6-astra", "architecture": map[string]any{"output_modalities": []string{"embeddings"}}},
	}, nil, false)

	models, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models type = %T, want []map[string]any", resp["models"])
	}
	visibility := make(map[string]any, len(models))
	for _, entry := range models {
		slug, _ := entry["slug"].(string)
		visibility[slug] = entry["visibility"]
	}
	for _, id := range []string{"chat-model", "unknown-model", "speech-model"} {
		if got, ok := visibility[id]; !ok || got == "hide" {
			t.Fatalf("%s visibility = %v (listed=%v), want listed and not hidden", id, got, ok)
		}
	}
	// gpt-6-astra exercises the template path.
	for _, id := range []string{"embedding-model", "rerank-model", "decision-model", "gpt-6-astra"} {
		if got := visibility[id]; got != "hide" {
			t.Fatalf("%s visibility = %v, want hide", id, got)
		}
	}
}
