package config

import "testing"

func TestQuotaAPIV8Path(t *testing.T) {
	data := []byte("observability:\n  quota-api:\n    enabled: true\n    cache-ttl-seconds: 120\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("quota-api must be valid in the v8 layout: %v", err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.QuotaAPI.Enabled || cfg.QuotaAPI.CacheTTLSeconds != 120 {
		t.Fatalf("quota-api = %+v", cfg.QuotaAPI)
	}
}
