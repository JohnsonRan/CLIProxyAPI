package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func runUpdateManagementPanel(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/server/update-panel", nil)
	h.UpdateManagementPanel(c)
	return w
}

func TestUpdateManagementPanel_StaticDirMissing(t *testing.T) {
	w := runUpdateManagementPanel(t, &Handler{})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error").String(); got != "static_dir_missing" {
		t.Fatalf("error = %q, want static_dir_missing; body %s", got, w.Body.String())
	}
}

func TestUpdateManagementPanel_RefusesWhenControlPanelDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.DisableControlPanel = true
	w := runUpdateManagementPanel(t, &Handler{cfg: cfg, configFilePath: t.TempDir() + "/config.yaml"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error").String(); got != "control_panel_disabled" {
		t.Fatalf("error = %q, want control_panel_disabled", got)
	}
}
