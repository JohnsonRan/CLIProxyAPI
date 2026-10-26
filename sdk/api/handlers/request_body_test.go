package handlers

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func compressRequestTestPayload(t *testing.T, encoding string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	case "br":
		bw := brotli.NewWriter(&buf)
		if _, err := bw.Write(payload); err != nil {
			t.Fatalf("brotli write: %v", err)
		}
		if err := bw.Close(); err != nil {
			t.Fatalf("brotli close: %v", err)
		}
	case "zstd":
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		if _, err := zw.Write(payload); err != nil {
			t.Fatalf("zstd write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}
	case "deflate-zlib":
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}
	case "deflate-raw":
		fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			t.Fatalf("flate writer: %v", err)
		}
		if _, err := fw.Write(payload); err != nil {
			t.Fatalf("flate write: %v", err)
		}
		if err := fw.Close(); err != nil {
			t.Fatalf("flate close: %v", err)
		}
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}
	return buf.Bytes()
}

func TestReadRequestBodyDecodesAllSupportedEncodings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello world"}]}`)

	cases := []struct {
		name            string
		compressFormat  string
		contentEncoding string
	}{
		{name: "gzip", compressFormat: "gzip", contentEncoding: "gzip"},
		{name: "x-gzip", compressFormat: "gzip", contentEncoding: "x-gzip"},
		{name: "brotli", compressFormat: "br", contentEncoding: "br"},
		{name: "zstd", compressFormat: "zstd", contentEncoding: "zstd"},
		{name: "deflate zlib", compressFormat: "deflate-zlib", contentEncoding: "deflate"},
		{name: "deflate raw", compressFormat: "deflate-raw", contentEncoding: "deflate"},
		{name: "magic gzip without header", compressFormat: "gzip", contentEncoding: ""},
		{name: "magic zstd without header", compressFormat: "zstd", contentEncoding: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed := compressRequestTestPayload(t, tc.compressFormat, payload)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(compressed))
			if tc.contentEncoding != "" {
				c.Request.Header.Set("Content-Encoding", tc.contentEncoding)
			}

			got, err := ReadRequestBody(c)
			if err != nil {
				t.Fatalf("ReadRequestBody() error = %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("ReadRequestBody() = %q, want %q", got, payload)
			}
		})
	}
}

func TestReadRequestBodyChainedEncodingsAndMaxBytesLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := []byte(`{"model":"gpt-5","input":"` + strings.Repeat("a", 512) + `"}`)

	gzipped := compressRequestTestPayload(t, "gzip", payload)
	brotliOverGzip := compressRequestTestPayload(t, "br", gzipped)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(brotliOverGzip))
	c.Request.Header.Set("Content-Encoding", "gzip, br")

	got, err := ReadRequestBody(c)
	if err != nil {
		t.Fatalf("chained ReadRequestBody() error = %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("chained ReadRequestBody() = %q, want %q", got, payload)
	}

	for _, encoding := range []string{"gzip", "br", "zstd", "deflate"} {
		t.Run("maxBytes_"+encoding, func(t *testing.T) {
			compressFormat := encoding
			if encoding == "deflate" {
				compressFormat = "deflate-zlib"
			}
			compressed := compressRequestTestPayload(t, compressFormat, payload)
			recLimit := httptest.NewRecorder()
			cLimit, _ := gin.CreateTestContext(recLimit)
			cLimit.Set(MaxRequestBodyBytesContextKey, int64(64))
			cLimit.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed))
			cLimit.Request.Header.Set("Content-Encoding", encoding)

			_, errLimit := ReadRequestBody(cLimit)
			var maxBytesErr *http.MaxBytesError
			if !errors.As(errLimit, &maxBytesErr) {
				t.Fatalf("ReadRequestBody() error = %v, want *http.MaxBytesError", errLimit)
			}
		})
	}
}

func TestEffectiveDecodedRequestBodyLimitDefaultsWhenUnset(t *testing.T) {
	if got := EffectiveDecodedRequestBodyLimit(0); got != DefaultMaxDecodedRequestBodyBytes {
		t.Fatalf("limit for 0 = %d, want default %d", got, DefaultMaxDecodedRequestBodyBytes)
	}
	if got := EffectiveDecodedRequestBodyLimit(1024); got != 1024 {
		t.Fatalf("limit for 1024 = %d, want 1024", got)
	}
}

func TestDecodeRequestBodyRejectsHugeZstdWindow(t *testing.T) {
	// Magic, descriptor without content size, window descriptor 0x80 (2^26 = 64 MiB),
	// then one last raw block holding "x".
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x80, 0x09, 0x00, 0x00, 0x78}
	if _, errDecode := decodeRequestBody(frame, "zstd", 0); errDecode == nil {
		t.Fatal("expected huge zstd window to be rejected")
	}
}
