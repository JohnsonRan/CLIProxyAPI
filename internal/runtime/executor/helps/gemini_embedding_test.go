package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIEmbeddingToGeminiMapsDimensions(t *testing.T) {
	_, single, _, errSingle := ConvertOpenAIEmbeddingToGemini("gemini-embedding-001", []byte(`{"input":"hi","dimensions":256}`))
	if errSingle != nil {
		t.Fatal(errSingle)
	}
	if got := gjson.GetBytes(single, "outputDimensionality").Int(); got != 256 {
		t.Fatalf("single outputDimensionality = %d, want 256; body=%s", got, single)
	}

	_, batch, _, errBatch := ConvertOpenAIEmbeddingToGemini("gemini-embedding-001", []byte(`{"input":["a","b"],"dimensions":128}`))
	if errBatch != nil {
		t.Fatal(errBatch)
	}
	for i, request := range gjson.GetBytes(batch, "requests").Array() {
		if got := request.Get("outputDimensionality").Int(); got != 128 {
			t.Fatalf("batch request %d outputDimensionality = %d, want 128; body=%s", i, got, batch)
		}
	}
}

func TestConvertOpenAIEmbeddingToGeminiRejectsTokenArrays(t *testing.T) {
	for _, payload := range []string{`{"input":[123,456]}`, `{"input":[[1,2],[3]]}`, `{"input":42}`} {
		if _, _, _, errConvert := ConvertOpenAIEmbeddingToGemini("m", []byte(payload)); errConvert == nil {
			t.Fatalf("payload %s: expected token input to be rejected", payload)
		}
	}
}

func TestConvertGeminiEmbeddingToOpenAIUsageIsNotVectorLength(t *testing.T) {
	out, errConvert := ConvertGeminiEmbeddingToOpenAI("m", []byte(`{"embedding":{"values":[0.1,0.2,0.3,0.4]}}`), false)
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	if got := gjson.GetBytes(out, "usage.prompt_tokens").Int(); got != 0 {
		t.Fatalf("prompt_tokens = %d, want 0 when Gemini reports no usage", got)
	}

	out, errConvert = ConvertGeminiEmbeddingToOpenAI("m", []byte(`{"embeddings":[{"values":[0.1,0.2]}],"usageMetadata":{"promptTokenCount":7}}`), true)
	if errConvert != nil {
		t.Fatal(errConvert)
	}
	if got := gjson.GetBytes(out, "usage.total_tokens").Int(); got != 7 {
		t.Fatalf("total_tokens = %d, want 7 from usageMetadata", got)
	}
}
