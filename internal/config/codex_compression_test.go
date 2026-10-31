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
		name    string
		yaml    string
		enabled bool
	}{
		{name: "default on", yaml: "port: 8317\n", enabled: true},
		{name: "explicit off", yaml: "codex:\n  upstream-request-compression: false\n", enabled: false},
		{name: "legacy zstd", yaml: "codex:\n  upstream-request-compression: \"zstd\"\n", enabled: true},
		{name: "legacy unquoted zstd", yaml: "codex:\n  upstream-request-compression: zstd\n", enabled: true},
		{name: "legacy auto", yaml: "codex:\n  upstream-request-compression: auto\n", enabled: true},
		{name: "legacy off", yaml: "codex:\n  upstream-request-compression: \"off\"\n", enabled: false},
		{name: "legacy gzip means zstd on", yaml: "codex:\n  upstream-request-compression: gzip\n", enabled: true},
		{name: "legacy no", yaml: "codex:\n  upstream-request-compression: no\n", enabled: false},
		{name: "legacy N", yaml: "codex:\n  upstream-request-compression: N\n", enabled: false},
		{name: "legacy yes", yaml: "codex:\n  upstream-request-compression: yes\n", enabled: true},
		{name: "legacy none", yaml: "codex:\n  upstream-request-compression: none\n", enabled: false},
		{name: "v8 legacy br means zstd on", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: br\n", enabled: true},
		{name: "v8 legacy off", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: \"off\"\n", enabled: false},
		{name: "v8 zstd", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: zstd\n", enabled: true},
		{name: "v8 no", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: no\n", enabled: false},
		{name: "v8 N", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: N\n", enabled: false},
		{name: "v8 yes", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: yes\n", enabled: true},
		{name: "v8 none", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: none\n", enabled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if cfg.Codex.UpstreamRequestCompression != tt.enabled {
				t.Fatalf("enabled=%v, want %v", cfg.Codex.UpstreamRequestCompression, tt.enabled)
			}
		})
	}

	for _, bad := range []struct {
		name string
		yaml string
	}{
		{name: "legacy integer rejected", yaml: "codex:\n  upstream-request-compression: 0\n"},
		{name: "v8 integer rejected", yaml: "config-version: 8\nupstream:\n  codex:\n    upstream-request-compression: 0\n"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, errParse := ParseConfigBytes([]byte(bad.yaml)); errParse == nil {
				t.Fatalf("%s must fail to parse", bad.name)
			}
		})
	}
}
