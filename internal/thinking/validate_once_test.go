package thinking

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// legacyExtractCodexUsageConfig is extractCodexUsageConfig before it reused
// its own validation for the configuration_update scan.
func legacyExtractCodexUsageConfig(body []byte) ThinkingConfig {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ThinkingConfig{}
	}
	if config := extractConfigurationUpdateConfig(body); hasThinkingConfig(config) {
		return config
	}
	return extractCodexConfig(body)
}

// legacyExtractReasoningEffort is ExtractReasoningEffort before the Responses
// path validated the body once. It goes through extractThinkingConfigForUsage
// like the original, so a later change to that mapping shows up here.
func legacyExtractReasoningEffort(body []byte, provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if isResponsesFormat(provider) {
		if effort := reasoningEffortFromConfig(extractConfigurationUpdateConfig(body)); effort != "" {
			return effort
		}
	}
	if effort := reasoningEffortFromSuffix(ParseSuffix(model)); effort != "" {
		return effort
	}
	config := extractThinkingConfigForUsage(body, provider)
	if !hasThinkingConfig(config) {
		switch provider {
		case "openai-response", "openai":
			config = legacyExtractCodexUsageConfig(body)
		}
	}
	return reasoningEffortFromConfig(config)
}

// legacyExtractTranslatedReasoningEffort is ExtractTranslatedReasoningEffort
// before it stopped reading the Responses fields twice.
func legacyExtractTranslatedReasoningEffort(body []byte, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	config := extractThinkingConfigForUsage(body, provider)
	if !hasThinkingConfig(config) {
		switch provider {
		case "openai", "openai-response":
			config = legacyExtractCodexUsageConfig(body)
			if !hasThinkingConfig(config) {
				config = extractOpenAIConfig(body)
			}
		}
	}
	return reasoningEffortFromConfig(config)
}

func validateOnceBodies() map[string][]byte {
	bodies := map[string]string{
		"empty":                ``,
		"not-json":             `not json`,
		"truncated":            `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}}`,
		"truncated-top-effort": `{"reasoning_effort":"high","reasoning":{"effort":"medium"`,
		"object-empty":         `{}`,
		"array":                `[{"type":"configuration_update","reasoning":{"effort":"low"}}]`,
		"top-effort":           `{"reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":"hi"}]}`,
		"top-effort-none":      `{"reasoning":{"effort":"none"}}`,
		"top-effort-number":    `{"reasoning":{"effort":3}}`,
		"chat-effort":          `{"reasoning_effort":"medium","messages":[]}`,
		"chat-and-responses":   `{"reasoning_effort":"low","reasoning":{"effort":"xhigh"}}`,
		"update":               `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}}]}`,
		"updates-last-wins":    `{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"message"},{"type":"configuration_update","reasoning":{"effort":" High "}}]}`,
		"update-empty-effort":  `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"  "}}]}`,
		"update-none":          `{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","reasoning":{"effort":"none"}}]}`,
		"update-auto":          `{"input":[{"type":"configuration_update","reasoning":{"effort":"AUTO"}}]}`,
		"update-number":        `{"reasoning":{"effort":"low"},"input":[{"type":"configuration_update","reasoning":{"effort":5}}]}`,
		"input-string":         `{"reasoning":{"effort":"low"},"input":"configuration_update"}`,
		"claude-budget":        `{"thinking":{"type":"enabled","budget_tokens":8192}}`,
		"claude-adaptive":      `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`,
		"claude-disabled":      `{"thinking":{"type":"disabled"}}`,
		"claude-between-tools": `{"thinking":{"type":"between_tools"}}`,
		"gemini-level":         `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`,
		"gemini-budget":        `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`,
		"kimi":                 `{"thinking":{"type":"enabled","effort":"low"}}`,
		"duplicate-keys":       `{"reasoning":{"effort":"low"},"reasoning":{"effort":"high"},"input":[],"input":[{"type":"configuration_update","reasoning":{"effort":"medium"}}]}`,
	}
	out := make(map[string][]byte, len(bodies)+1)
	for name, body := range bodies {
		out[name] = []byte(body)
	}
	out["nil"] = nil
	return out
}

