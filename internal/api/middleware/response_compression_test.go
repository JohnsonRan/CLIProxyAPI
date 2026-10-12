package middleware

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func decodeCompressedResponseTestBytes(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("zstd.NewReader: %v", err)
		}
		defer dec.Close()
		out, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("zstd read: %v", err)
		}
		return out
	case "br":
		out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
		if err != nil {
			t.Fatalf("brotli read: %v", err)
		}
		return out
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		defer func() { _ = zr.Close() }()
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gzip read: %v", err)
		}
		return out
	case "deflate":
		fr := flate.NewReader(bytes.NewReader(data))
		defer func() { _ = fr.Close() }()
		out, err := io.ReadAll(fr)
		if err != nil {
			t.Fatalf("deflate read: %v", err)
		}
		return out
	default:
		t.Fatalf("unsupported encoding %q", encoding)
		return nil
	}
}

func TestNegotiateResponseContentEncoding(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{header: "", want: ""},
		{header: "identity", want: ""},
		{header: "gzip", want: "gzip"},
		{header: "br", want: "br"},
		{header: "zstd", want: "zstd"},
		{header: "deflate", want: "deflate"},
		{header: "gzip, deflate, br, zstd", want: "zstd"},
		{header: "gzip, deflate, br", want: "br"},
		{header: "gzip;q=0.9, br;q=0.8, zstd;q=0.5", want: "gzip"},
		{header: "zstd;q=0, br;q=0.7, gzip;q=0.7", want: "br"},
		{header: "gzip;q=0.4, identity;q=0.9", want: ""},
		{header: "*;q=0.5", want: "zstd"},
	}
	for _, tc := range cases {
		if got := negotiateResponseContentEncoding([]string{tc.header}); got != tc.want {
			t.Fatalf("negotiateResponseContentEncoding(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestResponseCompressionMiddlewareJSONAndSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(ResponseCompressionMiddleware())
	router.GET("/v1/models", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": strings.Repeat("model-entry-", 64)})
	})
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
		c.Writer.Flush()
		_, _ = c.Writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
		c.Writer.Flush()
	})
	router.OPTIONS("/v1/responses", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusNoContent)
	})

	for _, enc := range []string{"zstd", "br", "gzip", "deflate"} {
		t.Run("json_"+enc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Accept-Encoding", enc)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != enc {
				t.Fatalf("Content-Encoding = %q, want %q", got, enc)
			}
			if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
				t.Fatalf("Vary = %q, want Accept-Encoding", rec.Header().Get("Vary"))
			}
			decoded := decodeCompressedResponseTestBytes(t, enc, rec.Body.Bytes())
			if !bytes.Contains(decoded, []byte("model-entry-")) {
				t.Fatalf("decoded JSON missing expected content: %s", decoded)
			}
		})

		t.Run("sse_"+enc, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`))
			req.Header.Set("Accept-Encoding", enc)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != enc {
				t.Fatalf("Content-Encoding = %q, want %q", got, enc)
			}
			decoded := decodeCompressedResponseTestBytes(t, enc, rec.Body.Bytes())
			if !bytes.Contains(decoded, []byte("event: response.created")) || !bytes.Contains(decoded, []byte("event: response.completed")) {
				t.Fatalf("decoded SSE missing expected events: %s", decoded)
			}
		})
	}

	t.Run("no_content_uncompressed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
		req.Header.Set("Accept-Encoding", "zstd, br, gzip")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("204 Content-Encoding = %q, want empty", got)
		}
	})
}
