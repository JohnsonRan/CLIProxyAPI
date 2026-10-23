package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
	for _, bad := range []string{"/stats?from=x", "/stats?from=1700086400&to=1700000000", "/stats?tz_offset=9999", "/events?failed=maybe"} {
		if w = do(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", bad, w.Code)
		}
	}
	if w = do("/events?failed=true&limit=5"); w.Code != http.StatusOK {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	if w = do("/pricing"); w.Code != http.StatusOK {
		t.Fatalf("pricing: %d %s", w.Code, w.Body.String())
	}
}
