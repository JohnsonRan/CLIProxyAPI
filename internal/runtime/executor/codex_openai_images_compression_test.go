package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func brotliBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, errWrite := writer.Write(data); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return buf.Bytes()
}

// Codex image requests advertise Accept-Encoding explicitly, so Go no longer
// decodes the response transparently and the executor must do it.
func TestCodexExecutorDirectOpenAIImageDecodesCompressedResponses(t *testing.T) {
	jsonBody := []byte(`{"created":1713833628,"data":[{"b64_json":"AA=="}]}`)
	sseBody := []byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n")

	for _, stream := range []bool{false, true} {
		var gotAcceptEncoding string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAcceptEncoding = r.Header.Get("Accept-Encoding")
			body := jsonBody
			w.Header().Set("Content-Type", "application/json")
			if stream {
				body = sseBody
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(brotliBytes(t, body))
		}))

		executor := NewCodexExecutor(&config.Config{})
		auth := newCodexOpenAIImageTestAuth(server.URL)
		req := cliproxyexecutor.Request{Model: "gpt-image-2", Payload: []byte(`{"model":"gpt-image-2","prompt":"otter"}`)}
		opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, stream)

		var got []byte
		if stream {
			result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
			if errStream != nil {
				t.Fatalf("stream=%v: ExecuteStream() error = %v", stream, errStream)
			}
			var combined bytes.Buffer
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream=%v: chunk error = %v", stream, chunk.Err)
				}
				combined.Write(chunk.Payload)
			}
			got = combined.Bytes()
		} else {
			resp, errExecute := executor.Execute(context.Background(), auth, req, opts)
			if errExecute != nil {
				t.Fatalf("stream=%v: Execute() error = %v", stream, errExecute)
			}
			got = resp.Payload
		}
		server.Close()

		if !strings.Contains(gotAcceptEncoding, "br") {
			t.Fatalf("stream=%v: Accept-Encoding = %q, want br advertised", stream, gotAcceptEncoding)
		}
		if !bytes.Contains(got, []byte(`"b64_json":"AA=="`)) {
			t.Fatalf("stream=%v: response was not decoded: %q", stream, got)
		}
	}
}
