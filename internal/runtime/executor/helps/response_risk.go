package helps

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// Response risk values reported through usage.Record.UpstreamRisk.
const (
	// ResponseRiskTrustedAccessForCyber: Codex recommended Trusted Access for Cyber verification
	// (response.metadata openai_verification_recommendation), the Codex CLI's cyber safety signal.
	ResponseRiskTrustedAccessForCyber = "trusted_access_for_cyber"
	// ResponseRiskSafetyBuffering: Codex announced an additional safety check in the stream
	// (response.metadata of type safety_buffering), which websocket responses carry instead of headers.
	ResponseRiskSafetyBuffering = "safety_buffering"
	// responseRiskRefusalPrefix prefixes the category of a Claude refusal, e.g. "refusal_cyber".
	responseRiskRefusalPrefix = "refusal"
	// ResponseRiskPromptBlockedPrefix prefixes a Gemini promptFeedback.blockReason, e.g.
	// "prompt_blocked_prohibited_content": the prompt itself was blocked.
	ResponseRiskPromptBlockedPrefix = "prompt_blocked_"
	// ResponseRiskOutputBlockedPrefix prefixes a Gemini safety finishReason, e.g.
	// "output_blocked_safety": generation was stopped by the safety filter.
	ResponseRiskOutputBlockedPrefix = "output_blocked_"
)

var (
	riskNeedleRefusal       = []byte(`"refusal"`)
	riskNeedleSensitive     = []byte(`"sensitive"`)
	riskNeedleTrustedAccess = []byte(ResponseRiskTrustedAccessForCyber)
	riskNeedleBuffering     = []byte(`"safety_buffering"`)
	riskNeedleBlockReason   = []byte(`"blockReason"`)
	riskNeedleFinishReason  = []byte(`"finishReason"`)
)

// geminiSafetyFinishReasons are the finishReason values of the Gemini safety filters. RECITATION
// (copyright) and ordinary stops are not risk control.
var geminiSafetyFinishReasons = map[string]bool{
	"SAFETY": true, "PROHIBITED_CONTENT": true, "BLOCKLIST": true, "SPII": true,
	"IMAGE_SAFETY": true, "IMAGE_PROHIBITED_CONTENT": true,
}

// ResponseRisk is a safety action an upstream response announced and the reason it gave, if any.
type ResponseRisk struct {
	Kind   string
	Reason string
}

// extractResponseRisk reports the safety action an upstream response frame announces; Kind is
// empty when there is none. A substring check gates the JSON lookups, so ordinary frames cost
// one or two byte scans.
func extractResponseRisk(payload []byte, provider string) ResponseRisk {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		if !bytes.Contains(payload, riskNeedleRefusal) && !bytes.Contains(payload, riskNeedleSensitive) {
			return ResponseRisk{}
		}
		return extractClaudeRefusal(jsonPayload(payload))
	case "codex":
		if !bytes.Contains(payload, riskNeedleTrustedAccess) && !bytes.Contains(payload, riskNeedleBuffering) {
			return ResponseRisk{}
		}
		return ResponseRisk{Kind: extractCodexMetadataRisk(jsonPayload(payload))}
	case "gemini", "gemini-interactions", "vertex", "aistudio", "antigravity":
		// finishReason only appears on the last chunk, so ordinary deltas skip the JSON lookups.
		if !bytes.Contains(payload, riskNeedleBlockReason) && !bytes.Contains(payload, riskNeedleFinishReason) {
			return ResponseRisk{}
		}
		return extractGeminiSafetyBlock(jsonPayload(payload))
	default:
		return ResponseRisk{}
	}
}

// extractClaudeRefusal reads stop_reason refusal (or sensitive) from a non-streamed message or
// from the streamed message_delta, names it by stop_details.category and keeps
// stop_details.explanation as the reason.
func extractClaudeRefusal(data []byte) ResponseRisk {
	if len(data) == 0 {
		return ResponseRisk{}
	}
	root := gjson.ParseBytes(data)
	stop := root.Get("stop_reason")
	details := root.Get("stop_details")
	if root.Get("type").String() == "message_delta" {
		stop = root.Get("delta.stop_reason")
		details = root.Get("delta.stop_details")
	}
	reason := stop.String()
	if reason != "refusal" && reason != "sensitive" {
		return ResponseRisk{}
	}
	category := sanitizeRiskToken(details.Get("category").String())
	if category == "" {
		category = reason
	}
	return ResponseRisk{
		Kind:   responseRiskRefusalPrefix + "_" + category,
		Reason: strings.TrimSpace(details.Get("explanation").String()),
	}
}

// extractCodexMetadataRisk reads the safety fields of a Codex response.metadata frame.
func extractCodexMetadataRisk(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.metadata", "codex.response.metadata":
		metadata := gjson.GetBytes(data, "metadata")
		for _, v := range metadata.Get("openai_verification_recommendation").Array() {
			if v.String() == ResponseRiskTrustedAccessForCyber {
				return ResponseRiskTrustedAccessForCyber
			}
		}
		if metadata.Get("type").String() == "safety_buffering" {
			return ResponseRiskSafetyBuffering
		}
	}
	// The Codex CLI also accepts a top-level safety_buffering object on any event.
	if gjson.GetBytes(data, "safety_buffering").IsObject() {
		return ResponseRiskSafetyBuffering
	}
	return ""
}

// extractGeminiSafetyBlock reads a blocked prompt (promptFeedback.blockReason) or a candidate the
// safety filter stopped (finishReason), from plain or Antigravity-wrapped (response.*) payloads.
// The reason is the upstream message when given, otherwise the blocked safety categories.
func extractGeminiSafetyBlock(data []byte) ResponseRisk {
	if len(data) == 0 {
		return ResponseRisk{}
	}
	root := gjson.ParseBytes(data)
	if wrapped := root.Get("response"); wrapped.IsObject() {
		root = wrapped
	}
	feedback := root.Get("promptFeedback")
	if block := feedback.Get("blockReason").String(); block != "" && block != "BLOCK_REASON_UNSPECIFIED" {
		reason := strings.TrimSpace(feedback.Get("blockReasonMessage").String())
		if reason == "" {
			reason = blockedSafetyRatings(feedback.Get("safetyRatings"))
		}
		return ResponseRisk{Kind: ResponseRiskPromptBlockedPrefix + sanitizeRiskToken(block), Reason: reason}
	}
	for _, candidate := range root.Get("candidates").Array() {
		finish := candidate.Get("finishReason").String()
		if !geminiSafetyFinishReasons[finish] {
			continue
		}
		reason := strings.TrimSpace(candidate.Get("finishMessage").String())
		if reason == "" {
			reason = blockedSafetyRatings(candidate.Get("safetyRatings"))
		}
		return ResponseRisk{Kind: ResponseRiskOutputBlockedPrefix + sanitizeRiskToken(finish), Reason: reason}
	}
	return ResponseRisk{}
}

// blockedSafetyRatings lists the safety categories Gemini blocked on, or rated HIGH.
func blockedSafetyRatings(ratings gjson.Result) string {
	var parts []string
	for _, rating := range ratings.Array() {
		probability := rating.Get("probability").String()
		if rating.Get("blocked").Bool() || probability == "HIGH" {
			parts = append(parts, rating.Get("category").String()+" ("+probability+")")
		}
	}
	return strings.Join(parts, ", ")
}

// sanitizeRiskToken keeps an upstream-controlled label short and limited to [a-z0-9_].
func sanitizeRiskToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}
