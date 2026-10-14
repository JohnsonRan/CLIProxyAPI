package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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
