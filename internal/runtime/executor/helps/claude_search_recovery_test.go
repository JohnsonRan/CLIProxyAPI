package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRecoverClaudeSearchHistoryReplacesOnlyRejectedTurn(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5-5",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"search go 1.26"}]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go 1.26"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Go 1.26 Release Notes","url":"https://go.dev/doc/go1.26","encrypted_content":"enc_abc"}]},
				{"type":"text","text":"Go 1.26 is out.","citations":[{"type":"web_search_result_location","title":"Go 1.26","url":"https://go.dev/doc/go1.26","cited_text":"Go 1.26","encrypted_index":"idx_1"}]}
			]},
			{"role":"user","content":[{"type":"text","text":"thanks"}]}
		]
	}`)
	failure := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.1.content.0: Invalid ` + "`encrypted_content`" + ` in ` + "`search_result`" + ` block"}}`)

	repaired, ok := RecoverClaudeSearchHistory(body, failure)
	if !ok {
		t.Fatal("expected RecoverClaudeSearchHistory to succeed")
	}
	if strings.Contains(string(repaired), "web_search_tool_result") || strings.Contains(string(repaired), "server_tool_use") {
		t.Fatalf("expected native search blocks to be converted to text, got: %s", repaired)
	}
	if !strings.Contains(gjson.GetBytes(repaired, "messages.1.content.1.text").String(), "https://go.dev/doc/go1.26") {
		t.Fatalf("expected preserved source URL in converted text block, got: %s", repaired)
	}
}
