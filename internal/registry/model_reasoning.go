package registry

import "strings"

// openAIModelReasoning returns the reasoning capability advertised in OpenAI
// model listings. nil means the capability is unknown and must be omitted.
//
// A declared ThinkingSupport is reported as-is: levels keep their wire values
// and budget ranges are reported only when declared. A model without
// ThinkingSupport is reported as not supporting reasoning, because the proxy
// strips reasoning settings for it; user-defined models without an explicit
// thinking configuration are passed through unchanged, so they stay unknown.
func openAIModelReasoning(model *ModelInfo) map[string]any {
	if model == nil || model.DefaultThinking {
		return nil
	}
	support := model.Thinking
	if support == nil {
		if model.UserDefined && !model.ExplicitThinking {
			return nil
		}
		return map[string]any{"supported": false}
	}

	levels := listedReasoningLevels(support.Levels)
	hasBudget := support.Min > 0 || support.Max > 0
	supported := support.Max > 0
	for _, level := range levels {
		if level != "none" {
			supported = true
			break
		}
	}

	reasoning := map[string]any{"supported": supported}
	if len(levels) > 0 {
		reasoning["levels"] = levels
	}
	if hasBudget {
		reasoning["budget"] = map[string]any{
			"min":             support.Min,
			"max":             support.Max,
			"zero_allowed":    support.ZeroAllowed,
			"dynamic_allowed": support.DynamicAllowed,
		}
	}
	return reasoning
}

func listedReasoningLevels(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		level := strings.ToLower(strings.TrimSpace(item))
		if level == "" {
			continue
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		out = append(out, level)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
