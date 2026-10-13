package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type rerankUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *rerankUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type rerankUsageNoop struct{}

func (rerankUsageNoop) HandleUsage(context.Context, usage.Record) {}

func TestOpenAICompatExecutor_Embeddings(t *testing.T) {
	var requestedPath string
	var requestedAuth string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		requestedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{
					"object": "embedding",
					"embedding": [0.1, 0.2, 0.3],
					"index": 0
				}
			],
			"model": "text-embedding-3-small",
			"usage": {
				"prompt_tokens": 5,
				"total_tokens": 5
			}
		}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewOpenAICompatExecutor("openai-compat", cfg)

	auth := &cliproxyauth.Auth{
		ID:       "auth-test",
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-secret-key",
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "text-embedding-3-small",
		Payload: []byte(`{"model": "text-embedding-3-small", "input": "hello world"}`),
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

	if requestedPath != "/embeddings" {
		t.Errorf("requestedPath = %q, want /embeddings", requestedPath)
	}
	if requestedAuth != "Bearer test-secret-key" {
		t.Errorf("requestedAuth = %q, want 'Bearer test-secret-key'", requestedAuth)
	}
	if gjson.Get(receivedBody, "input").String() != "hello world" {
		t.Errorf("received input = %q, want 'hello world'", gjson.Get(receivedBody, "input").String())
	}
	if gjson.GetBytes(resp.Payload, "data.0.embedding.0").Float() != 0.1 {
		t.Errorf("expected embedding result in response payload, got %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutor_Rerank(t *testing.T) {
	var requestedPath string
	var requestedAuth string
	var receivedBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		requestedAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "rerank-123",
			"results": [
				{
					"index": 0,
					"relevance_score": 0.98,
					"document": {"text": "Paris is the capital of France"}
				}
			]
		}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	exec := NewOpenAICompatExecutor("openai-compat", cfg)

	auth := &cliproxyauth.Auth{
		ID:       "auth-test",
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "test-secret-key",
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "bge-reranker-v2-m3",
		Payload: []byte(`{"model": "bge-reranker-v2-m3", "query": "capital of France", "documents": ["Paris is the capital of France"]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("rerank"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey: "/v1/rerank",
		},
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if requestedPath != "/rerank" {
		t.Errorf("requestedPath = %q, want /rerank", requestedPath)
	}
	if requestedAuth != "Bearer test-secret-key" {
		t.Errorf("requestedAuth = %q, want 'Bearer test-secret-key'", requestedAuth)
	}
	if gjson.Get(receivedBody, "query").String() != "capital of France" {
		t.Errorf("received query = %q, want 'capital of France'", gjson.Get(receivedBody, "query").String())
	}
	if gjson.GetBytes(resp.Payload, "results.0.relevance_score").Float() != 0.98 {
		t.Errorf("expected relevance score 0.98, got %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutor_RerankPublishesUsage(t *testing.T) {
	const authID = "auth-rerank-usage"
	capture := &rerankUsageCapture{authID: authID, records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), rerankUsageNoop{})
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.5}],"meta":{"tokens":{"input_tokens":12,"output_tokens":0}}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compat", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         authID,
		Provider:   "openai-compatibility",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test-secret-key"},
	}
	req := cliproxyexecutor.Request{
		Model:   "BAAI/bge-reranker-v2-m3",
		Payload: []byte(`{"model":"BAAI/bge-reranker-v2-m3","query":"q","documents":["d"]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("rerank"),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/rerank"},
	}
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case record := <-capture.records:
		if record.Failed {
			t.Fatal("usage record marked failed")
		}
		if record.Model != "BAAI/bge-reranker-v2-m3" {
			t.Fatalf("model = %q", record.Model)
		}
		if record.Detail.InputTokens != 12 || record.Detail.TotalTokens != 12 {
			t.Fatalf("usage = %+v", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rerank usage record")
	}
}
