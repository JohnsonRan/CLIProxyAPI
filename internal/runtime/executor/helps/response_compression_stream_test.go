package helps

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

// A streaming upstream may send headers and then wait; decoding must not block
// until its first byte, or callers stall before their bootstrap timers start.
func TestDecodeHTTPResponseBodyDoesNotWaitForFirstByte(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	resp := &http.Response{Header: http.Header{}, Body: reader}

	done := make(chan error, 1)
	go func() { done <- DecodeHTTPResponseBody(resp) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DecodeHTTPResponseBody blocked waiting for the first body byte")
	}

	go func() {
		_, _ = writer.Write([]byte("data: {}\n\n"))
		_ = writer.Close()
	}()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "data: {}\n\n" {
		t.Fatalf("body = %q, %v", got, err)
	}
}

func TestDecodeHTTPResponseBodySniffsMagicBytesWithoutContentEncoding(t *testing.T) {
	payload := []byte(`{"ok":true}`)
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(encodeTestResponsePayload(t, encoding, payload)))}
			if err := DecodeHTTPResponseBody(resp); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != string(payload) {
				t.Fatalf("body = %q, %v", got, err)
			}
		})
	}
}
