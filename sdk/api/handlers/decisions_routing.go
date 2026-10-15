package handlers

import "strings"

// OpenAIDecisionsEntryProtocol is the entry protocol of POST /v1/decisions.
// It must match helps.OpenAIDecisionsFormat in the executors.
const OpenAIDecisionsEntryProtocol = "openai-decisions"

// openAIDecisionsExecutionProviders keeps the providers whose executors forward
// the Decisions API: codex (codex-api-key) and openai-compatibility entries.
// Other executors would try to translate the body as a chat request, fail, and
// cool down a model they serve for chat.
func openAIDecisionsExecutionProviders(providers []string) []string {
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "codex" || name == "openai-compatibility" || strings.HasPrefix(name, "openai-compatible-") {
			out = append(out, provider)
		}
	}
	return out
}
