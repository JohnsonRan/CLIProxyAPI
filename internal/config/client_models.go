package config

import (
	"strings"
)

// ClientKeyConfig configures per-client API key authentication and model permissions.
type ClientKeyConfig struct {
	// APIKey is the client authentication key.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is an optional display name for management clients. It does not affect access.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Disabled rejects requests authenticated with this key while keeping it in the config.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Models lists visible and usable model IDs or patterns (e.g. "gpt-4o", "claude-*").
	// When non-empty, only matching models are visible and usable for this API key.
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`

	// AllowedModels is an alias for Models.
	AllowedModels []string `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`

	// ExcludedModels lists model IDs or patterns that cannot be seen or used by this API key.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

// AllowedModelList returns the effective allowed model patterns configured for this key.
func (c *ClientKeyConfig) AllowedModelList() []string {
	if c == nil {
		return nil
	}
	if len(c.Models) == 0 {
		return c.AllowedModels
	}
	if len(c.AllowedModels) == 0 {
		return c.Models
	}
	combined := append(append([]string(nil), c.Models...), c.AllowedModels...)
	return NormalizeExcludedModels(combined)
}

// SanitizeClientKeys normalizes ClientModels, ClientExcludedModels, and ClientKeys entries.
func (cfg *Config) SanitizeClientKeys() {
	if cfg == nil {
		return
	}
	cfg.ClientModels = NormalizeExcludedModels(cfg.ClientModels)
	cfg.ClientExcludedModels = NormalizeExcludedModels(cfg.ClientExcludedModels)

	if len(cfg.ClientKeys) == 0 {
		if len(cfg.APIKeys) > 0 {
			cfg.ClientKeys = make([]ClientKeyConfig, 0, len(cfg.APIKeys))
			for _, k := range cfg.APIKeys {
				trimmed := strings.TrimSpace(k)
				if trimmed != "" {
					cfg.ClientKeys = append(cfg.ClientKeys, ClientKeyConfig{
						APIKey: trimmed,
					})
				}
			}
		}
		return
	}

	normalized := make([]ClientKeyConfig, 0, len(cfg.ClientKeys))
	seen := make(map[string]struct{}, len(cfg.ClientKeys))
	for _, entry := range cfg.ClientKeys {
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		if _, exists := seen[entry.APIKey]; exists {
			continue
		}
		seen[entry.APIKey] = struct{}{}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Models = NormalizeExcludedModels(entry.Models)
		entry.AllowedModels = NormalizeExcludedModels(entry.AllowedModels)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		normalized = append(normalized, entry)
	}
	cfg.ClientKeys = normalized
}

// HasClientModelRestrictions reports whether any model visibility/usability restriction
// applies to the given client API key (or globally).
func (cfg *SDKConfig) HasClientModelRestrictions(apiKey string) bool {
	if cfg == nil {
		return false
	}
	cleanedKey := strings.TrimSpace(apiKey)
	if cleanedKey != "" {
		for i := range cfg.ClientKeys {
			if strings.TrimSpace(cfg.ClientKeys[i].APIKey) == cleanedKey {
				if len(cfg.ClientKeys[i].AllowedModelList()) > 0 || len(cfg.ClientKeys[i].ExcludedModels) > 0 {
					return true
				}
				break
			}
		}
	}
	return len(cfg.ClientModels) > 0 || len(cfg.ClientExcludedModels) > 0
}

// GetClientModelRules returns the effective allowed patterns and excluded patterns for an API key.
func (cfg *SDKConfig) GetClientModelRules(apiKey string) (allowed []string, excluded []string) {
	if cfg == nil {
		return nil, nil
	}
	cleanedKey := strings.TrimSpace(apiKey)
	var matchedKey *ClientKeyConfig
	if cleanedKey != "" {
		for i := range cfg.ClientKeys {
			if strings.TrimSpace(cfg.ClientKeys[i].APIKey) == cleanedKey {
				matchedKey = &cfg.ClientKeys[i]
				break
			}
		}
	}

	if matchedKey != nil {
		keyAllowed := matchedKey.AllowedModelList()
		if len(keyAllowed) > 0 {
			// Per-key explicit allowed models take full precedence over global defaults.
			allowed = keyAllowed
			excluded = matchedKey.ExcludedModels
			return allowed, excluded
		}

		allowed = cfg.ClientModels
		if len(matchedKey.ExcludedModels) > 0 {
			excluded = matchedKey.ExcludedModels
		} else {
			excluded = cfg.ClientExcludedModels
		}
		return allowed, excluded
	}

	return cfg.ClientModels, cfg.ClientExcludedModels
}

// IsModelAllowedForClient reports whether a model is allowed (visible and usable) for the given client API key.
func (cfg *SDKConfig) IsModelAllowedForClient(apiKey, modelID string) bool {
	if cfg == nil {
		return true
	}
	trimmedModel := strings.TrimSpace(modelID)
	if trimmedModel == "" {
		return true
	}

	allowed, excluded := cfg.GetClientModelRules(apiKey)
	if len(allowed) == 0 && len(excluded) == 0 {
		return true
	}

	baseModel := stripThinkingSuffix(trimmedModel)
	unprefixed := strings.TrimPrefix(trimmedModel, "models/")
	unprefixedBase := strings.TrimPrefix(baseModel, "models/")

	candidates := []string{trimmedModel, baseModel, unprefixed, unprefixedBase}

	// 1. Check excluded patterns first.
	if len(excluded) > 0 {
		for _, pat := range excluded {
			for _, candidate := range candidates {
				if MatchModelPattern(pat, candidate) {
					return false
				}
			}
		}
	}

	// 2. Check allowed patterns if specified.
	if len(allowed) > 0 {
		for _, pat := range allowed {
			for _, candidate := range candidates {
				if MatchModelPattern(pat, candidate) {
					return true
				}
			}
		}
		return false
	}

	return true
}

// FilterModelsForClient filters a slice of model IDs according to client API key permissions.
func (cfg *SDKConfig) FilterModelsForClient(apiKey string, modelIDs []string) []string {
	if cfg == nil || len(modelIDs) == 0 {
		return modelIDs
	}
	if !cfg.HasClientModelRestrictions(apiKey) {
		return modelIDs
	}
	filtered := make([]string, 0, len(modelIDs))
	for _, m := range modelIDs {
		if cfg.IsModelAllowedForClient(apiKey, m) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// MatchModelPattern matches a model name against a pattern (case-insensitive, supporting '*').
func MatchModelPattern(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" || value == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}

func stripThinkingSuffix(model string) string {
	idx := strings.Index(model, "(")
	if idx > 0 && strings.HasSuffix(model, ")") {
		return strings.TrimSpace(model[:idx])
	}
	return model
}
