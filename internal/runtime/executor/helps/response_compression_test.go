package helps

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func encodeTestResponsePayload(t *testing.T, encoding string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
	case "br":
		bw := brotli.NewWriter(&buf)
		_, _ = bw.Write(payload)
		_ = bw.Close()
	case "zstd":
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = zw.Write(payload)
		_ = zw.Close()
	case "deflate-zlib":
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
	case "deflate-raw":
		fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = fw.Write(payload)
		_ = fw.Close()
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}
	return buf.Bytes()
}

func TestDecodeHTTPResponseBodyAllEncodings(t *testing.T) {
	payload := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	cases := []struct {
		name            string
		format          string
		contentEncoding string
	}{
		{name: "gzip", format: "gzip", contentEncoding: "gzip"},
		{name: "brotli", format: "br", contentEncoding: "br"},
		{name: "zstd", format: "zstd", contentEncoding: "zstd"},
		{name: "deflate zlib", format: "deflate-zlib", contentEncoding: "deflate"},
		{name: "deflate raw", format: "deflate-raw", contentEncoding: "deflate"},
		{name: "magic gzip", format: "gzip", contentEncoding: ""},
		{name: "magic zstd", format: "zstd", contentEncoding: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := encodeTestResponsePayload(t, tc.format, payload)
			header := http.Header{}
			if tc.contentEncoding != "" {
				header.Set("Content-Encoding", tc.contentEncoding)
			}
			resp := &http.Response{
				StatusCode:    http.StatusOK,
				Header:        header,
				Body:          io.NopCloser(bytes.NewReader(encoded)),
				ContentLength: int64(len(encoded)),
			}

			if err := DecodeHTTPResponseBody(resp); err != nil {
				t.Fatalf("DecodeHTTPResponseBody() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding after decode = %q, want empty", got)
			}
			decoded, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if !bytes.Equal(decoded, payload) {
				t.Fatalf("decoded = %q, want %q", decoded, payload)
			}
		})
	}
}
