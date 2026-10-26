package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexConfigRequestCompressionMinBytes(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  *CodexConfig
		want int
	}{
		{name: "nil", want: 1024},
		{name: "zero", cfg: &CodexConfig{}, want: 1024},
		{name: "negative", cfg: &CodexConfig{UpstreamRequestCompressionMinBytes: -1}, want: 1024},
		{name: "explicit", cfg: &CodexConfig{UpstreamRequestCompressionMinBytes: 2048}, want: 2048},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.RequestCompressionMinBytes(); got != tt.want {
				t.Fatalf("RequestCompressionMinBytes()=%d, want %d", got, tt.want)
			}
		})
	}
}

func TestCodexConfigRequestCompressionSerialization(t *testing.T) {
	var cfg CodexConfig
	if cfg.UpstreamRequestCompression {
		t.Fatal("request compression must default to disabled")
	}
	if errUnmarshal := yaml.Unmarshal([]byte("upstream-request-compression: true\nupstream-request-compression-min-bytes: 4096\n"), &cfg); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !cfg.UpstreamRequestCompression || cfg.RequestCompressionMinBytes() != 4096 {
		t.Fatalf("unexpected YAML config: %+v", cfg)
	}
	data, errMarshal := json.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var values map[string]any
	if errUnmarshal := json.Unmarshal(data, &values); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if values["upstream-request-compression"] != true || values["upstream-request-compression-min-bytes"] != float64(4096) {
		t.Fatalf("unexpected JSON: %s", data)
	}
}

func TestCodexRequestCompressionDefaultAndLegacyValues(t *testing.T) {
	for _, tt := range []struct {
		name     string
		yaml     string
		enabled  bool
		encoding string
	}{
		{name: "default on", yaml: "port: 8317\n", enabled: true, encoding: "zstd"},
		{name: "explicit off", yaml: "codex:\n  upstream-request-compression: false\n", enabled: false, encoding: "zstd"},
		{name: "legacy zstd", yaml: "codex:\n  upstream-request-compression: \"zstd\"\n", enabled: true, encoding: "zstd"},
		{name: "legacy auto", yaml: "codex:\n  upstream-request-compression: auto\n", enabled: true, encoding: "zstd"},
		{name: "legacy off", yaml: "codex:\n  upstream-request-compression: \"off\"\n", enabled: false, encoding: "zstd"},
		{name: "legacy gzip", yaml: "codex:\n  upstream-request-compression: gzip\n", enabled: true, encoding: "gzip"},
		{name: "v8 legacy br", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: br\n", enabled: true, encoding: "br"},
		{name: "v8 encoding field", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression-encoding: br\n", enabled: true, encoding: "br"},
		{name: "explicit encoding wins", yaml: "codex:\n  upstream-request-compression: gzip\n  upstream-request-compression-encoding: br\n", enabled: true, encoding: "br"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if cfg.Codex.UpstreamRequestCompression != tt.enabled || cfg.Codex.RequestCompressionEncoding() != tt.encoding {
				t.Fatalf("enabled=%v encoding=%q, want %v %q", cfg.Codex.UpstreamRequestCompression, cfg.Codex.RequestCompressionEncoding(), tt.enabled, tt.encoding)
			}
		})
	}
}
