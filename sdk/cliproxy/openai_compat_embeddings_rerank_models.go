package cliproxy

import "github.com/router-for-me/CLIProxyAPI/v8/internal/config"

// embeddingRerankOutputModalities declares embedding and rerank models with
// their OpenRouter output modality names ("embeddings", "rerank") so model
// listings can tell them apart from chat models. Explicit output-modalities
// configuration wins.
func embeddingRerankOutputModalities(model config.OpenAICompatibilityModel) []string {
	if len(model.OutputModalities) > 0 {
		return model.OutputModalities
	}
	var out []string
	if model.Embedding {
		out = append(out, "embeddings")
	}
	if model.Rerank {
		out = append(out, "rerank")
	}
	return out
}
