package util

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

var responsesToolIdentityFixtures = []struct{ name, raw string }{
	{"plain functions", `{"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`},
	{"apply_patch winner", `{"tools":[{"type":"namespace","name":"a.b","tools":[{"type":"custom","name":"apply_patch"}]},{"type":"function","name":"a_b__apply_patch","parameters":{"type":"object","properties":{"n":{"type":"number"}}}},{"type":"function","name":"apply_patch"}],"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch"}]}]}`},
	{"custom apply_patch", `{"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}},{"type":"custom","name":"js_repl"}]}`},
	{"client tool_search", `{"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}},{"type":"tool_search","execution":"client","description":"Search deferred tools.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},{"type":"function","name":"tool.search","parameters":{"type":"object"}},{"type":"function","name":"deferred_fn","defer_loading":true,"parameters":{"type":"object"}}]}`},
	{"tool_search history only", `{"input":[{"type":"tool_search_call","execution":"client","call_id":"c1","arguments":{"query":"x"}},{"type":"tool_search_output","execution":"client","call_id":"c1","tools":[]}]}`},
	{"request wrapper", `{"request":{"model":"gpt","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"mcp__server__tool","parameters":{"type":"object","properties":{"a":{"type":"string","format":"uri"}}}}]}}`},
	{"request key that is not a wrapper", `{"request":{"note":"x"},"tools":[{"type":"custom","name":"apply_patch"}]}`},
	{"no tools", `{"model":"gpt","input":"hi"}`},
}

func TestResponsesToolReverseIdentityMapMatchesDeclarations(t *testing.T) {
	for _, fixture := range responsesToolIdentityFixtures {
		name, raw := fixture.name, fixture.raw
		t.Run(name, func(t *testing.T) {
			root := gjson.Parse(raw)
			if req := root.Get("request"); req.Exists() && (req.Get("model").Exists() || req.Get("input").Exists() || req.Get("tools").Exists()) {
				root = req
			}
			_, wantForward, wantReverse := BuildGeminiFunctionDeclarations(root)
			if name != "no tools" && len(wantReverse) == 0 {
				t.Fatal("fixture resolves no tools")
			}
			gotDecls, gotForward, gotReverse := buildGeminiFunctionDeclarations(root, false)
			if gotDecls != nil {
				t.Fatalf("identity-only build rendered %d declarations", len(gotDecls))
			}
			if !reflect.DeepEqual(gotForward, wantForward) {
				t.Fatalf("forward map differs:\n got %v\nwant %v", gotForward, wantForward)
			}
			if !reflect.DeepEqual(gotReverse, wantReverse) {
				t.Fatalf("reverse map differs:\n got %v\nwant %v", gotReverse, wantReverse)
			}
			if got := ResponsesToolReverseIdentityMap([]byte(raw)); !reflect.DeepEqual(got, wantReverse) {
				t.Fatalf("ResponsesToolReverseIdentityMap differs:\n got %v\nwant %v", got, wantReverse)
			}
		})
	}
}

// benchmarkResponsesToolsBody mimics a Codex request: dozens of function tools
// with nested JSON schemas plus the custom apply_patch tool.
func benchmarkResponsesToolsBody() []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt","input":[{"type":"message","role":"user","content":"hi"}],"tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}}`)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, `,{"type":"function","name":"mcp__server__tool_%d","description":"Tool %d does things.","parameters":{"type":"object","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false,"properties":{"path":{"type":"string","format":"uri","description":"Path"},"mode":{"type":"string","enum":["a","b","c"]},"options":{"type":"object","properties":{"depth":{"type":"integer","minimum":0,"maximum":10},"items":{"type":"array","items":{"anyOf":[{"type":"string"},{"type":"number"}]}}},"required":["depth"]}},"required":["path"]}}`, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkResponsesToolReverseIdentityMap(b *testing.B) {
	body := benchmarkResponsesToolsBody()
	b.ReportAllocs()
	for b.Loop() {
		if ResponsesToolReverseIdentityMap(body) == nil {
			b.Fatal("empty identity map")
		}
	}
}
