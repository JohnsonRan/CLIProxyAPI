package models

import "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"

// hideCodexClientNonChatModel hides embedding, rerank and decision models,
// which Codex cannot call, the same way image and speech models are hidden.
func hideCodexClientNonChatModel(entry map[string]any, model map[string]any) {
	if registry.IsNonChatOutput(listedOutputModalities(model)) {
		entry["visibility"] = "hide"
	}
}

// listedOutputModalities returns architecture.output_modalities from a model
// listing entry, accepting both registry ([]string) and decoded JSON ([]any)
// forms. It returns nil when the listing does not declare outputs.
func listedOutputModalities(model map[string]any) []string {
	architecture, ok := model["architecture"].(map[string]any)
	if !ok {
		return nil
	}
	switch raw := architecture["output_modalities"].(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			if modality, okString := item.(string); okString {
				out = append(out, modality)
			}
		}
		return out
	default:
		return nil
	}
}
