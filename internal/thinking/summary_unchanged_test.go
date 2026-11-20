package thinking

import (
	"bytes"
	"testing"
)

// The translator registry and the antigravity translators apply summary intent
// on bytes; once the body already carries it, applying it again must return the
// same body instead of a copy.
func TestApplySummaryConfigReturnsTheBodyWhenAlreadyApplied(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		provider string
		body     string
		config   SummaryConfig
	}{
		{"claude", "claude", "", `{"thinking":{"type":"adaptive"}}`, SummaryConfig{Mode: SummaryEnabled}},
		{"claude off", "claude", "", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, SummaryConfig{Mode: SummaryDisabled}},
		{"gemini", "gemini", "", `{"generationConfig":{}}`, SummaryConfig{Mode: SummaryEnabled}},
		{"antigravity", "antigravity", "", `{"request":{}}`, SummaryConfig{Mode: SummaryDisabled}},
		{"interactions", "interactions", "", `{"generation_config":{}}`, SummaryConfig{Mode: SummaryEnabled}},
		{"responses", "openai-response", "", `{"reasoning":{"effort":"high"}}`, SummaryConfig{Mode: SummaryEnabled, Detail: "detailed"}},
		{"openrouter", "openai", "openrouter", `{"include_reasoning":false}`, SummaryConfig{Mode: SummaryEnabled}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			once := applySummaryConfigForProvider([]byte(tc.body), tc.format, "", tc.provider, nil, tc.config)
			if bytes.Equal(once, []byte(tc.body)) {
				t.Fatalf("first apply changed nothing: %s", once)
			}
			twice := applySummaryConfigForProvider(once, tc.format, "", tc.provider, nil, tc.config)
			if !bytes.Equal(twice, once) {
				t.Fatalf("second apply changed the body: %s, want %s", twice, once)
			}
			if &twice[0] != &once[0] {
				t.Fatalf("second apply copied an unchanged body")
			}
		})
	}
}
