package synthesizer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestSynthesizeAuthFileAllowedModels(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: authDir, Now: time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(authDir, "codex.json")
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"array", `{"type":"codex","email":"a@b.c","allowed_models":[" GPT-* ","o3","gpt-*"]}`, "gpt-*,o3"},
		{"comma string", `{"type":"codex","email":"a@b.c","allowed_models":"o3, gpt-*"}`, "gpt-*,o3"},
		{"invalid keeps valid entries", `{"type":"codex","email":"a@b.c","allowed_models":["o3",1]}`, "o3"},
		{"empty is unrestricted", `{"type":"codex","email":"a@b.c","allowed_models":[]}`, ""},
		{"absent", `{"type":"codex","email":"a@b.c"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths := mustSynthesizeAuthFile(t, ctx, path, tc.raw)
			if len(auths) != 1 {
				t.Fatalf("got %d auths, want 1", len(auths))
			}
			got, exists := auths[0].Attributes["allowed_models"]
			if got != tc.want || (tc.want == "" && exists) {
				t.Fatalf("allowed_models = %q (present %v), want %q", got, exists, tc.want)
			}
		})
	}
}
