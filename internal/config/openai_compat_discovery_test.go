package config

import (
	"reflect"
	"testing"
)

func TestOpenAICompatibilityDiscoveryFieldsSurviveV8Layout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
api-keys:
  openai-compatibility:
    - name: discover
      base-url: https://api.example.com/v1
      auto-discover-models: true
      excluded-models:
        - " GLM-4.* "
        - "*-preview"
      keys:
        - api-key: sk-test
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("OpenAICompatibility len = %d, want 1", len(cfg.OpenAICompatibility))
	}
	compat := cfg.OpenAICompatibility[0]
	if !compat.AutoDiscoverModels {
		t.Fatal("auto-discover-models was dropped by the v8 layout")
	}
	cfg.SanitizeOpenAICompatibility()
	compat = cfg.OpenAICompatibility[0]
	if want := []string{"glm-4.*", "*-preview"}; !reflect.DeepEqual(compat.ExcludedModels, want) {
		t.Fatalf("excluded-models = %#v, want %#v", compat.ExcludedModels, want)
	}
	if len(compat.APIKeyEntries) != 1 || compat.APIKeyEntries[0].APIKey != "sk-test" {
		t.Fatalf("api key entries = %#v", compat.APIKeyEntries)
	}
}
