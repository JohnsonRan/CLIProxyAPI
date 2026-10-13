package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiExecutor_EmbeddingsSingle(t *testing.T) {
	var requestedPath string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"embedding": {
				"values": [0.123, 0.456, -0.789]
			}
		}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewGeminiExecutor(cfg)

	auth := &cliproxyauth.Auth{
		ID:       "auth-gemini",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "test-gemini-key",
			"base_url": server.URL,
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "text-embedding-004",
		Payload: []byte(`{"model": "text-embedding-004", "input": "test prompt"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-embedding"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/embeddings",
		},
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.HasSuffix(requestedPath, "/models/text-embedding-004:embedContent") {
		t.Errorf("requestedPath = %q, want suffix /models/text-embedding-004:embedContent", requestedPath)
	}
	if gjson.Get(receivedBody, "content.parts.0.text").String() != "test prompt" {
		t.Errorf("unexpected translated Gemini body: %s", receivedBody)
	}
	if gjson.GetBytes(resp.Payload, "data.0.embedding.0").Float() != 0.123 {
		t.Errorf("unexpected translated OpenAI response: %s", string(resp.Payload))
	}
}

func TestGeminiExecutor_EmbeddingsBatch(t *testing.T) {
	var requestedPath string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"embeddings": [
				{ "values": [0.1, 0.2] },
				{ "values": [0.3, 0.4] }
			]
		}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewGeminiExecutor(cfg)

	auth := &cliproxyauth.Auth{
		ID:       "auth-gemini",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "test-gemini-key",
			"base_url": server.URL,
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "text-embedding-004",
		Payload: []byte(`{"model": "text-embedding-004", "input": ["first", "second"]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-embedding"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/embeddings",
		},
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.HasSuffix(requestedPath, "/models/text-embedding-004:batchEmbedContents") {
		t.Errorf("requestedPath = %q, want suffix /models/text-embedding-004:batchEmbedContents", requestedPath)
	}
	if gjson.Get(receivedBody, "requests.1.content.parts.0.text").String() != "second" {
		t.Errorf("unexpected batch translated Gemini body: %s", receivedBody)
	}
	if gjson.GetBytes(resp.Payload, "data.1.embedding.0").Float() != 0.3 {
		t.Errorf("unexpected translated OpenAI response: %s", string(resp.Payload))
	}
}
