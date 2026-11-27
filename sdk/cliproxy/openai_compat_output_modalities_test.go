package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestModelCapabilitiesCompatOutputModalities(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "compat",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-chat", Alias: "chat"},
			{Name: "upstream-image", Alias: "image", Image: true},
			{Name: "upstream-image-text", Alias: "image-text", Image: true, OutputModalities: []string{"TEXT", "image"}},
		},
	}

	want := map[string]string{
		"chat":       "",
		"image":      "image",
		"image-text": "text,image",
	}
	models := buildOpenAICompatibilityConfigModels(compat)
	if len(models) != len(want) {
		t.Fatalf("model count = %d, want %d", len(models), len(want))
	}
	for _, model := range models {
		expected, ok := want[model.ID]
		if !ok {
			t.Fatalf("unexpected model %q", model.ID)
		}
		if got := joinModalities(model.SupportedOutputModalities); got != expected {
			t.Fatalf("%s SupportedOutputModalities = %q, want %q", model.ID, got, expected)
		}
	}
}

func TestModelCapabilitiesCompatDefaultThinkingIsMarked(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "compat",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-default", Alias: "default"},
			{Name: "upstream-explicit", Alias: "explicit", Thinking: &registry.ThinkingSupport{Levels: []string{"high"}}},
			{Name: "upstream-image", Alias: "image", Image: true},
		},
	}

	type flags struct{ explicit, fallback bool }
	want := map[string]flags{
		"default":  {explicit: false, fallback: true},
		"explicit": {explicit: true, fallback: false},
		"image":    {explicit: false, fallback: false},
	}
	for _, model := range buildOpenAICompatibilityConfigModels(compat) {
		expected, ok := want[model.ID]
		if !ok {
			t.Fatalf("unexpected model %q", model.ID)
		}
		got := flags{explicit: model.ExplicitThinking, fallback: model.DefaultThinking}
		if got != expected {
			t.Fatalf("%s thinking flags = %+v, want %+v", model.ID, got, expected)
		}
	}
}
