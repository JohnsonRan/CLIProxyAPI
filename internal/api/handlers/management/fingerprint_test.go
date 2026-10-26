package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	lmfpd "github.com/Ikaleio/lm-detector/sdk/go"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func swapFingerprintLoad(t *testing.T, load func(context.Context) (*lmfpd.Detector, error)) {
	t.Helper()
	prevLoad := fingerprintLoad
	fingerprintLoad = load
	t.Cleanup(func() {
		fingerprintLoad = prevLoad
	})
}

func runFingerprintDetect(t *testing.T, h *Handler, api http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/observability/fingerprint/detect", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.FingerprintDetect(api)(c)
	return rec
}

func TestFingerprintDetectRejectsMissingModel(t *testing.T) {
	swapFingerprintLoad(t, func(context.Context) (*lmfpd.Detector, error) {
		t.Fatal("detector must not load for an invalid request")
		return nil, nil
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	rec := runFingerprintDetect(t, h, http.NotFoundHandler(), `{"model":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestFingerprintDetectReportsDataLoadFailureAndRetries(t *testing.T) {
	calls := 0
	swapFingerprintLoad(t, func(context.Context) (*lmfpd.Detector, error) {
		calls++
		return nil, errors.New("offline")
	})
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	for range 2 {
		rec := runFingerprintDetect(t, h, http.NotFoundHandler(), `{"model":"gpt-5"}`)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "fingerprint_data_unavailable") {
			t.Fatalf("status = %d body = %s, want 502 fingerprint_data_unavailable", rec.Code, rec.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("load calls = %d, want a retry after a failed load", calls)
	}
}

func TestFingerprintProgressStreamsDistinctSampleChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	p := &fingerprintProgress{w: c.Writer, last: map[int]string{}}
	if p.finish(gin.H{"type": "error"}) {
		t.Fatal("finish before any sample must leave the answer to plain JSON")
	}
	p.onSample(0, lmfpd.Sample{State: "queued", ExpectedCount: 40})
	p.onSample(0, lmfpd.Sample{State: "streaming", Count: 3, ExpectedCount: 40})
	p.onSample(0, lmfpd.Sample{State: "streaming", Count: 3, ExpectedCount: 40, RawText: "more text"})
	p.onSample(1, lmfpd.Sample{State: "queued", ExpectedCount: 40})
	p.onSample(0, lmfpd.Sample{State: "complete", Count: 40, ExpectedCount: 40})
	if !p.finish(gin.H{"type": "result", "result": gin.H{"built_at": "x"}}) {
		t.Fatal("finish after samples must write the result line")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Fatalf("content type = %q", ct)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var event struct {
			Type   string `json:"type"`
			Index  int    `json:"index"`
			Sample struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"sample"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if event.Type == "sample" {
			got = append(got, strconv.Itoa(event.Index)+":"+event.Sample.State+"/"+strconv.Itoa(event.Sample.Count))
		} else {
			got = append(got, event.Type)
		}
	}
	want := "0:queued/0 0:streaming/3 1:queued/0 0:complete/40 result"
	if strings.Join(got, " ") != want {
		t.Fatalf("events = %v, want %s", got, want)
	}
}

type mockFailingResponseWriter struct {
	header     http.Header
	writeCalls int
	flushCalls int
	writeErr   error
}

func (m *mockFailingResponseWriter) Header() http.Header {
	if m.header == nil {
		m.header = make(http.Header)
	}
	return m.header
}

func (m *mockFailingResponseWriter) WriteHeader(statusCode int) {}

func (m *mockFailingResponseWriter) Write(p []byte) (int, error) {
	m.writeCalls++
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return len(p), nil
}

func (m *mockFailingResponseWriter) Flush() {
	m.flushCalls++
}

func TestLoopbackHandlerDetachesFromClientContext(t *testing.T) {
	// Case 1: incoming request context was already cancelled before ServeHTTP.
	clientCtx1, clientCancel1 := context.WithCancel(context.Background())
	clientCancel1()

	parentCtx := context.Background()
	var sawErrBefore error
	innerBefore := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawErrBefore = r.Context().Err()
	})

	lhBefore := &loopbackHandler{inner: innerBefore, parentCtx: parentCtx}
	recBefore := httptest.NewRecorder()
	reqBefore := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1", nil).WithContext(clientCtx1)
	lhBefore.ServeHTTP(recBefore, reqBefore)

	if sawErrBefore != nil {
		t.Fatalf("inner handler saw cancelled context for pre-cancelled client context: %v", sawErrBefore)
	}

	// Case 2: incoming request context cancelled while inner handler is running.
	clientCtx2, clientCancel2 := context.WithCancel(context.Background())
	handlerEntered := make(chan struct{})
	clientCancelled := make(chan struct{})
	var sawErrDuring error

	innerDuring := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-clientCancelled
		sawErrDuring = r.Context().Err()
	})

	lhDuring := &loopbackHandler{inner: innerDuring, parentCtx: parentCtx}
	recDuring := httptest.NewRecorder()
	reqDuring := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1", nil).WithContext(clientCtx2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		lhDuring.ServeHTTP(recDuring, reqDuring)
	}()

	<-handlerEntered
	clientCancel2()
	close(clientCancelled)
	<-done

	if sawErrDuring != nil {
		t.Fatalf("inner handler saw cancelled context after client disconnected mid-request: %v", sawErrDuring)
	}
}

func TestLoopbackHandlerCancelsWhenParentContextCancels(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	handlerEntered := make(chan struct{})
	var sawDone bool

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-r.Context().Done()
		sawDone = true
	})

	lh := &loopbackHandler{inner: inner, parentCtx: parentCtx}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		lh.ServeHTTP(rec, req)
	}()

	<-handlerEntered
	parentCancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("inner handler was not cancelled when parent context was cancelled")
	}

	if !sawDone {
		t.Fatal("inner handler context was not cancelled when parent context was cancelled")
	}
}

func TestLoopbackHandlerDrainsOnWriteError(t *testing.T) {
	mock := &mockFailingResponseWriter{writeErr: io.ErrClosedPipe}
	var write1Err, write2Err error
	var write1N, write2N int

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter must implement http.Flusher")
		}

		write1N, write1Err = w.Write([]byte("chunk 1"))
		if flusher != nil {
			flusher.Flush()
		}

		write2N, write2Err = w.Write([]byte("chunk 2 terminal usage"))
		if flusher != nil {
			flusher.Flush()
		}
	})

	lh := &loopbackHandler{inner: inner, parentCtx: context.Background()}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1", nil)
	lh.ServeHTTP(mock, req)

	if write1Err != nil || write1N != len("chunk 1") {
		t.Fatalf("first write returned n=%d, err=%v; want %d, nil", write1N, write1Err, len("chunk 1"))
	}
	if write2Err != nil || write2N != len("chunk 2 terminal usage") {
		t.Fatalf("second write returned n=%d, err=%v; want %d, nil", write2N, write2Err, len("chunk 2 terminal usage"))
	}
	if mock.writeCalls != 1 {
		t.Fatalf("underlying write calls = %d, want 1", mock.writeCalls)
	}
	if mock.flushCalls != 0 {
		t.Fatalf("underlying flush calls = %d, want 0", mock.flushCalls)
	}
}

func TestLoopbackServerCloseWaitsForInFlightHandler(t *testing.T) {
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerExited := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		close(handlerExited)
	})

	lbServer, err := startLoopbackServer(context.Background(), handler)
	if err != nil {
		t.Fatalf("startLoopbackServer failed: %v", err)
	}

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req, _ := http.NewRequest(http.MethodGet, lbServer.BaseURL(), nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-handlerEntered

	closeDone := make(chan struct{})
	go func() {
		lbServer.Close()
		close(closeDone)
	}()

	// Confirm that s.listener.Close() has executed by checking that new connection attempts fail.
	for {
		conn, dialErr := net.DialTimeout("tcp", lbServer.listener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			break
		}
		_ = conn.Close()
	}

	// Close() must still be blocked in inFlight.Wait() because handler has not exited.
	select {
	case <-closeDone:
		t.Fatal("lbServer.Close returned while handler was still blocked in-flight")
	default:
	}

	close(releaseHandler)
	<-handlerExited
	<-closeDone
	<-reqDone
}

// TestFingerprintDetectSendsChallengesThroughLoopback needs reference data: set LMFPD_TEST_DATA to a local
// lm-detector data directory, to "online" to download the published data, or to "cache" to reuse the SDK's
// default cache file.
func TestFingerprintDetectSendsChallengesThroughLoopback(t *testing.T) {
	source := os.Getenv("LMFPD_TEST_DATA")
	if source == "" {
		t.Skip("LMFPD_TEST_DATA not set")
	}
	opts := lmfpd.LoadOptions{Cache: t.TempDir() + "/data.bin"}
	switch source {
	case "online":
	case "cache":
		opts.Cache = ""
	default:
		opts.Sources = []string{source}
	}
	swapFingerprintLoad(t, func(ctx context.Context) (*lmfpd.Detector, error) { return lmfpd.Load(ctx, opts) })

	var mu sync.Mutex
	var seen []string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")+" "+body.Model)
		mu.Unlock()
		numbers := make([]string, 300)
		for i := range numbers {
			numbers[i] = []string{"17", "203", "88", "341", "5", "129"}[i%6]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   body.Model,
			"choices": []any{map[string]any{"message": map[string]any{"content": strings.Join(numbers, ", ")}, "finish_reason": "stop"}},
		})
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"", "client-key"}}}, nil)
	rec := runFingerprintDetect(t, h, api, `{"model":"gpt-5","api":"chat_completions","no_stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		BuiltAt string         `json:"built_at"`
		Samples []lmfpd.Sample `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.BuiltAt == "" || len(resp.Samples) != 3 {
		t.Fatalf("built_at = %q samples = %d, want data version and 3 samples", resp.BuiltAt, len(resp.Samples))
	}
	if len(seen) != 3 {
		t.Fatalf("loopback requests = %d, want 3", len(seen))
	}
	for _, s := range seen {
		if s != "/v1/chat/completions Bearer client-key gpt-5" {
			t.Fatalf("loopback request = %q", s)
		}
	}

	rec = runFingerprintDetect(t, h, api, `{"model":"gpt-5","api":"chat_completions","no_stream":true,"progress":true}`)
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if rec.Code != http.StatusOK || len(lines) < 4 {
		t.Fatalf("progress status = %d lines = %d: %s", rec.Code, len(lines), rec.Body.String())
	}
	var last struct {
		Type   string `json:"type"`
		Result struct {
			BuiltAt string         `json:"built_at"`
			Samples []lmfpd.Sample `json:"samples"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode last line: %v", err)
	}
	if last.Type != "result" || last.Result.BuiltAt == "" || len(last.Result.Samples) != 3 {
		t.Fatalf("last line = %s", lines[len(lines)-1])
	}
}
