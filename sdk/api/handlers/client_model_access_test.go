package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// "auto" must not let an exclusion-only key reach an excluded model.
func TestValidateClientModelAccessChecksResolvedAutoModel(t *testing.T) {
	const clientID = "client-model-access-auto-test"
	// The newest model wins "auto" resolution, so make the excluded one newest.
	registry.GetGlobalRegistry().RegisterClient(clientID, "codex", []*registry.ModelInfo{{ID: "secret-model-auto-test", Created: 1 << 40}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })

	h := &BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{ClientExcludedModels: []string{"secret-*"}}}
	ctx := coreusage.WithAPIKey(context.Background(), "client-key")

	for _, model := range []string{"auto", "auto(high)", "secret-model-auto-test"} {
		if errMsg := h.validateClientModelAccess(ctx, model); errMsg == nil {
			t.Fatalf("validateClientModelAccess(%q) allowed an excluded model", model)
		}
	}
	if errMsg := h.validateClientModelAccess(ctx, "gpt-5"); errMsg != nil {
		t.Fatalf("validateClientModelAccess(gpt-5) = %v, want allowed", errMsg)
	}
}

// Client model rules apply to a catalog a response interceptor replaced, and
// the interceptor's Content-Length does not survive the shorter body.
func TestWriteModelListResponseFiltersInterceptedCatalogForClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{ClientExcludedModels: []string{"secret-*"}}}
	h.SetPluginHost(&handlerInterceptorTestHost{
		interceptResponse: func(_ context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
			headers := cloneHeader(req.ResponseHeaders)
			headers.Set("Content-Length", "1024")
			return pluginapi.ResponseInterceptResponse{
				Headers: headers,
				Body:    []byte(`{"object":"list","data":[{"id":"secret-model"},{"id":"public-model"}]}`),
			}
		},
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set("userApiKey", "client-key")
	h.WriteModelListResponse(c, "openai", gin.H{"object": "list", "data": []gin.H{{"id": "public-model"}}})

	data := gjson.GetBytes(recorder.Body.Bytes(), "data").Array()
	if len(data) != 1 || data[0].Get("id").String() != "public-model" {
		t.Fatalf("intercepted list = %s, want only public-model", recorder.Body.String())
	}
	if length := recorder.Header().Get("Content-Length"); length != "" {
		t.Fatalf("Content-Length = %s survived a filtered body", length)
	}
}
