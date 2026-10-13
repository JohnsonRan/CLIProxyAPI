package helps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// IsGeminiEmbeddingRequest checks if the request is an embedding request targeted at Gemini.
func IsGeminiEmbeddingRequest(sourceFormat string, requestPath string) bool {
	if sourceFormat == "openai-embedding" {
		return true
	}
	return strings.HasSuffix(requestPath, "/embeddings") || strings.HasSuffix(requestPath, "/embedding")
}

// ConvertOpenAIEmbeddingToGemini translates an OpenAI /v1/embeddings request payload
// to Gemini :embedContent or :batchEmbedContents request payload and action name.
func ConvertOpenAIEmbeddingToGemini(model string, payload []byte) (action string, body []byte, isBatch bool, err error) {
	inputVal := gjson.GetBytes(payload, "input")
	if !inputVal.Exists() {
		return "", nil, false, fmt.Errorf("missing input parameter")
	}

	normalizedModel := strings.TrimPrefix(model, "models/")

	if inputVal.IsArray() {
		arr := inputVal.Array()
		requests := make([]map[string]any, 0, len(arr))
		for _, item := range arr {
			requests = append(requests, map[string]any{
				"model": "models/" + normalizedModel,
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": item.String()},
					},
				},
			})
		}
		batchPayload, errMarshal := json.Marshal(map[string]any{
			"requests": requests,
		})
		if errMarshal != nil {
			return "", nil, false, errMarshal
		}
		return "batchEmbedContents", batchPayload, true, nil
	}

	singlePayload, errMarshal := json.Marshal(map[string]any{
		"content": map[string]any{
			"parts": []map[string]any{
				{"text": inputVal.String()},
			},
		},
	})
	if errMarshal != nil {
		return "", nil, false, errMarshal
	}
	return "embedContent", singlePayload, false, nil
}

// ConvertGeminiEmbeddingToOpenAI translates Gemini :embedContent or :batchEmbedContents
// response to standard OpenAI /v1/embeddings response.
func ConvertGeminiEmbeddingToOpenAI(model string, geminiResp []byte, isBatch bool) ([]byte, error) {
	type embeddingItem struct {
		Object    string    `json:"object"`
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	}
	type openAIEmbeddingResponse struct {
		Object string          `json:"object"`
		Data   []embeddingItem `json:"data"`
		Model  string          `json:"model"`
		Usage  map[string]int  `json:"usage"`
	}

	data := make([]embeddingItem, 0)
	totalTokens := 0

	if isBatch {
		embeddingsVal := gjson.GetBytes(geminiResp, "embeddings")
		if embeddingsVal.IsArray() {
			for i, emb := range embeddingsVal.Array() {
				vals := emb.Get("values").Array()
				floats := make([]float64, len(vals))
				for j, v := range vals {
					floats[j] = v.Float()
				}
				data = append(data, embeddingItem{
					Object:    "embedding",
					Embedding: floats,
					Index:     i,
				})
				totalTokens += len(floats)
			}
		}
	} else {
		vals := gjson.GetBytes(geminiResp, "embedding.values").Array()
		floats := make([]float64, len(vals))
		for j, v := range vals {
			floats[j] = v.Float()
		}
		data = append(data, embeddingItem{
			Object:    "embedding",
			Embedding: floats,
			Index:     0,
		})
		totalTokens = len(floats)
	}

	resp := openAIEmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  model,
		Usage: map[string]int{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	}
	return json.Marshal(resp)
}
