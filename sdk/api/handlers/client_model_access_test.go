package handlers

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
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
