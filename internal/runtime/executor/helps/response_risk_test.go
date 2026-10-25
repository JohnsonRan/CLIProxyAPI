package helps

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestExtractResponseRisk(t *testing.T) {
	cases := []struct {
		name, provider, payload, want string
	}{
		{"claude stream refusal", "claude", `data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"}}}`, "refusal_cyber"},
		{"claude message refusal", "claude", `{"type":"message","stop_reason":"refusal","stop_details":{"category":"Bio Weapons"}}`, "refusal_bio_weapons"},
		{"claude sensitive without details", "claude", `{"type":"message_delta","delta":{"stop_reason":"sensitive"}}`, "refusal_sensitive"},
		{"claude end_turn", "claude", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, ""},
		{"claude text mentioning refusal", "claude", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"\"refusal\""}}`, ""},
		{"claude refusal string elsewhere", "claude", `{"type":"content_block_start","content_block":{"type":"refusal"}}`, ""},
		{"codex trusted access", "codex", `{"type":"response.metadata","metadata":{"openai_verification_recommendation":["trusted_access_for_cyber"]}}`, ResponseRiskTrustedAccessForCyber},
		{"codex websocket metadata", "codex", `{"type":"codex.response.metadata","metadata":{"openai_verification_recommendation":["other","trusted_access_for_cyber"]}}`, ResponseRiskTrustedAccessForCyber},
		{"codex buffering metadata", "codex", `{"type":"response.metadata","metadata":{"type":"safety_buffering","retry_model":"gpt-x"}}`, ResponseRiskSafetyBuffering},
		{"codex buffering field", "codex", `{"type":"response.in_progress","safety_buffering":{"faster_model":"gpt-x"}}`, ResponseRiskSafetyBuffering},
		{"codex moderation only", "codex", `{"type":"response.metadata","metadata":{"openai_chatgpt_moderation_metadata":{"prompt":{}}}}`, ""},
		{"codex text mentioning the flag", "codex", `{"type":"response.output_text.delta","delta":"trusted_access_for_cyber"}`, ""},
		{"other provider", "openai", `{"stop_reason":"refusal"}`, ""},
		{"gemini blocked prompt", "gemini", `{"promptFeedback":{"blockReason":"SAFETY"}}`, "prompt_blocked_safety"},
		{"antigravity wrapped block", "antigravity", `{"response":{"promptFeedback":{"blockReason":"OTHER"}}}`, "prompt_blocked_other"},
		{"vertex safety stop", "vertex", `data: {"candidates":[{"finishReason":"PROHIBITED_CONTENT","finishMessage":"m"}]}`, "output_blocked_prohibited_content"},
		{"gemini ordinary stop", "gemini", `{"candidates":[{"finishReason":"STOP"}]}`, ""},
		{"gemini recitation is not risk", "aistudio", `{"candidates":[{"finishReason":"RECITATION"}]}`, ""},
		{"gemini unspecified block", "gemini", `{"promptFeedback":{"blockReason":"BLOCK_REASON_UNSPECIFIED"}}`, ""},
		{"gemini delta", "gemini", `{"candidates":[{"content":{"parts":[{"text":"SAFETY"}]}}]}`, ""},
	}
	for _, tc := range cases {
		if got := extractResponseRisk([]byte(tc.payload), tc.provider).Kind; got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The refusal arrives in message_delta and Codex metadata can follow response.completed; both
// must still reach the record after the response model is final, and the first signal wins.
func TestUsageReporterKeepsFirstResponseRiskAfterTerminalEvent(t *testing.T) {
	reporter := NewUsageReporter(context.Background(), "codex", "gpt-x", nil)
	for _, line := range []string{
		`{"type":"response.completed","response":{"model":"gpt-x"}}`,
		`{"type":"response.metadata","metadata":{"openai_verification_recommendation":["trusted_access_for_cyber"]}}`,
		`{"type":"response.metadata","metadata":{"type":"safety_buffering"}}`,
	} {
		reporter.ObserveResponseModel([]byte(line))
	}
	if got := reporter.buildRecordForModel("gpt-x", usage.Detail{}, false, usage.Failure{}).UpstreamRisk; got != ResponseRiskTrustedAccessForCyber {
		t.Fatalf("UpstreamRisk = %q, want %q", got, ResponseRiskTrustedAccessForCyber)
	}
	claude := extractResponseRisk([]byte(`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":" blocked under the Usage Policy "}}}`), "claude")
	if claude.Reason != "blocked under the Usage Policy" {
		t.Fatalf("claude reason = %q", claude.Reason)
	}
	if got := reporter.buildRecordForModel("gpt-image-x", usage.Detail{}, false, usage.Failure{}).UpstreamRisk; got != "" {
		t.Fatalf("additional-model records must not carry the risk, got %q", got)
	}
}
