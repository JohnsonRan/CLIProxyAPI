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

func TestSystemOneRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{name: "invalid json", body: `{`, wantMsg: "Invalid request: body must be a JSON object"},
		{name: "array body", body: `[]`, wantMsg: "Invalid request: body must be a JSON object"},
		{name: "missing model", body: `{"state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`, wantMsg: "Missing required parameter 'model'"},
		{name: "missing state", body: `{"model":"jev-latest","questions":{"q":{"type":"noul","instructions":"i"}}}`, wantMsg: "Missing required parameter 'state'"},
		{name: "null state", body: `{"model":"jev-latest","state":null,"questions":{"q":{"type":"noul","instructions":"i"}}}`, wantMsg: "Missing required parameter 'state'"},
		{name: "missing questions", body: `{"model":"jev-latest","state":"s"}`, wantMsg: "Missing or invalid required parameter 'questions'"},
		{name: "empty questions", body: `{"model":"jev-latest","state":"s","questions":{}}`, wantMsg: "Missing or invalid required parameter 'questions'"},
		{name: "questions array", body: `{"model":"jev-latest","state":"s","questions":[]}`, wantMsg: "Missing or invalid required parameter 'questions'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewBufferString(tt.body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.SystemOne(c)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if got := gjson.GetBytes(w.Body.Bytes(), "error.message").String(); got != tt.wantMsg {
				t.Fatalf("error.message = %q, want %q", got, tt.wantMsg)
			}
			if got := gjson.GetBytes(w.Body.Bytes(), "error.type").String(); got != "invalid_request_error" {
				t.Fatalf("error.type = %q", got)
			}
		})
	}
}
