package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// GetModelCatalog returns every model the proxy can currently serve.
// Unlike /v1/models, this list is not filtered by client API key rules, so the
// management panel can offer those rules a real choice.
func (h *Handler) GetModelCatalog(c *gin.Context) {
	models := registry.GetGlobalRegistry().GetAvailableModelInfos()
	result := make([]gin.H, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		entry := gin.H{"id": id}
		if name := strings.TrimSpace(model.DisplayName); name != "" {
			entry["display_name"] = name
		}
		if modelType := strings.TrimSpace(model.Type); modelType != "" {
			entry["type"] = modelType
		}
		if ownedBy := strings.TrimSpace(model.OwnedBy); ownedBy != "" {
			entry["owned_by"] = ownedBy
		}
		result = append(result, entry)
	}
	c.JSON(http.StatusOK, gin.H{"models": result})
}
