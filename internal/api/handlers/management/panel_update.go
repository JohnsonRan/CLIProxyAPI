package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
)

// UpdateManagementPanel checks for and installs the latest management panel asset on demand.
func (h *Handler) UpdateManagementPanel(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler_unavailable"})
		return
	}

	h.mu.Lock()
	configFilePath := h.configFilePath
	panelDisabled, panelRepo, proxyURL := false, "", ""
	if h.cfg != nil {
		panelDisabled = h.cfg.RemoteManagement.DisableControlPanel
		panelRepo = strings.TrimSpace(h.cfg.RemoteManagement.PanelGitHubRepository)
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	h.mu.Unlock()

	if panelDisabled {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "control_panel_disabled",
			"message": "the management control panel is disabled by remote-management.disable-control-panel",
		})
		return
	}

	staticDir := managementasset.StaticDir(configFilePath)
	if staticDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "static_dir_missing",
			"message": "management static directory is not configured or resolved",
		})
		return
	}

	updated, status, err := managementasset.ForceUpdateManagementHTML(c.Request.Context(), staticDir, proxyURL, panelRepo)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   status,
			"message": fmt.Sprintf("failed to update management panel: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"updated": updated,
		"message": status,
	})
}
