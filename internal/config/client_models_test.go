package config

import (
	"testing"
)

func TestClientModels_PatternMatching(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "gpt-4o", true},
		{"*", "claude-3-7-sonnet", true},
		{"gpt-4o", "gpt-4o", true},
		{"GPT-4O", "gpt-4o", true},
		{"gpt-4o", "GPT-4O", true},
		{"gpt-4o*", "gpt-4o", true},
		{"gpt-4o*", "gpt-4o-mini", true},
		{"gpt-4o*", "gpt-4o-2024-08-06", true},
		{"claude-3-5-*", "claude-3-5-sonnet-20241022", true},
		{"claude-3-5-*", "claude-3-5-haiku", true},
		{"claude-3-5-*", "claude-3-7-sonnet", false},
		{"*-preview", "gemini-2.5-pro-preview", true},
		{"*-preview", "gemini-2.5-pro", false},
		{"gemini-*-flash", "gemini-2.5-flash", true},
		{"gemini-*-flash", "gemini-2.5-pro", false},
	}

	for _, tt := range tests {
		got := MatchModelPattern(tt.pattern, tt.value)
		if got != tt.want {
			t.Errorf("MatchModelPattern(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

func TestClientModels_IsModelAllowedForClient(t *testing.T) {
	cfg := &SDKConfig{
		ClientModels:         []string{"gpt-4o*", "claude-3-5-*"},
		ClientExcludedModels: []string{"*-preview"},
		ClientKeys: []ClientKeyConfig{
			{
				APIKey: "key-default",
			},
			{
				APIKey: "key-custom",
				Models: []string{"gemini-2.5-pro", "gemini-2.5-flash"},
			},
			{
				APIKey:         "key-with-exclusions",
				Models:         []string{"gpt-4o*"},
				ExcludedModels: []string{"gpt-4o-mini"},
			},
			{
				APIKey: "key-admin",
				Models: []string{"*"},
			},
		},
	}

	t.Run("key with default restrictions", func(t *testing.T) {
		key := "key-default"
		if !cfg.IsModelAllowedForClient(key, "gpt-4o") {
			t.Error("expected gpt-4o allowed")
		}
		if !cfg.IsModelAllowedForClient(key, "gpt-4o-mini") {
			t.Error("expected gpt-4o-mini allowed")
		}
		if !cfg.IsModelAllowedForClient(key, "claude-3-5-sonnet-20241022") {
			t.Error("expected claude-3-5-sonnet allowed")
		}
		// Suffix stripping
		if !cfg.IsModelAllowedForClient(key, "claude-3-5-sonnet(high)") {
			t.Error("expected claude-3-5-sonnet(high) allowed via suffix strip")
		}
		// Global exclusion blocks preview
		if cfg.IsModelAllowedForClient(key, "gpt-4o-preview") {
			t.Error("expected gpt-4o-preview blocked by global exclusion")
		}
		// Unlisted model blocked
		if cfg.IsModelAllowedForClient(key, "gemini-2.5-pro") {
			t.Error("expected gemini-2.5-pro blocked")
		}
	})

	t.Run("key with custom restrictions", func(t *testing.T) {
		key := "key-custom"
		if !cfg.IsModelAllowedForClient(key, "gemini-2.5-pro") {
			t.Error("expected gemini-2.5-pro allowed")
		}
		// Gemini models/ prefix stripping
		if !cfg.IsModelAllowedForClient(key, "models/gemini-2.5-pro") {
			t.Error("expected models/gemini-2.5-pro allowed")
		}
		if !cfg.IsModelAllowedForClient(key, "gemini-2.5-flash") {
			t.Error("expected gemini-2.5-flash allowed")
		}
		// Model from global default is overridden and blocked for this key
		if cfg.IsModelAllowedForClient(key, "gpt-4o") {
			t.Error("expected gpt-4o blocked for custom key")
		}
	})

	t.Run("key with custom exclusions", func(t *testing.T) {
		key := "key-with-exclusions"
		if !cfg.IsModelAllowedForClient(key, "gpt-4o") {
			t.Error("expected gpt-4o allowed")
		}
		if cfg.IsModelAllowedForClient(key, "gpt-4o-mini") {
			t.Error("expected gpt-4o-mini blocked by key exclusion")
		}
	})

	t.Run("admin key wildcard", func(t *testing.T) {
		key := "key-admin"
		if !cfg.IsModelAllowedForClient(key, "deepseek-r1") {
			t.Error("expected admin key allows any model")
		}
		if !cfg.IsModelAllowedForClient(key, "gemini-2.5-pro-preview") {
			t.Error("expected admin key allows even preview model")
		}
	})

	t.Run("unconfigured key inherits global", func(t *testing.T) {
		key := "unknown-key"
		if !cfg.IsModelAllowedForClient(key, "gpt-4o") {
			t.Error("expected unconfigured key inherits global gpt-4o")
		}
		if cfg.IsModelAllowedForClient(key, "gemini-2.5-pro") {
			t.Error("expected unconfigured key blocked from gemini-2.5-pro")
		}
	})

	t.Run("empty config allows all", func(t *testing.T) {
		emptyCfg := &SDKConfig{}
		if !emptyCfg.IsModelAllowedForClient("any-key", "any-model") {
			t.Error("expected empty config allows all models")
		}
		if emptyCfg.HasClientModelRestrictions("any-key") {
			t.Error("expected no restrictions for empty config")
		}
	})
}

func TestClientModels_YAMLUnmarshalV8(t *testing.T) {
	yamlData := []byte(`
config-version: 8
server:
  port: 8317
access:
  models:
    - "gpt-4o*"
  excluded-models:
    - "*-preview"
  api-keys:
    - "sk-simple"
    - api-key: "sk-restricted"
      models:
        - "claude-3-5-sonnet"
    - api-key: "sk-admin"
      allowed-models:
        - "*"
`)

	cfg, err := ParseConfigBytes(yamlData)
	if err != nil {
		t.Fatalf("ParseConfigBytes failed: %v", err)
	}

	if len(cfg.APIKeys) != 3 {
		t.Fatalf("cfg.APIKeys len = %d, want 3 (%v)", len(cfg.APIKeys), cfg.APIKeys)
	}
	if cfg.APIKeys[0] != "sk-simple" || cfg.APIKeys[1] != "sk-restricted" || cfg.APIKeys[2] != "sk-admin" {
		t.Fatalf("cfg.APIKeys = %v", cfg.APIKeys)
	}

	if len(cfg.ClientModels) != 1 || cfg.ClientModels[0] != "gpt-4o*" {
		t.Fatalf("cfg.ClientModels = %v, want [gpt-4o*]", cfg.ClientModels)
	}
	if len(cfg.ClientExcludedModels) != 1 || cfg.ClientExcludedModels[0] != "*-preview" {
		t.Fatalf("cfg.ClientExcludedModels = %v, want [*-preview]", cfg.ClientExcludedModels)
	}

	if len(cfg.ClientKeys) != 3 {
		t.Fatalf("cfg.ClientKeys len = %d, want 3", len(cfg.ClientKeys))
	}

	// sk-simple inherits global
	if !cfg.IsModelAllowedForClient("sk-simple", "gpt-4o") {
		t.Error("sk-simple should allow gpt-4o")
	}
	if cfg.IsModelAllowedForClient("sk-simple", "claude-3-5-sonnet") {
		t.Error("sk-simple should block claude-3-5-sonnet")
	}

	// sk-restricted only allows claude-3-5-sonnet
	if !cfg.IsModelAllowedForClient("sk-restricted", "claude-3-5-sonnet") {
		t.Error("sk-restricted should allow claude-3-5-sonnet")
	}
	if cfg.IsModelAllowedForClient("sk-restricted", "gpt-4o") {
		t.Error("sk-restricted should block gpt-4o")
	}

	// sk-admin allows *
	if !cfg.IsModelAllowedForClient("sk-admin", "anything-at-all") {
		t.Error("sk-admin should allow anything")
	}
}
