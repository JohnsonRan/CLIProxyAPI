package management

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

func TestUsageStoreHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	r := gin.New()
	r.GET("/stats", h.GetUsageStoreStats)
	r.GET("/events", h.GetUsageStoreEvents)
	r.GET("/pricing", h.GetUsageStorePricing)
	r.GET("/options", h.GetUsageStoreOptions)
	r.GET("/realtime", h.GetUsageStoreRealtime)
	do := func(url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		return w
	}

	usagestore.Shutdown()
	if w := do("/stats"); w.Code != http.StatusConflict {
		t.Fatalf("disabled store: status %d, want 409", w.Code)
	}

	cfg := &config.Config{}
	cfg.UsageStore = config.UsageStoreConfig{Enable: true, Path: filepath.Join(t.TempDir(), "usage.db"),
		Pricing: config.UsagePricingConfig{DisableModelsDev: true}}
	usagestore.Apply(cfg)
	defer usagestore.Shutdown()

	w := do("/stats?from=1699999200&to=1700085600&tz_offset=480")
	if w.Code != http.StatusOK {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}
	var stats usagestore.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil || stats.Bucket != "hour" || len(stats.Series) != 24 {
		t.Fatalf("stats body: %v %s", err, w.Body.String())
	}
	for _, bad := range []string{
		"/stats?from=x",
		"/stats?from=1700086400&to=1700000000",
		"/stats?tz_offset=9999",
		"/events?failed=maybe",
		"/events?substituted=bogus",
		"/events?before=10&after=20",
		"/events?after=abc",
		"/events?before=abc",
		"/events?limit=abc",
		"/realtime?minutes=0",
		"/realtime?minutes=181",
		"/realtime?minutes=abc",
	} {
		if w = do(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", bad, w.Code)
		}
	}

	// Test >50 values rejection -> 400
	var manyModels []string
	for i := 0; i < 51; i++ {
		manyModels = append(manyModels, fmt.Sprintf("m%d", i))
	}
	if w = do("/stats?model=" + strings.Join(manyModels, ",")); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for >50 models, got %d", w.Code)
	}

	// Test comma and repeated param parsing
	if w = do("/stats?model=m1,m2&model=m3"); w.Code != http.StatusOK {
		t.Fatalf("comma/repeated model params: %d %s", w.Code, w.Body.String())
	}

	// Test /options
	if w = do("/options"); w.Code != http.StatusOK {
		t.Fatalf("options: %d %s", w.Code, w.Body.String())
	}
	var opts usagestore.Options
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("parse options: %v", err)
	}

	// Test /realtime
	if w = do("/realtime?minutes=10"); w.Code != http.StatusOK {
		t.Fatalf("realtime: %d %s", w.Code, w.Body.String())
	}
	var rt usagestore.Realtime
	if err := json.Unmarshal(w.Body.Bytes(), &rt); err != nil || len(rt.Series) != 10 {
		t.Fatalf("parse realtime: %v (series len %d)", err, len(rt.Series))
	}

	// Test /events after and last_id
	w = do("/events?after=100")
	if w.Code != http.StatusOK {
		t.Fatalf("events after: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Events     []usagestore.Event `json:"events"`
		NextBefore int64              `json:"next_before"`
		LastID     int64              `json:"last_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("parse events: %v", err)
	}
	if page.LastID != 100 {
		t.Fatalf("expected last_id 100 when empty, got %d", page.LastID)
	}
	if w = do("/events?failed=true&limit=5"); w.Code != http.StatusOK {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	if w = do("/pricing"); w.Code != http.StatusOK {
		t.Fatalf("pricing: %d %s", w.Code, w.Body.String())
	}
}

func TestUsageStoreExportAndAnalysis(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	r := gin.New()
	r.GET("/export", h.ExportUsageStoreEvents)
	r.GET("/analysis", h.GetUsageStoreAnalysis)
	cfg := &config.Config{}
	cfg.UsageStore = config.UsageStoreConfig{Enable: true, Path: filepath.Join(t.TempDir(), "usage.db"),
		Pricing: config.UsagePricingConfig{DisableModelsDev: true}}
	usagestore.Apply(cfg)
	usagestore.Current().Enqueue(usagestore.Event{
		Timestamp:     time.Now().UnixMilli(),
		Model:         "m",
		Alias:         "=formula",
		Error:         "a,\"b\"\nc",
		ResponseModel: "=danger",
		Substituted:   true,
	})
	usagestore.Shutdown()
	usagestore.Apply(cfg)
	defer usagestore.Shutdown()
	do := func(url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		return w
	}

	w := do("/export")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), ".csv") {
		t.Fatalf("csv export: %d %v", w.Code, w.Header())
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(records) != 2 || records[1][18] != "a,\"b\"\nc" || records[1][6] != "'=formula" ||
		records[0][32] != "response_model" || records[0][33] != "substituted" ||
		records[1][32] != "'=danger" || records[1][33] != "true" {
		t.Fatalf("csv must quote fields, sanitize formula triggers and include substitution cols: %v %q", err, records)
	}
	w = do("/export?format=json&model=m")
	var events []usagestore.Event
	if err = json.Unmarshal(w.Body.Bytes(), &events); err != nil || len(events) != 1 || events[0].Alias != "=formula" {
		t.Fatalf("json export: %v %s", err, w.Body.String())
	}
	if w = do("/export?format=xml"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad format: %d", w.Code)
	}
	if w = do("/analysis?tz_offset=480"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"heatmap":[`) {
		t.Fatalf("analysis: %d %s", w.Code, w.Body.String())
	}
}
