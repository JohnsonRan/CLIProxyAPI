package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseCodexClientUsage(t *testing.T) {
	observedAt := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	body := []byte(`{
		"plan_type": "plus",
		"rate_limit": {
			"primary_window": {"used_percent": 42.5, "limit_window_seconds": 18000, "reset_after_seconds": 600},
			"secondary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_at": 1792108800}
		},
		"code_review_rate_limit": {"primary_window": {"used_percent": 150, "limit_window_seconds": 604800}},
		"additional_rate_limits": [
			{"limit_name": "GPT-5.3-Codex-Spark", "rate_limit": {"primary_window": {"used_percent": 5, "limit_window_seconds": 18000}}}
		]
	}`)
	plan, windows := parseCodexClientUsage(body, observedAt)
	if plan != "plus" {
		t.Fatalf("plan = %q, want plus", plan)
	}
	if len(windows) != 4 {
		t.Fatalf("windows = %d, want 4: %+v", len(windows), windows)
	}
	if windows[0].Window != "5h" || windows[0].Scope != "" || windows[0].Used != 42.5 || windows[0].Remaining != 57.5 {
		t.Fatalf("primary window = %+v", windows[0])
	}
	if windows[0].ResetAt == nil || !windows[0].ResetAt.Equal(observedAt.Add(10*time.Minute)) {
		t.Fatalf("primary reset = %v", windows[0].ResetAt)
	}
	if windows[1].Window != "7d" || windows[1].ResetAt == nil || windows[1].ResetAt.Unix() != 1792108800 {
		t.Fatalf("secondary window = %+v", windows[1])
	}
	if windows[2].Scope != "code-review" || windows[2].Used != 100 || windows[2].Remaining != 0 {
		t.Fatalf("code review window must clamp to 100: %+v", windows[2])
	}
	if windows[3].Scope != "GPT-5.3-Codex-Spark" || windows[3].Window != "5h" {
		t.Fatalf("additional window = %+v", windows[3])
	}
}

