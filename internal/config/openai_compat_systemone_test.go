package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAICompatModelSystemOneFlagRoundTrips(t *testing.T) {
	const yamlConfig = `api-keys:
  openai-compatibility:
    - name: commandcode
      base-url: https://api.commandcode.ai/provider/v1
      keys:
        - api-key: cc-key
      models:
        - name: typesafe/jev
          alias: cc-jev
          systemone: true
        - name: deepseek/deepseek-v4-flash
          alias: flash
`
	cfg, err := ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].Models) != 2 {
		t.Fatalf("openai-compatibility = %+v", cfg.OpenAICompatibility)
	}
	models := cfg.OpenAICompatibility[0].Models
	if !models[0].SystemOne || models[1].SystemOne {
		t.Fatalf("systemone flags = %v/%v, want true/false", models[0].SystemOne, models[1].SystemOne)
	}

	// Management clients read and write the flag through JSON.
	raw, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded []OpenAICompatibilityModel
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !decoded[0].SystemOne || decoded[1].SystemOne {
		t.Fatalf("json round trip lost systemone: %s", raw)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, []byte(yamlConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseConfigBytes(saved)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	if got := reloaded.OpenAICompatibility[0].Models; !got[0].SystemOne || got[1].SystemOne {
		t.Fatalf("saved config lost systemone flags:\n%s", saved)
	}
}
