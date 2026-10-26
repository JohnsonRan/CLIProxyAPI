package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	lmfpd "github.com/Ikaleio/lm-detector/sdk/go"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func swapFingerprintLoad(t *testing.T, load func(context.Context) (*lmfpd.Detector, error)) {
	t.Helper()
	fingerprintDetectorMu.Lock()
	prevLoad, prevDetector := fingerprintLoad, fingerprintDetector
	fingerprintLoad, fingerprintDetector = load, nil
	fingerprintDetectorMu.Unlock()
	t.Cleanup(func() {
		fingerprintDetectorMu.Lock()
		fingerprintLoad, fingerprintDetector = prevLoad, prevDetector
		fingerprintDetectorMu.Unlock()
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

// TestFingerprintDetectSendsChallengesThroughLoopback needs reference data: set LMFPD_TEST_DATA to a local
// lm-detector data directory, or to "online" to download the published data.
func TestFingerprintDetectSendsChallengesThroughLoopback(t *testing.T) {
	source := os.Getenv("LMFPD_TEST_DATA")
	if source == "" {
		t.Skip("LMFPD_TEST_DATA not set")
	}
	opts := lmfpd.LoadOptions{Cache: t.TempDir() + "/data.bin"}
	if source != "online" {
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
}
