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

func TestRegisterModelsForAuthCommandCodeRespectsPrefixConfig(t *testing.T) {
	stubCommandCodeCatalog(t, func(context.Context, *config.Config, *coreauth.Auth) ([]*internalregistry.ModelInfo, error) {
		return commandcodeauth.ParseModels([]byte(`{"data":[
			{"id":"gpt-6-luna","supported_endpoints":["/chat/completions","/responses"]},
			{"id":"deepseek/deepseek-v4-pro","supported_endpoints":["/chat/completions"]},
			{"id":"claude-opus-5-5","supported_endpoints":["/messages"]}
		]}`), 1), nil
	})

	tests := []struct {
		name   string
		prefix string
		force  bool
		want   []string
	}{
		{name: "no prefix", want: []string{"gpt-6-luna", "deepseek/deepseek-v4-pro"}},
		{name: "prefix", prefix: "cc", want: []string{"gpt-6-luna", "deepseek/deepseek-v4-pro", "cc/gpt-6-luna", "cc/deepseek/deepseek-v4-pro"}},
		{name: "forced prefix", prefix: "cc", force: true, want: []string{"cc/gpt-6-luna", "cc/deepseek/deepseek-v4-pro"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const authID = "commandcode-registration-test"
			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(authID)
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			cfg := &config.Config{}
			cfg.ForceModelPrefix = tc.force
			service := &Service{cfg: cfg}
			service.registerModelsForAuth(context.Background(), &coreauth.Auth{
				ID:       authID,
				Provider: commandcodeauth.ProviderKey,
				Prefix:   tc.prefix,
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
			if len(got) != len(tc.want) {
				t.Fatalf("registered models = %v, want %v", got, tc.want)
			}
			for _, id := range tc.want {
				if !got[id] {
					t.Errorf("missing registered model %q (got %v)", id, got)
				}
			}
		})
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

	catalog.get(context.Background(), nil, nil)
	if calls != 2 {
		t.Fatalf("failed fetch retried before commandCodeCatalogRetry: calls = %d", calls)
	}
	now = now.Add(commandCodeCatalogRetry)
	failing = false
	catalog.get(context.Background(), nil, nil)
	catalog.get(context.Background(), nil, nil)
	if calls != 3 {
		t.Fatalf("catalog not refetched once after the retry delay: calls = %d", calls)
	}
}
