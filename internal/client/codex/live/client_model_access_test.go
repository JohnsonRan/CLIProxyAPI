package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Per-key model rules apply to the model the client asked for, not to the
// upstream model it is rewritten to.
func TestHandlerChecksRequestedModelBeforeRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.ClientExcludedModels = []string{"gpt-realtime*"}
	handler := NewHandler(auth.NewManager(nil, nil, nil), cfg)
	router := gin.New()
	router.POST("/v1/live", func(c *gin.Context) { c.Set("userApiKey", "client-key") }, handler.Handle)

	const boundary = "client-model-boundary"
	body := multipartBody(boundary, "v=0\r\no=desktop-offer\r\n", `{"model":"gpt-realtime"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "gpt-realtime") {
		t.Fatalf("error should name the requested model: %s", recorder.Body.String())
	}
}
