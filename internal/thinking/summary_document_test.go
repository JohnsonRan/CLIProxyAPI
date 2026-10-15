package thinking

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestSummaryDocumentMatchesGJSON pins the index to gjson.GetBytes semantics
// for every path the summary readers use, including duplicate members, nested
// misses, scalar roots and non-object bodies.
func TestSummaryDocumentMatchesGJSON(t *testing.T) {
	bodies := []string{
		`{}`,
		`[]`,
		`"text"`,
		`{"reasoning":{"summary":"auto","exclude":false},"include_reasoning":true}`,
		`{"reasoning":"flat","thinking":{"type":"enabled","budget_tokens":-1,"display":"omitted"}}`,
		`{"reasoning":{"summary":"concise"},"reasoning":{"summary":"detailed"}}`,
		`{"extra_body":{"google":{"thinking_config":{"include_thoughts":true}}},"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`,
		`{"generation_config":{"thinking_summaries":"none","thinking_config":{"includeThoughts":true}},"request":{"generationConfig":{"thinkingConfig":{"include_thoughts":true}}}}`,
		`{"messages":[{"role":"user","content":"a.b"}],"reasoning_effort":"low","reasoning.summary":"trap"}`,
	}
	paths := []string{
		"reasoning_effort",
		"reasoning",
		"reasoning.summary",
		"reasoning.generate_summary",
		"reasoning.exclude",
		"reasoning.enabled",
		"reasoning.includeThoughts",
		"include_reasoning",
		"thinking.type",
		"thinking.budget_tokens",
		"thinking.display",
		"thinking.include_thoughts",
		"extra_body.google.thinking_config.include_thoughts",
		"extra_body.extra_body.google.thinking_config.includeThoughts",
		"google.thinking_config.include_thoughts",
		"generationConfig.thinkingConfig.includeThoughts",
		"generation_config.thinking_config.includeThoughts",
		"generation_config.thinking_summaries",
		"request.generationConfig.thinkingConfig.include_thoughts",
		"missing.path",
	}
	for _, body := range bodies {
		doc := indexSummaryDocument([]byte(body))
		for _, path := range paths {
			want := gjson.GetBytes([]byte(body), path)
			got := doc.get(path)
			if got.Type != want.Type || got.Raw != want.Raw {
				t.Fatalf("body %s path %q: got %s (%v), want %s (%v)", body, path, got.Raw, got.Type, want.Raw, want.Type)
			}
		}
	}
}

func BenchmarkExtractSummaryConfigLargeChatBody(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-6","stream":true,"messages":[`)
	for i := 0; i < 48; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"role":"user","content":"` + strings.Repeat("some text here ", 70) + fmt.Sprint(i) + `"}`)
	}
	sb.WriteString(`],"reasoning_effort":"high"}`)
	body := []byte(sb.String())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if config := ExtractSummaryConfig(body, "openai"); config.Mode != SummaryEnabled {
			b.Fatalf("unexpected config %+v", config)
		}
	}
}
