package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type codexCompressionCapture struct {
	path          string
	encoding      string
	contentLength int64
	body          []byte
	streamOutput  []byte
	requestLog    string
}

const codexCompressionTestFiller = "The quick brown fox jumps over the lazy dog. "

func runCodexCompressionRequest(t *testing.T, encoding string, alt string, stream bool, extraAttrs map[string]string, filler string) codexCompressionCapture {
	t.Helper()
	completed := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	compacted := []byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	payload := []byte(`{"model":"gpt-5.5","instructions":"You are helpful.","input":[{"type":"message","role":"user","content":"` + filler + `"}]}`)
	if alt == "responses/compact" {
		payload = []byte(`{"model":"gpt-5.5","instructions":"You are helpful.","input":[{"type":"message","role":"user","content":"` + filler + `"},{"type":"compaction_trigger"}]}`)
	}

	var got codexCompressionCapture
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		got.path = req.URL.Path
		got.encoding = req.Header.Get("Content-Encoding")
		got.contentLength = req.ContentLength
		raw, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("read upstream body: %v", errRead)
		}
		got.body = raw
		if alt == "responses/compact" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(compacted)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(completed)),
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{RequestLog: true}, Codex: config.CodexConfig{UpstreamRequestCompression: encoding}})
	attrs := map[string]string{
		"base_url": "https://chatgpt.com/backend-api/codex",
		"api_key":  "test",
	}
	for k, v := range extraAttrs {
		attrs[k] = v
	}
	auth := &cliproxyauth.Auth{Attributes: attrs}
	req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: alt, Stream: stream}

	if stream {
		result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
		if errStream != nil {
			t.Fatalf("ExecuteStream error: %v", errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream error: %v", chunk.Err)
			}
			got.streamOutput = append(got.streamOutput, chunk.Payload...)
		}
	} else if _, errExec := executor.Execute(ctx, auth, req, opts); errExec != nil {
		t.Fatalf("Execute error: %v", errExec)
	}
	got.requestLog = recordedCodexRequestLog(ginCtx)
	return got
}

func recordedCodexRequestLog(ginCtx *gin.Context) string {
	var out strings.Builder
	for _, value := range ginCtx.Keys {
		rv := reflect.ValueOf(value)
		if rv.Kind() != reflect.Slice {
			continue
		}
		for i := 0; i < rv.Len(); i++ {
			elem := rv.Index(i)
			if elem.Kind() == reflect.Ptr {
				elem = elem.Elem()
			}
			if elem.Kind() != reflect.Struct {
				continue
			}
			if field := elem.FieldByName("request"); field.IsValid() && field.Kind() == reflect.String {
				out.WriteString(field.String())
			}
		}
	}
	return out.String()
}

func decodeCodexUpstreamTestBody(t *testing.T, encoding string, raw []byte) []byte {
	t.Helper()
	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("upstream body is not zstd: %v", err)
		}
		defer dec.Close()
		decoded, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		return decoded
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("upstream body is not gzip: %v", err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		return decoded
	case "br":
		decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		return decoded
	default:
		t.Fatalf("unknown encoding %q", encoding)
		return nil
	}
}

func TestCodexExecutorUpstreamRequestCompression(t *testing.T) {
	tests := []struct {
		name   string
		alt    string
		stream bool
		path   string
	}{
		{name: "responses non-streaming", path: "/backend-api/codex/responses"},
		{name: "responses streaming", stream: true, path: "/backend-api/codex/responses"},
		{name: "responses compact", alt: "responses/compact", path: "/backend-api/codex/responses/compact"},
	}
	for _, encoding := range []string{"zstd", "gzip", "br"} {
		for _, tc := range tests {
			t.Run(encoding+"/"+tc.name, func(t *testing.T) {
				filler := strings.Repeat(codexCompressionTestFiller, 200)
				baseline := runCodexCompressionRequest(t, "off", tc.alt, tc.stream, nil, filler)
				if baseline.path != tc.path {
					t.Fatalf("path = %q, want %q", baseline.path, tc.path)
				}
				if baseline.encoding != "" {
					t.Fatalf("Content-Encoding = %q, want none when disabled", baseline.encoding)
				}
				if len(baseline.body) < 1024 {
					t.Fatalf("baseline body too small to exercise compression: %d bytes", len(baseline.body))
				}

				got := runCodexCompressionRequest(t, encoding, tc.alt, tc.stream, nil, filler)
				if got.path != tc.path {
					t.Fatalf("path = %q, want %q", got.path, tc.path)
				}
				if got.encoding != encoding {
					t.Fatalf("Content-Encoding = %q, want %s", got.encoding, encoding)
				}
				if got.contentLength != int64(len(got.body)) {
					t.Fatalf("ContentLength = %d, wire body = %d", got.contentLength, len(got.body))
				}
				if len(got.body) >= len(baseline.body) {
					t.Fatalf("compressed upstream body %d not smaller than identity body %d", len(got.body), len(baseline.body))
				}
				if decoded := decodeCodexUpstreamTestBody(t, encoding, got.body); !bytes.Equal(decoded, baseline.body) {
					t.Fatalf("decoded upstream body differs from identity body")
				}
				if tc.stream && !bytes.Contains(got.streamOutput, []byte(`"type":"response.completed"`)) {
					t.Fatalf("streaming run produced no terminal event; output=%s", got.streamOutput)
				}
				if got.requestLog == "" || !strings.Contains(got.requestLog, filler) {
					t.Fatal("request log must contain the plain request body")
				}
			})
		}
	}
}
