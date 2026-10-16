package config

import "testing"

func TestQuotaAPIV8Path(t *testing.T) {
	data := []byte("observability:\n  quota-api:\n    enabled: true\n    cache-ttl-seconds: 120\n    keys:\n      - api-key: sk-a\n        credentials: [codex-a.json, \" claude.json \"]\n      - api-key: sk-a\n        credentials: [xai.json, \"\"]\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("quota-api must be valid in the v8 layout: %v", err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaAPI.CacheTTLSeconds != 120 || len(cfg.QuotaAPI.Keys) != 2 {
		t.Fatalf("quota-api = %+v", cfg.QuotaAPI)
	}
	visible := cfg.QuotaAPI.VisibleCredentials(" sk-a ")
	for _, id := range []string{"codex-a.json", "claude.json", "xai.json"} {
		if _, ok := visible[id]; !ok {
			t.Fatalf("visible = %v, missing %s", visible, id)
		}
	}
	if len(visible) != 3 {
		t.Fatalf("visible = %v", visible)
	}
	if got := cfg.QuotaAPI.VisibleCredentials("sk-b"); len(got) != 0 {
		t.Fatalf("unlisted key sees %v", got)
	}
	if got := cfg.QuotaAPI.VisibleCredentials(""); len(got) != 0 {
		t.Fatalf("empty key sees %v", got)
	}
}
