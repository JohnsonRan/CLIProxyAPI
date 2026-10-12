package helps

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var codexTestEncodings = []string{
	CodexUpstreamRequestEncodingZstd,
	CodexUpstreamRequestEncodingGzip,
	CodexUpstreamRequestEncodingBr,
}

func newCompressionTestRequest(t *testing.T, rawURL string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func decodeCodexTestBody(t *testing.T, encoding string, raw []byte) []byte {
	t.Helper()
	switch encoding {
	case CodexUpstreamRequestEncodingZstd:
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("zstd reader: %v", err)
		}
		defer dec.Close()
		decoded, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("zstd decode: %v", err)
		}
		return decoded
	case CodexUpstreamRequestEncodingGzip:
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gzip decode: %v", err)
		}
		return decoded
	case CodexUpstreamRequestEncodingBr:
		decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("brotli decode: %v", err)
		}
		return decoded
	default:
		t.Fatalf("unknown encoding %q", encoding)
		return nil
	}
}

func TestNormalizeCodexUpstreamRequestCompression(t *testing.T) {
	cases := map[string]string{
		"":          "zstd",
		"auto":      "zstd",
		"true":      "zstd",
		"zstd":      "zstd",
		" ZSTD ":    "zstd",
		"zstandard": "zstd",
		"gzip":      "gzip",
		"Gzip":      "gzip",
		"br":        "br",
		"brotli":    "br",
		"off":       "",
		"none":      "",
		"false":     "",
		"identity":  "",
		"disabled":  "",
		"deflate":   "",
	}
	for in, want := range cases {
		if got := NormalizeCodexUpstreamRequestCompression(in); got != want {
			t.Fatalf("NormalizeCodexUpstreamRequestCompression(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexUpstreamRequestEncoding(t *testing.T) {
	base := "https://chatgpt.com/backend-api/codex"
	for _, encoding := range codexTestEncodings {
		on := &config.Config{Codex: config.CodexConfig{UpstreamRequestCompression: encoding}}
		cases := []struct {
			name string
			cfg  *config.Config
			url  string
			want string
		}{
			{name: "nil config defaults to zstd", cfg: nil, url: base + "/responses", want: "zstd"},
			{name: "empty config defaults to zstd", cfg: &config.Config{}, url: base + "/responses", want: "zstd"},
			{name: "off", cfg: &config.Config{Codex: config.CodexConfig{UpstreamRequestCompression: "off"}}, url: base + "/responses", want: ""},
			{name: "unsupported coding", cfg: &config.Config{Codex: config.CodexConfig{UpstreamRequestCompression: "deflate"}}, url: base + "/responses", want: ""},
			{name: "responses", cfg: on, url: base + "/responses", want: encoding},
			{name: "compact", cfg: on, url: base + "/responses/compact", want: encoding},
			{name: "images", cfg: on, url: base + "/images/generations", want: ""},
			{name: "custom base url host", cfg: on, url: "https://codex.example.com/v1/responses", want: ""},
		}
		for _, tc := range cases {
			t.Run(encoding+"/"+tc.name, func(t *testing.T) {
				req := newCompressionTestRequest(t, tc.url, nil)
				if got := CodexUpstreamRequestEncoding(tc.cfg, req); got != tc.want {
					t.Fatalf("CodexUpstreamRequestEncoding(%q) = %q, want %q", tc.url, got, tc.want)
				}
			})
		}
	}
}

func TestApplyCodexUpstreamRequestCompressionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-5","instructions":"` + strings.Repeat("You are a helpful assistant. ", 200) + `","input":[]}`)
	for _, encoding := range codexTestEncodings {
		req := newCompressionTestRequest(t, "https://chatgpt.com/backend-api/codex/responses", body)
		applied, err := ApplyCodexUpstreamRequestCompression(req, body, encoding)
		if err != nil {
			t.Fatal(err)
		}
		if !applied || req.Header.Get("Content-Encoding") != encoding {
			t.Fatalf("applied=%v Content-Encoding=%q, want %s", applied, req.Header.Get("Content-Encoding"), encoding)
		}
		raw, _ := io.ReadAll(req.Body)
		if int64(len(raw)) != req.ContentLength {
			t.Fatalf("ContentLength = %d, body = %d", req.ContentLength, len(raw))
		}
		if len(raw) >= len(body) {
			t.Fatalf("compressed body %d not smaller than %d", len(raw), len(body))
		}
		if decoded := decodeCodexTestBody(t, encoding, raw); !bytes.Equal(decoded, body) {
			t.Fatalf("%s round trip mismatch", encoding)
		}
	}

	incompressible := make([]byte, 2048)
	_, _ = rand.Read(incompressible)
	req := newCompressionTestRequest(t, "https://chatgpt.com/backend-api/codex/responses", incompressible)
	req.Header.Set("Content-Encoding", "gzip")
	applied, err := ApplyCodexUpstreamRequestCompression(req, incompressible, CodexUpstreamRequestEncodingZstd)
	if err != nil || applied || req.Header.Get("Content-Encoding") != "" {
		t.Fatalf("incompressible body should stay plain without Content-Encoding: applied=%v err=%v header=%q", applied, err, req.Header.Get("Content-Encoding"))
	}
}
