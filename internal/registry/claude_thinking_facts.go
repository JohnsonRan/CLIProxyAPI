package registry

// claudeThinkingFact records how the Anthropic API lets a Claude model turn
// thinking off. These are upstream API facts, not catalog choices, so they are
// applied over whichever models catalog is loaded: the embedded copy, the
// release-time refresh, or the runtime remote updater.
type claudeThinkingFact struct {
	// disableType is the thinking.type that turns thinking off when the model
	// rejects "disabled".
	disableType string
	// adaptiveOnly marks models that reject every way of turning thinking off.
	adaptiveOnly bool
}

// ThinkingDisableTypeNone is the DisableType of models that reject every way of
// turning thinking off; turning it off falls back to the lowest level instead.
const ThinkingDisableTypeNone = "none"

var claudeThinkingFacts = map[string]claudeThinkingFact{
	"claude-sonnet-5-5": {disableType: "between_tools"},
	"claude-opus-5-5":   {adaptiveOnly: true},
	"claude-fable-5":    {adaptiveOnly: true},
	"claude-fable-5-1":  {adaptiveOnly: true},
}

// applyClaudeThinkingFacts overlays claudeThinkingFacts onto catalog entries.
// An explicit disable_type in the catalog wins over the built-in value.
func applyClaudeThinkingFacts(models []*ModelInfo) {
	for _, model := range models {
		if model == nil || model.Thinking == nil {
			continue
		}
		fact, ok := claudeThinkingFacts[model.ID]
		if !ok {
			continue
		}
		if fact.disableType != "" && model.Thinking.DisableType == "" {
			model.Thinking.DisableType = fact.disableType
		}
		if fact.adaptiveOnly {
			model.Thinking.ZeroAllowed = false
			if model.Thinking.DisableType == "" {
				model.Thinking.DisableType = ThinkingDisableTypeNone
			}
		}
	}
}
