package responses

import (
	"os"
	"testing"
)

// convertGoldenRequest touches every top-level field the converter writes:
// instructions (system), tools, tool_choice, reasoning (thinking and the
// summary display), metadata.user_id, max_tokens, stream and messages with a
// tool round trip.
const convertGoldenRequest = `{"model":"claude-opus-5-5","stream":true,"max_output_tokens":4096,` +
	`"instructions":"Be brief.","prompt_cache_key":"pck-golden",` +
	`"reasoning":{"effort":"high","summary":"auto"},` +
	`"tools":[{"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],` +
	`"tool_choice":"auto",` +
	`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},` +
	`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},` +
	`{"type":"function_call_output","call_id":"call_1","output":"a.txt"},` +
	`{"type":"message","role":"user","content":"thanks"}]}`

// Pins the converted request byte for byte, so a change that reorders or
// reformats the top-level fields (for example where messages is spliced in)
// fails here even when every semantic test still passes. Regenerate with
// CPA_PRINT_GOLDEN=1 only for an intended output change.
func TestConvertOpenAIResponsesRequestToClaudeGoldenBytes(t *testing.T) {
	got, err := ConvertOpenAIResponsesRequestToClaude("claude-opus-5-5", []byte(convertGoldenRequest), true)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CPA_PRINT_GOLDEN") != "" {
		t.Logf("%s", got)
	}
	if string(got) != convertGoldenWant {
		t.Fatalf("converted request changed:\n got  %s\n want %s", got, convertGoldenWant)
	}
}

const convertGoldenWant = `{"model":"claude-opus-5-5","max_tokens":4096,"messages":[{"role":"user","content":"list files"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"shell","input":{"cmd":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a.txt"},{"type":"text","text":"thanks"}]}],"metadata":{"user_id":"b087183d9b10d3a6ed9f0527bad835ba62777cadee84922a1df780c3724402b3"},"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"},"stream":true,"system":[{"type":"text","text":"Be brief."}],"tools":[{"name":"shell","description":"Run a command","input_schema":{"properties":{"cmd":{"type":"string"}},"required":["cmd"],"type":"object"}}],"tool_choice":{"type":"auto"}}`
