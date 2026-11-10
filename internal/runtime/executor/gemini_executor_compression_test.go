package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/klauspost/compress/zstd"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestGeminiExecutorCompressedStreamPreservesReasoningReplay(t *testing.T) {
	internalcache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)

	const model = "gemini-3-flash"
	const signature = "EmcKZQFpFH0THU+YNRY5uVwt/0bo2hFo1RmXsMhlX9FCg2n6GssBe5sMV37KB6OrgeKKEP9fxN261QX"
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"Call get_weather for Beijing"}]}]}`)
	req := cliproxyexecutor.Request{Model: model, Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"Session-Id": {"compressed-gemini-replay"}},
	}
	scope, _, _, errPrepare := prepareGeminiTopLevelReasoningReplayPayloadDoc(context.Background(), model, req, opts, ojson.NewDoc(payload))
	if errPrepare != nil || !scope.valid() {
		t.Fatalf("prepare replay scope = %+v, error = %v", scope, errPrepare)
	}

	encoder, errEncoder := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if errEncoder != nil {
		t.Fatal(errEncoder)
	}
	defer func() {
		if errClose := encoder.Close(); errClose != nil {
			t.Errorf("close encoder: %v", errClose)
		}
	}()
	response := `data: {"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"` + signature + `","functionCall":{"name":"get_weather","args":{"city":"Beijing"},"id":"call_1"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}` + "\n\n"
	compressed := encoder.EncodeAll([]byte(response), nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "zstd")
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "test-key",
		"base_url": server.URL,
	}}
	stream, errStream := NewGeminiExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	var output strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), "response.completed") {
		t.Fatalf("compressed stream did not complete: %s", output.String())
	}
	items, ok := internalcache.GetAntigravityReasoningReplayItems(scope.modelName, scope.sessionKey)
	if !ok || len(items) != 1 {
		t.Fatalf("cached replay items = %d, found = %v, want 1 item", len(items), ok)
	}
	if got := ojson.GetBytes(items[0], "thoughtSignature").String(); got != signature {
		t.Fatalf("cached thoughtSignature = %q, want %q", got, signature)
	}
}
