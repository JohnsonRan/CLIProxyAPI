package responses

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The request body is validated and pinned when the per-stream state is
// created; later chunks must not re-validate (or re-pick) it.
func TestConvertClaudeResponseToOpenAIResponses_PinsRequestJSONPerStream(t *testing.T) {
	request := []byte(`{"model":"claude-test","instructions":"pinned-instructions"}`)
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1"}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for i, chunk := range chunks {
		// Only the first chunk carries the request; the registry passes the
		// same bytes on every chunk, but the state must not depend on it.
		var req []byte
		if i == 0 {
			req = request
		}
		outputs = append(outputs, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", req, nil, chunk, &param)...)
	}

	st, ok := param.(*claudeToResponsesState)
	if !ok {
		t.Fatalf("param = %T, want *claudeToResponsesState", param)
	}
	if !bytes.Equal(st.RequestJSON, request) {
		t.Fatalf("state.RequestJSON = %q, want %q", st.RequestJSON, request)
	}

	var completed string
	for _, output := range outputs {
		event, data := parseClaudeResponsesSSEEvent(t, output)
		if event == "response.completed" {
			completed = data.Get("response.instructions").String()
		}
	}
	if completed != "pinned-instructions" {
		t.Fatalf("response.completed instructions = %q, want pinned-instructions", completed)
	}
}

func BenchmarkConvertClaudeResponseToOpenAIResponses_TextDelta(b *testing.B) {
	// A 64 KiB request body, as a Codex session with a long history sends.
	request := []byte(`{"model":"claude-test","instructions":"` + strings.Repeat("x", 64*1024) + `","tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}]}`)
	start := []byte(`data: {"type":"message_start","message":{"id":"msg_1"}}`)
	blockStart := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	delta := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"token "}}`)

	var param any
	ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, nil, start, &param)
	ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, nil, blockStart, &param)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", request, nil, delta, &param)
	}
}
