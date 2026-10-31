package usagestore

import (
	"strings"
	"sync/atomic"
	"unicode/utf8"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// Risk values recorded on events the upstream flagged for safety reasons. Besides these and the
// riskErrorCodes, the executor reports refusal_<category> (Claude stop_reason refusal) and
// trusted_access_for_cyber (Codex response.metadata) through Record.UpstreamRisk.
const (
	// RiskModelReroute: Codex served another model than requested. The Codex CLI reports every
	// such OpenAI-Model mismatch as a reroute for high-risk cyber activity.
	RiskModelReroute = "model_reroute"
	// RiskSafetyBuffering: Codex held the response for an additional safety check.
	RiskSafetyBuffering = "safety_buffering"
)

// riskPromptsOff mirrors observability.usage.store.risk-prompts.disable.
var riskPromptsOff atomic.Bool

// riskErrorCodes are upstream rejections of the prompt itself; the code is stored as the risk.
var riskErrorCodes = []string{"cyber_policy", "bio_policy", "invalid_prompt"}

const (
	// maxRiskPromptBytes caps the prompt kept with a flagged event.
	maxRiskPromptBytes = 32 << 10
	// riskPromptHeadBytes is how much of the turn start (the user message) survives truncation;
	// the rest of the budget keeps the turn end, where the triggering tool output usually is.
	riskPromptHeadBytes = 8 << 10
	// maxRiskReasonBytes caps the upstream explanation kept with a flagged event.
	maxRiskReasonBytes = 2 << 10
)

// classifyRisk names the upstream safety action behind a Codex, Claude or Gemini-family record and
// the reason the upstream gave, or returns "" for other providers and unflagged requests.
func classifyRisk(record coreusage.Record, substituted bool) (risk, reason string) {
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	switch provider {
	case "codex", "claude", "gemini", "gemini-interactions", "vertex", "aistudio", "antigravity":
	default:
		return "", ""
	}
	if record.Failed || record.Fail.StatusCode >= 400 {
		for _, code := range riskErrorCodes {
			if strings.Contains(record.Fail.Body, `"`+code+`"`) {
				return code, errorMessage(record.Fail.Body)
			}
		}
	}
	// Signals the response announced: Claude refusals (refusal_<category>), Codex
	// trusted_access_for_cyber and in-stream safety_buffering, Gemini prompt_blocked_<reason>
	// and output_blocked_<reason>.
	if upstream := strings.TrimSpace(record.UpstreamRisk); upstream != "" {
		return upstream, truncate(strings.TrimSpace(record.UpstreamRiskReason), maxRiskReasonBytes)
	}
	if provider != "codex" {
		return "", ""
	}
	if substituted {
		return RiskModelReroute, ""
	}
	if strings.EqualFold(record.ResponseHeaders.Get("X-Codex-Safety-Buffering-Enabled"), "true") {
		return RiskSafetyBuffering, ""
	}
	return "", ""
}

// errorMessage returns the upstream message of a rejection body, or "" when it has none.
func errorMessage(body string) string {
	for _, path := range []string{"error.message", "message", "response.error.message"} {
		if m := strings.TrimSpace(gjson.Get(body, path).String()); m != "" {
			return truncate(m, maxRiskReasonBytes)
		}
	}
	return ""
}

// riskClause is the SQL condition, with a leading AND, for the risk result filter.
func riskClause(flagged bool) string {
	if flagged {
		return " AND risk != ''"
	}
	return " AND risk = ''"
}

// turnPrompt renders the current turn of a client request as plain text: from the last user
// message that carries text to the end of the conversation, including tool calls and outputs.
// It understands OpenAI Responses and Chat Completions, Claude and Gemini bodies; anything else
// is kept as raw text. The result is capped at maxRiskPromptBytes.
func turnPrompt(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if !gjson.ValidBytes(payload) {
		return clipMiddle(string(payload), maxRiskPromptBytes)
	}
	root := gjson.ParseBytes(payload)
	var items gjson.Result
	for _, path := range []string{"input", "messages", "contents", "request.contents"} {
		if v := root.Get(path); v.Exists() {
			items = v
			break
		}
	}
	if items.Type == gjson.String {
		return clipMiddle(items.Str, maxRiskPromptBytes)
	}
	if !items.IsArray() {
		if p := root.Get("prompt"); p.Type == gjson.String {
			return clipMiddle(p.Str, maxRiskPromptBytes)
		}
		return clipMiddle(string(payload), maxRiskPromptBytes)
	}
	list := items.Array()
	start := 0
	for i := len(list) - 1; i >= 0; i-- {
		if isUserText(list[i]) {
			start = i
			break
		}
	}
	var sb strings.Builder
	for _, item := range list[start:] {
		label := item.Get("role").String()
		if label == "" {
			label = item.Get("type").String()
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("[" + label + "]\n")
		collectText(item, &sb)
	}
	return clipMiddle(strings.TrimSpace(sb.String()), maxRiskPromptBytes)
}

// isUserText reports whether an item is a user message with text, as opposed to a user-role
// item that only returns tool results (Claude tool_result, Gemini functionResponse).
func isUserText(item gjson.Result) bool {
	if item.Get("role").String() != "user" {
		return false
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		return strings.TrimSpace(content.Str) != ""
	}
	for _, part := range append(content.Array(), item.Get("parts").Array()...) {
		switch part.Get("type").String() {
		case "text", "input_text", "":
			if strings.TrimSpace(part.Get("text").String()) != "" {
				return true
			}
		}
	}
	return false
}

// collectText appends the human-readable text of a message, part or tool item to sb and skips
// opaque fields such as encrypted reasoning, signatures and inline media.
func collectText(v gjson.Result, sb *strings.Builder) {
	switch {
	case v.Type == gjson.String:
		writeLine(sb, v.Str)
	case v.IsArray():
		v.ForEach(func(_, x gjson.Result) bool {
			collectText(x, sb)
			return true
		})
	case v.IsObject():
		if name := v.Get("name").String(); name != "" {
			for _, key := range []string{"arguments", "input", "args"} {
				if args := v.Get(key); args.Exists() {
					writeLine(sb, "-> "+name+": "+rawOrString(args))
					break
				}
			}
		}
		v.ForEach(func(k, x gjson.Result) bool {
			switch k.Str {
			case "text", "content", "output", "refusal", "summary", "thinking", "parts",
				"tool_calls", "function", "functionCall", "functionResponse":
				collectText(x, sb)
			case "response":
				writeLine(sb, rawOrString(x))
			}
			return true
		})
	}
}

func rawOrString(v gjson.Result) string {
	if v.Type == gjson.String {
		return v.Str
	}
	return v.Raw
}

func writeLine(sb *strings.Builder, s string) {
	if s = strings.TrimSpace(s); s != "" {
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
}

// clipMiddle keeps the first riskPromptHeadBytes and the last bytes of s within max, cutting
// on UTF-8 boundaries and marking the gap.
func clipMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const marker = "\n...[truncated]...\n"
	head := truncate(s, riskPromptHeadBytes)
	tail := s[len(s)-(max-len(head)-len(marker)):]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return head + marker + tail
}
