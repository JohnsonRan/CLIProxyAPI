package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestServer_EmbeddingsAndRerankRoutesMounted(t *testing.T) {
	cfg := &proxyconfig.Config{}
	server := NewServer(cfg, nil, nil, "")

	routes := []struct {
		method string
		path   string
	}{
		{"POST", "/v1/embeddings"},
		{"POST", "/v1/embedding"},
		{"POST", "/v1/rerank"},
		{"POST", "/rerank"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, err := http.NewRequest(rt.method, rt.path, bytes.NewBufferString("{}"))
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			server.engine.ServeHTTP(w, req)

			// Should reach the handler and return 400 (missing model parameter), NOT 404
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s %s returned status %d, want 400 (handler reached); body = %s", rt.method, rt.path, w.Code, w.Body.String())
			}

			errCode := gjson.GetBytes(w.Body.Bytes(), "error.code").String()
			if errCode != "missing_required_parameter" {
				t.Errorf("%s %s returned error code %q, want 'missing_required_parameter'", rt.method, rt.path, errCode)
			}
		})
	}
}