var validateOnceProviders = []string{
	"codex", "openai-response", " OpenAI-Response ", "CODEX", "openai", "claude", "gemini",
	"antigravity", "interactions", "xai", "kimi", "unknown", "",
}

var validateOnceModels = []string{
	"", "gpt-6", "gpt-6(high)", "gpt-6(none)", "gpt-6(auto)", "gpt-6(8192)", "gpt-6(0)", "gpt-6(bogus)",
}

func TestExtractReasoningEffortMatchesLegacy(t *testing.T) {
	for name, body := range validateOnceBodies() {
		for _, provider := range validateOnceProviders {
			for _, model := range validateOnceModels {
				got := ExtractReasoningEffort(body, provider, model)
				want := legacyExtractReasoningEffort(body, provider, model)
				if got != want {
					t.Errorf("ExtractReasoningEffort(%s, %q, %q) = %q, want %q", name, provider, model, got, want)
				}
			}
		}
	}
}

func TestExtractTranslatedReasoningEffortMatchesLegacy(t *testing.T) {
	for name, body := range validateOnceBodies() {
		for _, provider := range validateOnceProviders {
			got := ExtractTranslatedReasoningEffort(body, provider)
			want := legacyExtractTranslatedReasoningEffort(body, provider)
			if got != want {
				t.Errorf("ExtractTranslatedReasoningEffort(%s, %q) = %q, want %q", name, provider, got, want)
			}
		}
	}
}

func TestExtractCodexUsageConfigMatchesLegacy(t *testing.T) {
	for name, body := range validateOnceBodies() {
		got := extractCodexUsageConfig(body)
		want := legacyExtractCodexUsageConfig(body)
		if got != want {
			t.Errorf("extractCodexUsageConfig(%s) = %+v, want %+v", name, got, want)
		}
		if got, want := configurationUpdateConfig(body, true), extractConfigurationUpdateConfig(body); gjson.ValidBytes(body) && got != want {
			t.Errorf("configurationUpdateConfig(%s, true) = %+v, want %+v", name, got, want)
		}
	}
}

// largeResponsesBody builds a Codex-style request with a long input history
// and a trailing configuration_update.
func largeResponsesBody(turns int, update bool) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-6","stream":true,"instructions":"` + strings.Repeat("be helpful ", 400) + `","input":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"type":"message","role":"user","content":[{"type":"input_text","text":"%s %d"}]},`, strings.Repeat("some text here ", 70), i)
		fmt.Fprintf(&sb, `{"type":"function_call","name":"shell","call_id":"c%d","arguments":"{\"cmd\":\"ls\"}"},`, i)
		fmt.Fprintf(&sb, `{"type":"function_call_output","call_id":"c%d","output":"%s"}`, i, strings.Repeat("file.go ", 120))
	}
	if update {
		sb.WriteString(`,{"type":"configuration_update","reasoning":{"effort":"low"}}`)
	}
	sb.WriteString(`],"reasoning":{"effort":"high","summary":"auto"}}`)
	return []byte(sb.String())
}

func BenchmarkExtractReasoningEffortResponses(b *testing.B) {
	for _, update := range []bool{false, true} {
		body := largeResponsesBody(450, update)
		for _, provider := range []string{"openai-response", "codex"} {
			b.Run(fmt.Sprintf("%s/update=%t", provider, update), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = ExtractReasoningEffort(body, provider, "gpt-6")
				}
			})
		}
	}
}

func BenchmarkExtractTranslatedReasoningEffortResponses(b *testing.B) {
	body := largeResponsesBody(450, false)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ExtractTranslatedReasoningEffort(body, "openai-response")
	}
}

func BenchmarkExtractCodexUsageConfig(b *testing.B) {
	body := largeResponsesBody(450, false)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = extractCodexUsageConfig(body)
	}
}
