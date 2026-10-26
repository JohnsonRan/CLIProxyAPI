package middleware

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// With max-request-body-bytes unset, the pre-auth log capture must still bound
// a header-less (magic-byte) gzip bomb.
func TestCaptureRequestInfoBoundsDecodedBodyWithDefaultConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var compressed bytes.Buffer
	writer, errWriter := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if errWriter != nil {
		t.Fatal(errWriter)
	}
	chunk := make([]byte, 1<<20)
	for i := int64(0); i < maxDeferredErrorRequestBodyBytes/int64(len(chunk))+8; i++ {
		if _, errWrite := writer.Write(chunk); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/anything", bytes.NewReader(compressed.Bytes()))
	info, errCapture := captureRequestInfo(c, true)
	if errCapture != nil {
		t.Fatalf("captureRequestInfo: %v", errCapture)
	}
	if int64(len(info.Body)) > maxDeferredErrorRequestBodyBytes+64 {
		t.Fatalf("logged body = %d bytes, want at most about %d", len(info.Body), maxDeferredErrorRequestBodyBytes)
	}
	if !bytes.Contains(info.Body, []byte("DECOMPRESSED REQUEST BODY TRUNCATED")) {
		t.Fatalf("expected truncation marker, body length %d", len(info.Body))
	}
}

// A zstd frame that declares a window above the cap must be rejected instead of
// allocating that window.
func TestDecodeCapturedRequestBodyRejectsHugeZstdWindow(t *testing.T) {
	// Magic, descriptor without content size, window descriptor 0x80 (2^26 = 64 MiB),
	// then one last raw block holding "x".
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x80, 0x09, 0x00, 0x00, 0x78}
	if _, _, errDecode := decodeSingleCapturedRequestBodyWithLimit(frame, "zstd", 1<<30); errDecode == nil {
		t.Fatal("expected huge zstd window to be rejected")
	}
}
