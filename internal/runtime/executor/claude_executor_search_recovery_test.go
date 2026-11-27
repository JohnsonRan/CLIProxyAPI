package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeExecutor_SearchRecoveryRetryOnEncryptedRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var bodies [][]byte
			var mu sync.Mutex

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					http.Error(w, "read error", http.StatusBadRequest)
					return
				}
				mu.Lock()
				bodies = append(bodies, body)
				reqIndex := len(bodies)
				mu.Unlock()

				// First request: simulate Anthropic rejecting encrypted search content with HTTP 400
				if reqIndex == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.1.content.0: Invalid ` + "`encrypted_content`" + ` in ` + "`search_result`" + ` block"}}`))
					return
				}

				// Retry request: expect repaired plain text, return HTTP 200
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"recovered response"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered stream\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			}))
			defer server.Close()

			cfg := &config.Config{}
			auth := &cliproxyauth.Auth{
				ID:       "auth-1",
				Provider: "claude",
				Attributes: map[string]string{
					"base_url": server.URL,
					"api_key":  "test-key",
				},
			}
			exec := NewClaudeExecutor(cfg)

			streamJSON := "false"
			if stream {
				streamJSON = "true"
			}
			requestPayload := []byte(`{
				"model":"claude-opus-5-5",
				"stream":` + streamJSON + `,
				"messages":[
					{"role":"user","content":[{"type":"text","text":"earlier search"}]},
					{"role":"assistant","content":[
						{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"weather"}},
						{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"W","url":"https://example.com/weather","encrypted_content":"enc_123"}]},
						{"type":"text","text":"It is sunny.","citations":[{"type":"web_search_result_location","title":"W","url":"https://example.com/weather","cited_text":"sunny","encrypted_index":"idx_1"}]}
					]},
					{"role":"user","content":[{"type":"text","text":"what about tomorrow?"}]}
				]
			}`)

			req := cliproxyexecutor.Request{
				Model:   "claude-opus-5-5",
				Payload: requestPayload,
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatClaude,
				ResponseFormat: sdktranslator.FormatClaude,
				Stream:         stream,
			}

			if !stream {
				resp, err := exec.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("Execute() unexpected error: %v", err)
				}
				if !strings.Contains(string(resp.Payload), "recovered response") {
					t.Fatalf("Execute() unexpected payload: %s", resp.Payload)
				}
			} else {
				result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream() unexpected error: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("chunk error: %v", chunk.Err)
					}
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("expected 2 requests (1 initial + 1 recovery retry), got %d", len(bodies))
			}
			// Initial request had encrypted search result
			if !strings.Contains(string(bodies[0]), "web_search_tool_result") {
				t.Fatalf("initial request should contain web_search_tool_result: %s", bodies[0])
			}
			// Retried request had web search replaced by plain text
			if strings.Contains(string(bodies[1]), "web_search_tool_result") {
				t.Fatalf("retried request should NOT contain web_search_tool_result: %s", bodies[1])
			}
			if !strings.Contains(ojson.GetBytes(bodies[1], "messages.1.content.1.text").String(), "https://example.com/weather") {
				t.Fatalf("retried request should contain converted text with source URL: %s", bodies[1])
			}
		})
	}
}
