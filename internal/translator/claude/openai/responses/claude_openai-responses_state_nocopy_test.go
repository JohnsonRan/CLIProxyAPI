package responses

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// stateNoCopyRequest sets every echoed field, namespace, custom and history
// tool identities, so each value the stream state keeps comes from the request.
func stateNoCopyRequest(history string) []byte {
	return []byte(`{"model":"gpt-5","instructions":"be brief","max_output_tokens":99,"max_tool_calls":3,` +
		`"parallel_tool_calls":false,"previous_response_id":"resp_prev","prompt_cache_key":"cache-key",` +
		`"reasoning":{"effort":"high","summary":"auto"},"safety_identifier":"safe-id","service_tier":"priority",` +
		`"store":true,"temperature":1,"text":{"format":{"type":"text"}},"tool_choice":{"type":"function","name":"plain"},` +
		`"tools":[{"type":"namespace","name":"editor","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},` +
		`{"type":"function","name":"plain","parameters":{"type":"object","properties":{"q":{"type":"string"}}}},` +
		`{"type":"custom","name":"freeform","description":"raw text"},{"type":"function","name":"read.file","parameters":{"type":"object"}}],` +
		`"top_logprobs":2,"top_p":0.5,"truncation":"auto","user":"user-1","metadata":{"b":"2","a":"1"},` +
		`"input":[{"role":"user","content":"` + history + `"},{"type":"function_call","call_id":"c0","name":"read","namespace":"editor","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c0","output":"ok"},{"type":"function_call","call_id":"c1","name":"retired_tool","arguments":"{}"}]}`)
}

// The state is built from the caller's buffer without copying it, so it must
// not keep anything that aliases the buffer: overwriting the request after the
// first chunk must not change the stream.
func TestClaudeResponsesStateDoesNotAliasTheRequest(t *testing.T) {
	pristine := stateNoCopyRequest(strings.Repeat("context ", 64))
	chunks := stateNoCopyChunks()
	want := runClaudeChunks(pristine, chunks)

	buffer := bytes.Clone(pristine)
	var param any
	var got []byte
	for i, chunk := range chunks {
		request := pristine
		if i == 0 {
			request = buffer
		}
		got = append(got, bytes.Join(ConvertClaudeResponseToOpenAIResponses(context.Background(), "m", request, request, chunk, &param), nil)...)
		if i == 0 {
			for j := range buffer {
				buffer[j] = 'x'
			}
		}
	}
	if !bytes.Equal(normalizeEvents(t, got), normalizeEvents(t, want)) {
		t.Fatalf("stream changed after the request buffer was overwritten:\ngot  %s\nwant %s", got, want)
	}
}

func TestNewClaudeToResponsesStateDoesNotCopyTheRequest(t *testing.T) {
	request := stateNoCopyRequest(strings.Repeat("context ", 128*1024))
	allocs := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			newClaudeToResponsesState(request, false)
		}
	}).AllocedBytesPerOp()
	if allocs > int64(len(request))/4 {
		t.Fatalf("newClaudeToResponsesState allocates %d bytes per call on a %d byte request", allocs, len(request))
	}
}

func BenchmarkClaudeResponsesStreamLongHistory(b *testing.B) {
	request := stateNoCopyRequest(strings.Repeat("context ", 81920))
	chunks := claudeSSEFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var param any
		for _, chunk := range chunks {
			ConvertClaudeResponseToOpenAIResponses(context.Background(), "m", request, request, chunk, &param)
		}
	}
}

// stateNoCopyChunks adds a call to read.file, whose Claude name read_file only
// maps back through the state's tool names.
func stateNoCopyChunks() [][]byte {
	chunks := claudeSSEFixture()
	last := len(chunks) - 2
	extra := [][]byte{
		[]byte(`data: {"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"toolu_4","name":"read_file","input":{}}}`),
		[]byte(`data: {"type":"content_block_stop","index":5}`),
	}
	return append(append(append([][]byte{}, chunks[:last]...), extra...), chunks[last:]...)
}

func runClaudeChunks(request []byte, chunks [][]byte) []byte {
	var param any
	var out []byte
	for _, chunk := range chunks {
		out = append(out, bytes.Join(ConvertClaudeResponseToOpenAIResponses(context.Background(), "m", request, request, chunk, &param), nil)...)
	}
	return out
}
