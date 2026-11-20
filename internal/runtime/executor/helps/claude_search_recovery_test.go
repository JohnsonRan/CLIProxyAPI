package helps

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestRecoverClaudeSearchHistoryReplacesOnlyRejectedTurn(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-5-5",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"search go 1.26"}]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go 1.26"},"cache_control":{"type":"ephemeral"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Go 1.26 Release Notes","url":"https://go.dev/doc/go1.26","encrypted_content":"enc_abc"}]},
				{"type":"text","text":"Go 1.26 is out.","citations":[
					{"type":"web_search_result_location","title":"Go 1.26","url":"https://go.dev/doc/go1.26","cited_text":"Go 1.26","encrypted_index":"idx_1"},
					{"type":"char_location","cited_text":"Go 1.26","document_index":0,"start_char_index":0,"end_char_index":7}
				]}
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
	if !strings.Contains(ojson.GetBytes(repaired, "messages.1.content.1.text").String(), "https://go.dev/doc/go1.26") {
		t.Fatalf("expected preserved source URL in converted text block, got: %s", repaired)
	}
	if !strings.Contains(ojson.GetBytes(repaired, "messages.1.content.2.text").String(), "Go 1.26 is out.") {
		t.Fatalf("expected original response text preserved, got: %s", repaired)
	}
	// Check cache_control preserved
	if got := ojson.GetBytes(repaired, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("expected cache_control preserved on converted server_tool_use, got: %s", repaired)
	}
	// Check non-web citation kept
	citations := ojson.GetBytes(repaired, "messages.1.content.2.citations").Array()
	if len(citations) != 1 || citations[0].Get("type").String() != "char_location" {
		t.Fatalf("expected only non-web citation kept, got: %s", ojson.GetBytes(repaired, "messages.1.content.2.citations").Raw)
	}
}

func TestApplyClaudeSearchRecoveryDoc(t *testing.T) {
	rejectedBody := []byte(`{
		"model":"claude-opus-5-5",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"query"}]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_fail","name":"web_search","input":{"query":"q"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_fail","content":[{"type":"web_search_result","title":"A","url":"https://example.com","encrypted_content":"enc"}]}
			]}
		]
	}`)
	failure := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.1.content.0: Invalid ` + "`encrypted_content`" + ` in ` + "`search_result`" + ` block"}}`)

	ctx := WithClaudeSearchRecovery(context.Background(), failure, rejectedBody)
	if !CanRecoverClaudeSearch(ctx) {
		t.Fatal("expected CanRecoverClaudeSearch to be true")
	}

	nextBody := []byte(`{
		"model":"claude-opus-5-5",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"query"}]},
			{"role":"assistant","content":[
				{"type":"server_tool_use","id":"srvtoolu_fail","name":"web_search","input":{"query":"q"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_fail","content":[{"type":"web_search_result","title":"A","url":"https://example.com","encrypted_content":"enc"}]}
			]},
			{"role":"user","content":[{"type":"text","text":"follow up"}]}
		]
	}`)
	doc := ojson.NewDoc(nextBody)
	if !ApplyClaudeSearchRecoveryDoc(ctx, doc) {
		t.Fatal("expected ApplyClaudeSearchRecoveryDoc to modify doc")
	}
	out := doc.Bytes()
	if strings.Contains(string(out), "web_search_tool_result") || strings.Contains(string(out), "server_tool_use") {
		t.Fatalf("expected native search blocks to be converted to text, got: %s", out)
	}
	if !strings.Contains(ojson.GetBytes(out, "messages.1.content.1.text").String(), "https://example.com") {
		t.Fatalf("expected preserved source URL, got: %s", out)
	}
}

func TestClaudeSearchRecoveryBounding(t *testing.T) {
	ctx := context.Background()
	failure := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.1: Invalid ` + "`encrypted_content`" + ` in ` + "`search_result`" + ` block"}}`)
	body := []byte(`{"messages":[{"role":"user","content":[]},{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"tool_1"}]}]}`)

	for i := 0; i < 3; i++ {
		if !CanRecoverClaudeSearch(ctx) {
			t.Fatalf("expected retry permitted at iteration %d", i)
		}
		ctx = WithClaudeSearchRecovery(ctx, failure, body)
	}
	if CanRecoverClaudeSearch(ctx) {
		t.Fatal("expected retry bounded at 3 turns")
	}
}
