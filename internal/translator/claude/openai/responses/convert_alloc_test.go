package responses

import (
	"strings"
	"testing"
)

func largeResponsesRequest(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5-5","stream":true,"instructions":"be brief","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"tool_choice":"auto","reasoning":{"effort":"high","summary":"auto"},"input":[`)
	text := strings.Repeat("lorem ipsum ", 400)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]},`)
		b.WriteString(`{"type":"function_call","call_id":"call_` + strings.Repeat("a", i%7+1) + `","name":"shell","arguments":"{}"},`)
		b.WriteString(`{"type":"function_call_output","call_id":"call_` + strings.Repeat("a", i%7+1) + `","output":"` + text + `"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkConvertLargeResponsesRequestToClaude(b *testing.B) {
	request := largeResponsesRequest(100)
	b.SetBytes(int64(len(request)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", request, true); err != nil {
			b.Fatal(err)
		}
	}
}
