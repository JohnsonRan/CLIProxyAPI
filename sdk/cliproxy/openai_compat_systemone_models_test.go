package cliproxy

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildOpenAICompatibilityConfigModels_SystemOneOutputModalities(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "typesafe",
		Models: []config.OpenAICompatibilityModel{
			{Name: "upstream-chat", Alias: "chat"},
			{Name: "jev-latest", Alias: "typesafe/jev", SystemOne: true},
			{Name: "upstream-explicit", Alias: "explicit", SystemOne: true, OutputModalities: []string{"text"}},
		},
	}
	want := map[string]string{
		"chat":         "",
		"typesafe/jev": "decisions",
		"explicit":     "text",
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
		if got := strings.Join(model.SupportedOutputModalities, ","); got != expected {
			t.Fatalf("%s SupportedOutputModalities = %q, want %q", model.ID, got, expected)
		}
	}
	if compat.Models[1].OutputModalities != nil {
		t.Fatalf("configuration must not be mutated: %+v", compat.Models[1].OutputModalities)
	}
}
