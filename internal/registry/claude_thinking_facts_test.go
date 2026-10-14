package registry

import (
	"encoding/json"
	"testing"
)

// The release workflow and the runtime updater replace models.json with the
// remote catalog, so the thinking-off facts must survive a catalog that lacks them.
func TestClaudeThinkingFactsSurviveCatalogReplacement(t *testing.T) {
	previous := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = previous
		modelsCatalogStore.mu.Unlock()
	})

	var catalog staticModelsJSON
	if errUnmarshal := json.Unmarshal(embeddedModelsJSON, &catalog); errUnmarshal != nil {
		t.Fatalf("decode embedded catalog: %v", errUnmarshal)
	}
	for _, model := range catalog.Claude {
		if model != nil && model.Thinking != nil {
			model.Thinking.ZeroAllowed = true
			model.Thinking.DisableType = ""
		}
	}
	stripped, errMarshal := json.Marshal(&catalog)
	if errMarshal != nil {
		t.Fatalf("encode catalog: %v", errMarshal)
	}

	for name, load := range map[string]func([]byte) error{
		"loadModelsFromBytes": func(data []byte) error { return loadModelsFromBytes(data, "test") },
		"publishCatalogBytes": func(data []byte) error { _, err := publishCatalogBytes(data); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if errLoad := load(stripped); errLoad != nil {
				t.Fatalf("load catalog: %v", errLoad)
			}
			for id, fact := range claudeThinkingFacts {
				info := LookupStaticModelInfo(id)
				if info == nil || info.Thinking == nil {
					t.Fatalf("%s missing from catalog", id)
				}
				if fact.disableType != "" && info.Thinking.DisableType != fact.disableType {
					t.Errorf("%s DisableType = %q, want %q", id, info.Thinking.DisableType, fact.disableType)
				}
				if fact.adaptiveOnly && info.Thinking.ZeroAllowed {
					t.Errorf("%s ZeroAllowed = true, want false", id)
				}
			}
		})
	}
}
