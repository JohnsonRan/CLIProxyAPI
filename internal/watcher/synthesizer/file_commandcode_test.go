package synthesizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func synthesizeSingleCommandCodeAuth(t *testing.T, authData map[string]any) *coreauth.Auth {
	t.Helper()
	tempDir := t.TempDir()
	data, errMarshal := json.Marshal(authData)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(tempDir, "commandcode-alice.json"), data, 0o600); errWrite != nil {
		t.Fatalf("write auth: %v", errWrite)
	}
	auths, errSynth := NewFileSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynth != nil {
		t.Fatalf("synthesize: %v", errSynth)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	return auths[0]
}

func TestFileSynthesizer_Synthesize_CommandCode(t *testing.T) {
	auth := synthesizeSingleCommandCodeAuth(t, map[string]any{
		"type":      "commandcode",
		"api_key":   " user_secret ",
		"user_name": "alice",
	})
	if auth.Provider != "commandcode" {
		t.Fatalf("provider = %q", auth.Provider)
	}
	if got := auth.Attributes["base_url"]; got != "https://api.commandcode.ai/provider/v1" {
		t.Fatalf("base_url = %q", got)
	}
	if got := auth.Attributes[coreauth.AttributeAPIKey]; got != "user_secret" {
		t.Fatalf("api_key = %q", got)
	}
	if got := auth.AuthKind(); got != coreauth.AuthKindOAuth {
		t.Fatalf("auth kind = %q, want oauth", got)
	}
	if auth.Prefix != "commandcode" {
		t.Fatalf("prefix = %q, want commandcode", auth.Prefix)
	}
	if auth.Label != "alice" {
		t.Fatalf("label = %q, want alice", auth.Label)
	}
}

func TestFileSynthesizer_Synthesize_CommandCodeKeepsOverrides(t *testing.T) {
	auth := synthesizeSingleCommandCodeAuth(t, map[string]any{
		"type":     "commandcode",
		"api_key":  "user_secret",
		"base_url": "https://example.test/provider/v1",
		"prefix":   "cc",
		"email":    "alice@example.com",
	})
	if got := auth.Attributes["base_url"]; got != "https://example.test/provider/v1" {
		t.Fatalf("base_url = %q", got)
	}
	if auth.Prefix != "cc" {
		t.Fatalf("prefix = %q, want cc", auth.Prefix)
	}
	if auth.Label != "alice@example.com" {
		t.Fatalf("label = %q", auth.Label)
	}
}
