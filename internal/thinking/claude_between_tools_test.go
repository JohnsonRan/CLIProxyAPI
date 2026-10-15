package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
)

func TestClaudeThinkingOffUsesBetweenToolsOrAdaptiveFallback(t *testing.T) {
	t.Run("claude-sonnet-5-5 uses between_tools when thinking is turned off", func(t *testing.T) {
		out, err := ApplyThinking([]byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet-5-5(none)", "claude", "claude", "claude")
		if err != nil {
			t.Fatalf("ApplyThinking error: %v", err)
		}
		if got := gjson.GetBytes(out, "thinking.type").String(); got != "between_tools" {
			t.Fatalf("thinking.type = %q, want between_tools; body=%s", got, out)
		}
		if gjson.GetBytes(out, "thinking.display").Exists() {
			t.Fatalf("thinking.display should be removed for between_tools; body=%s", out)
		}
		if gjson.GetBytes(out, "output_config.effort").Exists() {
			t.Fatalf("output_config.effort should be removed for between_tools; body=%s", out)
		}
	})

	t.Run("claude-opus-5-5 falls back to adaptive low with omitted display when thinking is turned off", func(t *testing.T) {
		out, err := ApplyThinking([]byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`), "claude-opus-5-5(none)", "claude", "claude", "claude")
		if err != nil {
			t.Fatalf("ApplyThinking error: %v", err)
		}
		if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
			t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, out)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "low" {
			t.Fatalf("output_config.effort = %q, want low; body=%s", got, out)
		}
		if got := gjson.GetBytes(out, "thinking.display").String(); got != "omitted" {
			t.Fatalf("thinking.display = %q, want omitted; body=%s", got, out)
		}
	})

	t.Run("LookupStaticModelInfo resolves dated and bracketed Claude variants", func(t *testing.T) {
		for _, id := range []string{"claude-sonnet-5-5-20261001", "claude-sonnet-5-5[1m]"} {
			info := registry.LookupStaticModelInfo(id)
			if info == nil || info.Thinking == nil || info.Thinking.DisableType != "between_tools" {
				t.Fatalf("LookupStaticModelInfo(%q) = %#v, want DisableType=between_tools", id, info)
			}
		}
	})
}

// Only models with the explicit adaptive-only fact fall back to a level when
// thinking is turned off; other Claude-format models without a "none" level
// keep thinking.type="disabled".
func TestClaudeThinkingOffFallbackRequiresAdaptiveOnlyFact(t *testing.T) {
	levels := []string{"low", "medium", "high"}
	off := ThinkingConfig{Mode: ModeNone}

	thirdParty := &registry.ModelInfo{ID: "my-claude", Type: "claude", Thinking: &registry.ThinkingSupport{Levels: levels}}
	got, errValidate := ValidateConfig(off, thirdParty, "claude", "claude", false)
	if errValidate != nil {
		t.Fatalf("ValidateConfig(third-party) error: %v", errValidate)
	}
	if got.Mode != ModeNone {
		t.Fatalf("third-party model mode = %v, want ModeNone", got.Mode)
	}

	adaptiveOnly := &registry.ModelInfo{ID: "claude-opus-5-5", Type: "claude", Thinking: &registry.ThinkingSupport{Levels: levels, DisableType: registry.ThinkingDisableTypeNone}}
	got, errValidate = ValidateConfig(off, adaptiveOnly, "claude", "claude", false)
	if errValidate != nil {
		t.Fatalf("ValidateConfig(adaptive-only) error: %v", errValidate)
	}
	if got.Mode != ModeLevel || got.Level != "low" {
		t.Fatalf("adaptive-only model = %+v, want ModeLevel low", got)
	}
}
