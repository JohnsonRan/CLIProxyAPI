package cliproxy

import (
	"context"
	"errors"
	"testing"
	"time"

	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func stubCommandCodeCatalog(t *testing.T, fetch commandCodeCatalogFetcher) {
	t.Helper()
	previous := commandCodeModels
	commandCodeModels = &commandCodeModelCatalog{fetch: fetch}
	t.Cleanup(func() { commandCodeModels = previous })
}

func TestRegisterModelsForAuthCommandCodePublishesPrefixedModelsOnly(t *testing.T) {
	stubCommandCodeCatalog(t, func(context.Context, *config.Config, *coreauth.Auth) ([]*internalregistry.ModelInfo, error) {
		return commandcodeauth.ParseModels([]byte(`{"data":[
			{"id":"gpt-6-luna","supported_endpoints":["/chat/completions","/responses"]},
			{"id":"deepseek/deepseek-v4-pro","supported_endpoints":["/chat/completions"]},
			{"id":"claude-opus-5-5","supported_endpoints":["/messages"]}
		]}`), 1), nil
	})

	const authID = "commandcode-registration-test"
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{}}
	service.registerModelsForAuth(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: commandcodeauth.ProviderKey,
		Prefix:   commandcodeauth.DefaultModelPrefix,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
			coreauth.AttributeAPIKey:   "user_test",
			"base_url":                 commandcodeauth.ProviderBaseURL,
		},
	})

	got := map[string]bool{}
	for _, model := range modelRegistry.GetModelsForClient(authID) {
		got[model.ID] = true
	}
	want := []string{"commandcode/gpt-6-luna", "commandcode/deepseek/deepseek-v4-pro"}
	if len(got) != len(want) {
		t.Fatalf("registered models = %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("missing registered model %q (got %v)", id, got)
		}
	}
}

func TestCommandCodeModelCatalogKeepsLastGoodListAndRefetchesWhenStale(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	failing := false
	catalog := &commandCodeModelCatalog{
		now: func() time.Time { return now },
		fetch: func(context.Context, *config.Config, *coreauth.Auth) ([]*internalregistry.ModelInfo, error) {
			calls++
			if failing {
				return nil, errors.New("upstream down")
			}
			return []*internalregistry.ModelInfo{{ID: "m1"}}, nil
		},
	}

	if got := catalog.get(context.Background(), nil, nil); len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("first get = %v", got)
	}
	got := catalog.get(context.Background(), nil, nil)
	if calls != 1 {
		t.Fatalf("fresh catalog refetched: calls = %d", calls)
	}
	got[0].ID = "mutated"

	now = now.Add(commandCodeCatalogTTL)
	failing = true
	got = catalog.get(context.Background(), nil, nil)
	if calls != 2 {
		t.Fatalf("stale catalog not refetched: calls = %d", calls)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("failed refetch should keep the last good, unmutated list, got %v", got)
	}
}
