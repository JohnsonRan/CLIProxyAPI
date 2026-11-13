package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestGetModelCatalogIncludesRegisteredModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := registry.GetGlobalRegistry()
	clientID := "catalog-" + strings.ReplaceAll(t.Name(), "/", "-")
	modelID := "catalog-model-" + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})
	reg.RegisterClient(clientID, "codex", []*registry.ModelInfo{
		{ID: modelID, DisplayName: "Catalog Model", Type: "openai", OwnedBy: "test"},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/models", nil)
	h.GetModelCatalog(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
			OwnedBy     string `json:"owned_by"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, model := range resp.Models {
		if model.ID != modelID {
			continue
		}
		if model.DisplayName != "Catalog Model" || model.Type != "openai" || model.OwnedBy != "test" {
			t.Fatalf("model metadata = %+v", model)
		}
		return
	}
	t.Fatalf("catalog missing %s: %s", modelID, rec.Body.String())
}
