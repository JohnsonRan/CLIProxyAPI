package openai

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func newTestOpenAIHandler() *OpenAIAPIHandler {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	return NewOpenAIAPIHandler(base)
}

func TestEmbeddingsValidation(t *testing.T) {
	h := newTestOpenAIHandler()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "Missing model",
			body:       `{"input": "hello"}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing required parameter 'model'",
		},
		{
			name:       "Missing input",
			body:       `{"model": "text-embedding-3-small"}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing required parameter 'input'",
		},
		{
			name:       "Invalid JSON",
			body:       `{invalid json`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Invalid request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewBufferString(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Embeddings(c)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tt.wantStatus, w.Body.String())
			}
			msg := gjson.GetBytes(w.Body.Bytes(), "error.message").String()
			if !gjson.GetBytes(w.Body.Bytes(), "error").Exists() {
				t.Fatalf("expected error object, got %s", w.Body.String())
			}
			if msg == "" {
				t.Fatalf("expected error message containing %q, got empty", tt.wantMsg)
			}
		})
	}
}

func TestRerankValidation(t *testing.T) {
	h := newTestOpenAIHandler()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{
			name:       "Missing model",
			body:       `{"query": "test", "documents": ["doc1"]}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing required parameter 'model'",
		},
		{
			name:       "Missing query",
			body:       `{"model": "bge-reranker-v2-m3", "documents": ["doc1"]}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing required parameter 'query'",
		},
		{
			name:       "Missing documents",
			body:       `{"model": "bge-reranker-v2-m3", "query": "test"}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing or invalid required parameter 'documents'",
		},
		{
			name:       "Empty documents array",
			body:       `{"model": "bge-reranker-v2-m3", "query": "test", "documents": []}`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Missing or invalid required parameter 'documents'",
		},
		{
			name:       "Invalid JSON",
			body:       `{not valid json`,
			wantStatus: http.StatusBadRequest,
			wantMsg:    "Invalid request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewBufferString(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Rerank(c)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if !gjson.GetBytes(w.Body.Bytes(), "error").Exists() {
				t.Fatalf("expected error object, got %s", w.Body.String())
			}
		})
	}
}
