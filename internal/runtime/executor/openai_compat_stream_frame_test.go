package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func executeOpenAICompatStreamTest(t *testing.T, ssePayload string) ([]cliproxyexecutor.StreamChunk, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(ssePayload))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "compat",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url":     server.URL + "/v1",
			"api_key":      "test",
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}

	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6",
		Payload: []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"hello"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if errExecute != nil {
		return nil, errExecute
	}

	var chunks []cliproxyexecutor.StreamChunk
	for chunk := range result.Chunks {
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

func TestOpenAICompatStreamFrame_InvalidJSONEndsStream(t *testing.T) {
	chunks, err := executeOpenAICompatStreamTest(t, "data: {\"id\":\"chatcmpl_1\", incomplete json\n\n")
	if err != nil {
		t.Fatalf("unexpected ExecuteStream error: %v", err)
	}
	var foundErr error
	for _, chunk := range chunks {
		if chunk.Err != nil {
			foundErr = chunk.Err
			break
		}
	}
	if foundErr == nil {
		t.Fatal("expected stream error for invalid JSON frame, got nil")
	}
	const wantErr = "upstream stream ended with incomplete SSE data frame"
	if !strings.Contains(foundErr.Error(), wantErr) {
		t.Fatalf("stream error %q does not contain expected %q", foundErr.Error(), wantErr)
	}
}

func TestOpenAICompatStreamFrame_ErrorPayloadSurfacesAsError(t *testing.T) {
	payload := "data: {\"error\":{\"message\":\"rate limit reached\",\"type\":\"requests\",\"code\":\"rate_limit_exceeded\",\"status\":429}}\n\n"
	chunks, err := executeOpenAICompatStreamTest(t, payload)
	if err != nil {
		t.Fatalf("unexpected ExecuteStream error: %v", err)
	}
	var foundErr error
	for _, chunk := range chunks {
		if chunk.Err != nil {
			foundErr = chunk.Err
			break
		}
	}
	if foundErr == nil {
		t.Fatal("expected stream error for upstream error payload, got nil")
	}
	if !strings.Contains(foundErr.Error(), "rate limit reached") {
		t.Fatalf("stream error %q does not contain expected message", foundErr.Error())
	}
	if sErr, ok := foundErr.(statusErr); ok {
		if sErr.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected status code %d, got %d", http.StatusTooManyRequests, sErr.StatusCode())
		}
	}
}

func TestOpenAICompatStreamFrame_NormalFrameWithDonePassesThrough(t *testing.T) {
	payload := "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
	chunks, err := executeOpenAICompatStreamTest(t, payload)
	if err != nil {
		t.Fatalf("unexpected ExecuteStream error: %v", err)
	}
	var combined string
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		combined += string(chunk.Payload)
	}
	if !strings.Contains(combined, "hello") {
		t.Fatalf("expected streamed content to contain %q, got %q", "hello", combined)
	}
}