func TestParseClaudeClientUsage(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 30, "resets_at": "2026-10-16T05:00:00Z"},
		"seven_day": {"utilization": 12.5},
		"seven_day_opus": {"utilization": 80},
		"seven_day_sonnet": null,
		"extra_usage": {"is_enabled": true}
	}`)
	windows := parseClaudeClientUsage(body)
	if len(windows) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(windows), windows)
	}
	if windows[0].Window != "5h" || windows[0].Remaining != 70 || windows[0].ResetAt == nil {
		t.Fatalf("five hour = %+v", windows[0])
	}
	if windows[1].Window != "7d" || windows[1].Scope != "" || windows[1].Remaining != 87.5 {
		t.Fatalf("seven day = %+v", windows[1])
	}
	if windows[2].Scope != "opus" || windows[2].Remaining != 20 {
		t.Fatalf("opus = %+v", windows[2])
	}
}

func TestParseAntigravityClientUsage(t *testing.T) {
	body := []byte(`{"groups": [
		{"displayName": "Gemini", "buckets": [
			{"bucketId": "gemini-5h", "remainingFraction": 0.25, "resetTime": "2026-10-16T05:00:00Z"},
			{"bucketId": "gemini-weekly", "remainingFraction": "90%"},
			{"bucketId": "gemini-5h", "remainingFraction": 0.9}
		]},
		{"displayName": "Third party", "buckets": [
			{"bucketId": "3p-5h", "remainingFraction": "0.5"},
			{"bucketId": "3p-weekly", "remainingFraction": 1, "disabled": true},
			{"bucketId": "unknown", "remainingFraction": 1}
		]}
	]}`)
	windows := parseAntigravityClientUsage(body)
	if len(windows) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(windows), windows)
	}
	if windows[0].Scope != "gemini" || windows[0].Window != "5h" || windows[0].Remaining != 25 || windows[0].ResetAt == nil {
		t.Fatalf("gemini 5h = %+v", windows[0])
	}
	if windows[1].Window != "7d" || windows[1].Remaining != 90 {
		t.Fatalf("gemini weekly = %+v", windows[1])
	}
	if windows[2].Scope != "third-party" || windows[2].Remaining != 50 {
		t.Fatalf("third-party 5h = %+v", windows[2])
	}
}

func TestParseXAIClientUsage(t *testing.T) {
	weekly := []byte(`{"config": {"creditUsagePercent": 35, "currentPeriod": {"type": "weekly", "start": "2026-10-12T00:00:00Z", "end": "2026-10-19T00:00:00Z"}}}`)
	window, ok := parseXAIWeeklyClientUsage(weekly)
	if !ok || window.Window != "7d" || window.Remaining != 65 || window.ResetAt == nil {
		t.Fatalf("weekly = %+v ok=%v", window, ok)
	}
	if _, ok = parseXAIWeeklyClientUsage([]byte(`{"config": {"monthlyLimit": 15000}}`)); ok {
		t.Fatal("monthly payload must not parse as weekly")
	}

	monthly := []byte(`{"config": {"monthlyLimit": {"val": 15000}, "used": "3000", "billingPeriodEnd": "2026-11-01T00:00:00Z"}}`)
	plan, window, ok := parseXAIMonthlyClientUsage(monthly)
	if !ok || plan != "SuperGrok" || window.Window != "30d" || window.Used != 20 || window.ResetAt == nil {
		t.Fatalf("monthly = %+v plan=%q ok=%v", window, plan, ok)
	}
}

func TestAggregateClientUsage(t *testing.T) {
	early := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	credentials := []clientUsageCredential{
		{Name: "codex#1", Status: clientUsageCredentialOK, Windows: []clientUsageWindow{
			newClientUsageWindow(5*clientUsageHour, "", 40, &late),
			newClientUsageWindow(5*clientUsageHour, "code-review", 100, nil),
			newClientUsageWindow(clientUsageWeek, "", 10, nil),
		}},
		{Name: "antigravity#1", Status: clientUsageCredentialOK, Windows: []clientUsageWindow{
			newClientUsageWindow(5*clientUsageHour, "gemini", 20, nil),
			newClientUsageWindow(5*clientUsageHour, "third-party", 60, &early),
		}},
		{Name: "claude#1", Status: clientUsageCredentialFailure, Windows: []clientUsageWindow{
			newClientUsageWindow(5*clientUsageHour, "", 100, nil),
		}},
	}
	resp := buildClientUsageResponse(credentials)
	if len(resp.RateLimits) != 2 {
		t.Fatalf("rate limits = %+v", resp.RateLimits)
	}
	fiveHour := resp.RateLimits[0]
	// codex#1 uses its account window (60% left); antigravity#1 uses its tighter scope (40% left).
	if fiveHour.Window != "5h" || fiveHour.Remaining != 50 || fiveHour.Used != 50 || fiveHour.Credentials != 2 {
		t.Fatalf("5h aggregate = %+v", fiveHour)
	}
	if fiveHour.ResetAt == nil || !fiveHour.ResetAt.Equal(early) {
		t.Fatalf("5h reset = %v, want earliest %v", fiveHour.ResetAt, early)
	}
	if resp.RateLimits[1].Window != "7d" || resp.RateLimits[1].Remaining != 90 || resp.RateLimits[1].Credentials != 1 {
		t.Fatalf("7d aggregate = %+v", resp.RateLimits[1])
	}
	if resp.Remaining == nil || *resp.Remaining != 50 {
		t.Fatalf("top remaining = %v, want 50", resp.Remaining)
	}
	if resp.Mode != "quota_limited" || !resp.IsValid || resp.Unit != "%" {
		t.Fatalf("envelope = %+v", resp)
	}
}

func TestBuildClientUsageResponseEmptyPool(t *testing.T) {
	raw, err := json.Marshal(buildClientUsageResponse(nil))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["remaining"]; ok {
		t.Fatalf("empty pool must omit remaining: %s", raw)
	}
	if creds, ok := payload["credentials"].([]any); !ok || len(creds) != 0 {
		t.Fatalf("credentials must be an empty array: %s", raw)
	}
}

func TestClientUsageCacheReusesAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	cache := clientUsageCache{now: func() time.Time { return now }}
	var calls int
	probe := func() clientUsageProbeResult {
		calls++
		return clientUsageProbeResult{plan: "plus"}
	}
	cache.get("a", time.Minute, probe)
	now = now.Add(59 * time.Second)
	cache.get("a", time.Minute, probe)
	if calls != 1 {
		t.Fatalf("calls within TTL = %d, want 1", calls)
	}
	now = now.Add(time.Second)
	cache.get("a", time.Minute, probe)
	if calls != 2 {
		t.Fatalf("calls after TTL = %d, want 2", calls)
	}
	cache.prune(map[string]struct{}{})
	if len(cache.entries) != 0 {
		t.Fatalf("prune kept %d entries", len(cache.entries))
	}
}

func TestClientUsageCacheCollapsesConcurrentProbes(t *testing.T) {
	var cache clientUsageCache
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	probe := func() clientUsageProbeResult {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return clientUsageProbeResult{plan: "pro"}
	}
	var wg sync.WaitGroup
	results := make([]clientUsageProbeResult, 8)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = cache.get("a", time.Minute, probe)
	}()
	<-entered
	for i := 1; i < len(results); i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index] = cache.get("a", time.Minute, probe)
		}(i)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("probe calls = %d, want 1", calls.Load())
	}
	for i, result := range results {
		if result.plan != "pro" {
			t.Fatalf("result %d = %+v", i, result)
		}
	}
}

func TestClientUsageAuthsFiltersAndOrders(t *testing.T) {
	oauth := map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}
	list := []*coreauth.Auth{
		{ID: "b", Provider: "codex", Attributes: oauth},
		{ID: "z", Provider: "claude", Attributes: oauth},
		{ID: "a", Provider: "codex", Attributes: oauth},
		{ID: "key", Provider: "codex", Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-test"}},
		{ID: "off", Provider: "claude", Attributes: oauth, Disabled: true},
		{ID: "gem", Provider: "gemini", Attributes: oauth},
	}
	auths := clientUsageAuths(list)
	credentials := clientUsageCredentials(auths, make([]clientUsageProbeResult, len(auths)))
	got := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		got = append(got, credential.Name)
	}
	want := []string{"claude#1", "codex#1", "codex#2"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}
	if auths[1].ID != "a" {
		t.Fatalf("codex#1 must be the lowest ID, got %q", auths[1].ID)
	}
}

func TestGetClientUsageDisabledReturnsNotFound(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.GetClientUsage(ginCtx)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetClientUsageServesCachedCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	oauth := map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}
	for _, auth := range []*coreauth.Auth{
		{ID: "codex-a", Provider: "codex", Attributes: oauth, Metadata: map[string]any{"email": "secret@example.com"}},
		{ID: "claude-a", Provider: "claude", Attributes: oauth},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.QuotaAPI.Enabled = true
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	observedAt := time.Now().UTC()
	h.clientUsage.entries = map[string]clientUsageCacheEntry{
		"codex-a": {expiresAt: time.Now().Add(time.Hour), result: clientUsageProbeResult{
			plan: "plus", observedAt: observedAt,
			windows: []clientUsageWindow{newClientUsageWindow(5*clientUsageHour, "", 30, nil)},
		}},
		"claude-a": {expiresAt: time.Now().Add(time.Hour), result: clientUsageProbeResult{
			err: "upstream quota endpoint returned HTTP 401", observedAt: observedAt,
		}},
		"deleted": {expiresAt: time.Now().Add(time.Hour)},
	}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	h.GetClientUsage(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); containsAny(body, "secret@example.com", "codex-a", "claude-a") {
		t.Fatalf("response leaks credential identity: %s", body)
	}
	var resp clientUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Credentials) != 2 || resp.Credentials[0].Name != "claude#1" || resp.Credentials[0].Status != "error" {
		t.Fatalf("credentials = %+v", resp.Credentials)
	}
	if resp.Credentials[1].Name != "codex#1" || resp.Credentials[1].Plan != "plus" || len(resp.Credentials[1].Windows) != 1 {
		t.Fatalf("codex credential = %+v", resp.Credentials[1])
	}
	if resp.Remaining == nil || *resp.Remaining != 70 || len(resp.RateLimits) != 1 {
		t.Fatalf("aggregate = %+v remaining=%v", resp.RateLimits, resp.Remaining)
	}
	if _, ok := h.clientUsage.entries["deleted"]; ok {
		t.Fatal("cache kept a removed credential")
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && len(value) >= len(needle) {
			for i := 0; i+len(needle) <= len(value); i++ {
				if value[i:i+len(needle)] == needle {
					return true
				}
			}
		}
	}
	return false
}
