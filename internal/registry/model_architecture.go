package registry

import "strings"

// openAIModelArchitecture returns the OpenRouter-style architecture object
// advertised in OpenAI model listings. Unknown modalities are omitted rather
// than guessed; nil means nothing is known about the model.
func openAIModelArchitecture(model *ModelInfo) map[string]any {
	if model == nil {
		return nil
	}
	input := normalizeListedValues(model.SupportedInputModalities)
	output := normalizeListedValues(model.SupportedOutputModalities)
	if len(input) == 0 && len(output) == 0 {
		return nil
	}
	architecture := make(map[string]any, 3)
	if len(input) > 0 {
		architecture["input_modalities"] = input
	}
	if len(output) > 0 {
		architecture["output_modalities"] = output
	}
	if len(input) > 0 && len(output) > 0 {
		architecture["modality"] = strings.Join(input, "+") + "->" + strings.Join(output, "+")
	}
	return architecture
}

// Output modality names follow OpenRouter's model listing vocabulary.
const (
	OutputModalityText       = "text"
	OutputModalityImage      = "image"
	OutputModalityEmbeddings = "embeddings"
	OutputModalityRerank     = "rerank"
	OutputModalityDecisions  = "decisions"
)

// IsNonChatOutput reports whether every declared output modality is one that
// no chat client can consume: embeddings, rerank or decisions. Undeclared
// outputs are not treated as non-chat.
func IsNonChatOutput(outputModalities []string) bool {
	output := normalizeListedValues(outputModalities)
	if len(output) == 0 {
		return false
	}
	for _, modality := range output {
		switch modality {
		case OutputModalityEmbeddings, OutputModalityRerank, OutputModalityDecisions:
		default:
			return false
		}
	}
	return true
}

// normalizeListedValues lowercases, trims and de-duplicates listed values.
func normalizeListedValues(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		modality := strings.ToLower(strings.TrimSpace(item))
		if modality == "" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
