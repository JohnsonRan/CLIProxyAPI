package cliproxy

import (
	"context"
	"slices"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestRegisterModelsForAuth_CredentialAllowedModels(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-gemini-allowed",
		Provider: "gemini",
		Prefix:   "team",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"allowed_models":  "gemini-2.5-*",
			"excluded_models": "gemini-2.5-flash",
		},
	}
	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)

	var ids []string
	for _, model := range registry.GetModelsForClient(auth.ID) {
		ids = append(ids, model.ID)
	}
	if len(ids) == 0 {
		t.Fatal("expected allowed models to be registered")
	}
	for _, id := range ids {
		base := strings.TrimPrefix(id, "team/")
		if !matchWildcard("gemini-2.5-*", base) {
			t.Errorf("model %q registered outside the allow-list", id)
		}
		if base == "gemini-2.5-flash" {
			t.Errorf("excluded model %q registered", id)
		}
	}
	// The allow-list matches upstream IDs, so the prefixed route stays available.
	if !slices.Contains(ids, "team/gemini-2.5-pro") {
		t.Errorf("expected prefixed allowed model, got %v", ids)
	}
}

func TestApplyAllowedModels(t *testing.T) {
	models := []*ModelInfo{{ID: "a-1"}, {ID: "A-2"}, {ID: "b-1"}, nil}
	ids := func(in []*ModelInfo) []string {
		var out []string
		for _, m := range in {
			out = append(out, m.ID)
		}
		return out
	}
	if got := ids(applyAllowedModels(models, []string{"a-*"})); !slices.Equal(got, []string{"a-1", "A-2"}) {
		t.Fatalf("wildcard allow-list = %v", got)
	}
	if got := applyAllowedModels(models, []string{" ", ""}); len(got) != len(models) {
		t.Fatalf("blank allow-list must not restrict, got %d models", len(got))
	}
	if got := applyAllowedModels(models, []string{"none"}); len(got) != 0 {
		t.Fatalf("non-matching allow-list = %v", ids(got))
	}
}
