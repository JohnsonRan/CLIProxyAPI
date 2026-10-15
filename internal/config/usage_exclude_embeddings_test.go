package config

import "testing"

func TestUsageExcludeEmbeddingsUsesV8Layout(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("observability:\n  usage:\n    usage-statistics-enabled: true\n    exclude-embeddings: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.UsageExcludeEmbeddings || !cfg.UsageStatisticsEnabled {
		t.Fatalf("exclude-embeddings=%v usage-statistics-enabled=%v, want both true", cfg.UsageExcludeEmbeddings, cfg.UsageStatisticsEnabled)
	}
	if errValidate := ValidateV8Config([]byte("observability:\n  usage:\n    exclude-embeddings: true\n")); errValidate != nil {
		t.Fatalf("exclude-embeddings is not a valid v8 key: %v", errValidate)
	}
}
