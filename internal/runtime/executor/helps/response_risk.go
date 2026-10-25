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
)

var (
	riskNeedleRefusal       = []byte(`"refusal"`)
	riskNeedleSensitive     = []byte(`"sensitive"`)
	riskNeedleTrustedAccess = []byte(ResponseRiskTrustedAccessForCyber)
	riskNeedleBuffering     = []byte(`"safety_buffering"`)
)

// extractResponseRisk reports the safety action an upstream response frame announces, or "".
// A substring check gates the JSON lookups, so ordinary frames cost one or two byte scans.
func extractResponseRisk(payload []byte, provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		if !bytes.Contains(payload, riskNeedleRefusal) && !bytes.Contains(payload, riskNeedleSensitive) {
			return ""
		}
		return extractClaudeRefusal(jsonPayload(payload))
	case "codex":
		if !bytes.Contains(payload, riskNeedleTrustedAccess) && !bytes.Contains(payload, riskNeedleBuffering) {
			return ""
		}
		return extractCodexMetadataRisk(jsonPayload(payload))
	default:
		return ""
	}
}

// extractClaudeRefusal reads stop_reason refusal (or sensitive) from a non-streamed message or
// from the streamed message_delta, and names it by stop_details.category.
func extractClaudeRefusal(data []byte) string {
	if len(data) == 0 {
		return ""
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
		return ""
	}
	category := sanitizeRiskToken(details.Get("category").String())
	if category == "" {
		category = reason
	}
	return responseRiskRefusalPrefix + "_" + category
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
