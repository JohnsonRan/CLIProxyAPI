package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	codexTrustedAccessMetadataEvent = `{"type":"response.metadata","metadata":{"openai_verification_recommendation":["trusted_access_for_cyber"],"openai_chatgpt_moderation_metadata":{"prompt":{}}}}`
	claudeRefusalMessageDelta       = `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"blocked"}},"usage":{"output_tokens":1}}`
)

// riskUsageCapture keeps the first record of a provider that reports an upstream risk.
type riskUsageCapture struct {
	provider string
	records  chan usage.Record
}

func (c *riskUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider != c.provider || record.UpstreamRisk == "" {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

func captureRiskRecords(t *testing.T, provider string) *riskUsageCapture {
	t.Helper()
	c := &riskUsageCapture{provider: provider, records: make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin(t.Name(), c)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), noopClaudeUsagePlugin{}) })
	return c
}

func (c *riskUsageCapture) await(t *testing.T, want, wantReason string) {
	t.Helper()
	select {
	case record := <-c.records:
		if record.UpstreamRisk != want || record.UpstreamRiskReason != wantReason {
			t.Fatalf("UpstreamRisk = %q (%q), want %q (%q)", record.UpstreamRisk, record.UpstreamRiskReason, want, wantReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no usage record with UpstreamRisk %q", want)
	}
}

func TestCodexStreamRecordsTrustedAccessForCyber(t *testing.T) {
	for _, buffering := range []bool{false, true} {
		capture := captureRiskRecords(t, "codex")
		// Without buffering the frame arrives late, as in ordinary streams; with buffering it is
		// held during bootstrap, where upstream also sends it before rejections.
		events := []string{codexCreatedEvent, codexInProgressEvent, codexOutputDeltaEvent, codexTrustedAccessMetadataEvent, codexCompletedEventBody}
		if buffering {
			events = []string{codexCreatedEvent, codexInProgressEvent, codexTrustedAccessMetadataEvent, codexOutputDeltaEvent, codexCompletedEventBody}
		}
		server := codexSSEServer(events...)
		req, opts := codexTestRequest()
		result, err := NewCodexExecutor(codexBufferingConfig(buffering)).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
		if err != nil {
			server.Close()
			t.Fatalf("buffering=%v: ExecuteStream: %v", buffering, err)
		}
		if _, errDrain := drainChunks(result); errDrain != nil {
			server.Close()
			t.Fatalf("buffering=%v: stream: %v", buffering, errDrain)
		}
		server.Close()
		capture.await(t, "trusted_access_for_cyber", "")
	}
}

func TestCodexWebsocketRecordsTrustedAccessForCyber(t *testing.T) {
	capture := captureRiskRecords(t, "codex")
	server := codexWebsocketServer(t, codexCreatedEvent, codexInProgressEvent, codexOutputDeltaEvent, codexTrustedAccessMetadataEvent, codexCompletedEventBody)
	defer server.Close()
	req, opts := codexWebsocketRequest()
	result, err := NewCodexWebsocketsExecutor(&config.Config{}).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if _, errDrain := drainChunks(result); errDrain != nil {
		t.Fatalf("stream: %v", errDrain)
	}
	capture.await(t, "trusted_access_for_cyber", "")
}

func claudeRiskServer(contentType, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
}

func claudeRiskAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-123", "base_url": baseURL}}
}

func TestClaudeStreamRecordsRefusalCategory(t *testing.T) {
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		"data: " + claudeRefusalMessageDelta + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		capture := captureRiskRecords(t, "claude")
		server := claudeRiskServer("text/event-stream", stream)
		result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(context.Background(), claudeRiskAuth(server.URL), cliproxyexecutor.Request{
			Model:   "claude-opus-5",
			Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		}, cliproxyexecutor.Options{SourceFormat: format, ResponseFormat: format, Stream: true})
		if err != nil {
			server.Close()
			t.Fatalf("%s: ExecuteStream: %v", format, err)
		}
		for chunk := range result.Chunks {
			_ = chunk
		}
		server.Close()
		capture.await(t, "refusal_cyber", "blocked")
	}
}

func TestClaudeExecuteRecordsRefusalCategory(t *testing.T) {
	capture := captureRiskRecords(t, "claude")
	server := claudeRiskServer("application/json", `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"bio","explanation":"blocked"},"usage":{"input_tokens":10,"output_tokens":1}}`)
	defer server.Close()
	_, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), claudeRiskAuth(server.URL), cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	capture.await(t, "refusal_bio", "blocked")
}

func geminiRiskAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": baseURL}}
}

func TestGeminiExecuteRecordsBlockedPrompt(t *testing.T) {
	capture := captureRiskRecords(t, "gemini")
	server := claudeRiskServer("application/json", `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT","blockReasonMessage":"The prompt was blocked."},"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}}`)
	defer server.Close()
	_, _ = NewGeminiExecutor(&config.Config{}).Execute(context.Background(), geminiRiskAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	capture.await(t, "prompt_blocked_prohibited_content", "The prompt was blocked.")
}

func TestGeminiStreamRecordsSafetyStop(t *testing.T) {
	capture := captureRiskRecords(t, "gemini")
	server := claudeRiskServer("text/event-stream",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}`+"\n\n"+
			`data: {"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH","blocked":true},{"category":"HARM_CATEGORY_HARASSMENT","probability":"NEGLIGIBLE"}]}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`+"\n\n")
	defer server.Close()
	result, err := NewGeminiExecutor(&config.Config{}).ExecuteStream(context.Background(), geminiRiskAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.1-pro-preview",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for range result.Chunks {
	}
	capture.await(t, "output_blocked_safety", "HARM_CATEGORY_DANGEROUS_CONTENT (HIGH)")
}
