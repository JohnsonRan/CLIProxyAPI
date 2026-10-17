package executor

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// largeClaudeCodeBody resembles a long Claude Code turn: output_config is
// present, so the title-helper pre-filter lets the request through to the
// full walks, and messages carry the whole conversation.
func largeClaudeCodeBody(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5-5","max_tokens":32000,"output_config":{"effort":"high"},`)
	b.WriteString(`"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],`)
	b.WriteString(`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],"messages":[`)
	text := strings.Repeat("lorem ipsum ", 400)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"text","text":"%d %s"}]},`, i, text)
		fmt.Fprintf(&b, `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":"ls"}}]},`, i)
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"%s"}]}`, i, text)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// Each read-only scan of a large Claude request used to copy the scanned
// array (usually messages) through gjson.GetBytes; they now read in place.
func TestClaudeRequestReadOnlyScansDoNotCopyTheBody(t *testing.T) {
	body := largeClaudeCodeBody(100)
	scans := map[string]func(){
		"IsClaudeProbeOrHelperRequest":        func() { helps.IsClaudeProbeOrHelperRequest(body) },
		"IsClaudeNewPromptTurn":               func() { helps.IsClaudeNewPromptTurn(body) },
		"firstClaudeUserMessageIndex":         func() { firstClaudeUserMessageIndex(body) },
		"countCacheControls":                  func() { countCacheControls(body) },
		"claudeBodyHasSignedThinkingBlock":    func() { claudeBodyHasSignedThinkingBlock(body) },
		"claudeIncludeInlineTools":            func() { claudeIncludeInlineTools(body, nil) },
		"claudeHistoryHasAdvisorCallOrResult": func() { claudeHistoryHasAdvisorCallOrResult(body) },
	}
	const runs = 5
	for name, scan := range scans {
		scan()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			scan()
		}
		runtime.ReadMemStats(&after)
		allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs
		// Per-element bookkeeping (Result slices) is fine; a copy of the
		// conversation is not.
		if allocated > int64(len(body)/4) {
			t.Errorf("%s allocates %d bytes per call on a %d byte body", name, allocated, len(body))
		}
	}
}

func BenchmarkClaudeRequestReadOnlyScans(b *testing.B) {
	body := largeClaudeCodeBody(100)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		helps.IsClaudeProbeOrHelperRequest(body)
		helps.IsClaudeNewPromptTurn(body)
		firstClaudeUserMessageIndex(body)
		countCacheControls(body)
		claudeBodyHasSignedThinkingBlock(body)
		claudeIncludeInlineTools(body, nil)
		claudeHistoryHasAdvisorCallOrResult(body)
	}
}
