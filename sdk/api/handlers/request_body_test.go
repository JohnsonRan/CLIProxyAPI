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

func gzipBody(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, errWrite := w.Write(plain); errWrite != nil {
		t.Fatalf("gzip write: %v", errWrite)
	}
	if errClose := w.Close(); errClose != nil {
		t.Fatalf("gzip close: %v", errClose)
	}
	return buf.Bytes()
}

func zstdBody(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, errNew := zstd.NewWriter(&buf)
	if errNew != nil {
		t.Fatalf("zstd writer: %v", errNew)
	}
	if _, errWrite := w.Write(plain); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := w.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	return buf.Bytes()
}

func newBodyRequest(t *testing.T, body []byte, encoding string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

func TestReadRequestBodyDecodesSupportedEncodings(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	for _, tc := range []struct {
		name     string
		encoding string
		body     []byte
	}{
		{name: "plain", encoding: "", body: plain},
		{name: "identity", encoding: "identity", body: plain},
		{name: "zstd", encoding: "zstd", body: zstdBody(t, plain)},
		{name: "gzip", encoding: "gzip", body: gzipBody(t, plain)},
		{name: "x-gzip", encoding: "x-gzip", body: gzipBody(t, plain)},
		{name: "gzip_upper", encoding: "GZIP", body: gzipBody(t, plain)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newBodyRequest(t, tc.body, tc.encoding)
			decoded, err := ReadRequestBody(c)
			if err != nil {
				t.Fatalf("ReadRequestBody: %v", err)
			}
			if !bytes.Equal(decoded, plain) {
				t.Fatalf("decoded = %q, want %q", decoded, plain)
			}
		})
	}
}

func TestReadRequestBodyGzipOverCap(t *testing.T) {
	plain := bytes.Repeat([]byte("a"), 1024)
	c, _ := newBodyRequest(t, gzipBody(t, plain), "gzip")
	c.Set(MaxRequestBodyBytesContextKey, int64(64))

	if _, err := ReadRequestBody(c); err == nil {
		t.Fatalf("expected max bytes error for oversized gzip body")
	} else {
		var maxErr *http.MaxBytesError
		if !asMaxBytesErr(err, &maxErr) {
			t.Fatalf("error = %v (%T), want *http.MaxBytesError", err, err)
		}
	}
}

func TestReadRequestBodyMalformedGzip(t *testing.T) {
	// A gzip-looking body that is not valid JSON and cannot be gunzipped must fail
	// rather than fall through as compressed bytes.
	c, _ := newBodyRequest(t, []byte{0x1f, 0x8b, 0x08, 0x00, 0xde, 0xad}, "gzip")
	if _, err := ReadRequestBody(c); err == nil {
		t.Fatalf("expected decode error for malformed gzip body")
	}
}

func asMaxBytesErr(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if mbe, ok := err.(*http.MaxBytesError); ok {
			*target = mbe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

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
