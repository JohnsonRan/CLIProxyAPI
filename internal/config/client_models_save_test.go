package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Struct-based saves (plugin install, legacy management endpoints) must keep
// the per-key model rules, otherwise the affected keys silently become unrestricted.
func TestClientKeyRulesSurviveStructSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	source := `config-version: 8
access:
  api-keys:
    - plain-key
    # restricted CI key
    - api-key: scoped-key
      models:
        - gpt-5*
      excluded-models:
        - gpt-5-pro
    - api-key: removed-key
      models:
        - claude-*
    - api-key: named-key
      name: Alice
    - api-key: paused-key
      disabled: true
`
	if errWrite := os.WriteFile(path, []byte(source), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("load: %v", errLoad)
	}

	// An unrelated change plus a key removal, as a legacy endpoint would make.
	cfg.Debug = !cfg.Debug
	cfg.APIKeys = []string{"plain-key", "scoped-key", "named-key", "paused-key"}
	if errSave := SaveConfigPreserveComments(path, cfg, true); errSave != nil {
		t.Fatalf("save: %v", errSave)
	}

	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	saved := string(data)
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", errValidate, saved)
	}
	if strings.Contains(saved, "client-keys") {
		t.Fatalf("internal client-keys list leaked into the saved file:\n%s", saved)
	}
	if strings.Contains(saved, "removed-key") {
		t.Fatalf("removed key came back from its stale rules:\n%s", saved)
	}
	// Comments attached to mapping entries are not preserved: flattenV8 splits the
	// entry before the merge, so only the rules themselves are asserted here.

	reloaded, errReload := LoadConfig(path)
	if errReload != nil {
		t.Fatalf("reload: %v", errReload)
	}
	if !reloaded.HasClientModelRestrictions("scoped-key") {
		t.Fatalf("scoped-key lost its model rules after save:\n%s", saved)
	}
	if !reloaded.IsModelAllowedForClient("scoped-key", "gpt-5") || reloaded.IsModelAllowedForClient("scoped-key", "gpt-5-pro") {
		t.Fatalf("scoped-key rules changed after save:\n%s", saved)
	}
	if reloaded.HasClientModelRestrictions("plain-key") {
		t.Fatalf("plain-key gained restrictions after save:\n%s", saved)
	}
	if got := strings.Join(reloaded.APIKeys, ","); got != "plain-key,scoped-key,named-key,paused-key" {
		t.Fatalf("APIKeys = %q, want plain-key,scoped-key,named-key,paused-key", got)
	}
	// A name alone keeps the entry a mapping but adds no model restriction.
	if reloaded.HasClientModelRestrictions("named-key") {
		t.Fatalf("named-key gained restrictions from its name:\n%s", saved)
	}
	names := map[string]string{}
	disabled := map[string]bool{}
	for _, entry := range reloaded.ClientKeys {
		names[entry.APIKey] = entry.Name
		disabled[entry.APIKey] = entry.Disabled
	}
	// A disabled flag alone must survive the save, or the key silently becomes usable again.
	if !disabled["paused-key"] || disabled["plain-key"] {
		t.Fatalf("client key disabled flags after save = %v:\n%s", disabled, saved)
	}
	if names["named-key"] != "Alice" || names["plain-key"] != "" {
		t.Fatalf("client key names after save = %v:\n%s", names, saved)
	}
}
