package claude

import (
	"bytes"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// The Responses-to-Claude translator already writes the thinking block the
// applier asks for; applying it again must not copy the conversation.
func TestApplyLeavesAnAlreadyAppliedBodyUncopied(t *testing.T) {
	history := strings.Repeat(`{"role":"user","content":"x"},`, 2000)
	adaptive := &registry.ModelInfo{ID: "claude-opus-5-5", Thinking: &registry.ThinkingSupport{Min: 1024, Max: 64000, Levels: []string{"low", "medium", "high"}}}
	budget := &registry.ModelInfo{ID: "claude-sonnet-4-5", MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{Min: 1024, Max: 64000}}
	cases := []struct {
		name   string
		body   string
		config thinking.ThinkingConfig
		model  *registry.ModelInfo
	}{
		{"adaptive level", `{"messages":[` + history + `{}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
			thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: "high"}, adaptive},
		{"budget", `{"max_tokens":32000,"messages":[` + history + `{}],"thinking":{"type":"enabled","budget_tokens":8192}}`,
			thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 8192}, budget},
	}
	for _, tc := range cases {
		body := []byte(tc.body)
		got, err := NewApplier().Apply(body, tc.config, tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("%s: body changed:\n%s", tc.name, got[len(got)-120:])
		}
		if &got[0] != &body[0] {
			t.Errorf("%s: unchanged body was copied", tc.name)
		}
	}
}
