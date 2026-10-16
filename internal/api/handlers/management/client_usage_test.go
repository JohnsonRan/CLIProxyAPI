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

func TestRedactClientUsageBody(t *testing.T) {
	raw := []byte(`{"email":"a@b.c","user_id":"u","plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12.50,"reset_at":1760000000}},"accounts":[{"accountId":"x","userId":"y","name":"<team>"}],"credits":{"balance":"0"}}`)
	got, err := redactClientUsageBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"accounts":[{"name":"<team>"}],"credits":{"balance":"0"},"plan_type":"plus","rate_limit":{"primary_window":{"reset_at":1760000000,"used_percent":12.50}}}`
	if string(got) != want {
		t.Fatalf("redacted = %s\nwant      %s", got, want)
	}
	for _, bad := range [][]byte{[]byte("<html>"), []byte(`{"a":1} {"b":2}`), nil} {
		if _, err := redactClientUsageBody(bad); err == nil {
			t.Fatalf("redact(%q) accepted non-JSON", bad)
		}
	}
}

func TestClientUsageRequestPassesThroughAnyStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","account_id":"acc"}}`))
			return
		}
		if r.URL.Path == "/html" {
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		_, _ = w.Write([]byte(`{"email":"a@b.c","plan_type":"pro"}`))
	}))
	defer server.Close()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	auth := &coreauth.Auth{ID: "a", Provider: "codex"}

	ok := h.clientUsageRequest(context.Background(), auth, "tok", http.MethodGet, server.URL+"/ok", nil, nil)
	if !clientUsageUpstreamOK(ok) || string(ok.Body) != `{"plan_type":"pro"}` || ok.Endpoint != server.URL+"/ok" {
		t.Fatalf("ok response = %+v body=%s", ok, ok.Body)
	}
	failed := h.clientUsageRequest(context.Background(), auth, "tok", http.MethodGet, server.URL+"/fail", nil, nil)
	if clientUsageUpstreamOK(failed) || failed.StatusCode != http.StatusTooManyRequests || string(failed.Body) != `{"error":{"message":"slow down"}}` {
		t.Fatalf("failed response = %+v body=%s", failed, failed.Body)
	}
	html := h.clientUsageRequest(context.Background(), auth, "tok", http.MethodGet, server.URL+"/html", nil, nil)
	if clientUsageUpstreamOK(html) || html.StatusCode != http.StatusOK || html.Body != nil || html.Error == "" {
		t.Fatalf("html response = %+v", html)
	}
}

func TestClientUsageName(t *testing.T) {
	cases := []struct {
		auth *coreauth.Auth
		want string
	}{
		{&coreauth.Auth{Provider: "codex", Attributes: map[string]string{"note": " work "}}, "work"},
		{&coreauth.Auth{Provider: "claude", Metadata: map[string]any{"note": "personal", "email": "a@b.c"}}, "personal"},
		{&coreauth.Auth{Provider: "Grok", Metadata: map[string]any{"email": "a@b.c"}}, "xai"},
	}
	for _, tc := range cases {
		if got := clientUsageName(tc.auth); got != tc.want {
			t.Fatalf("name(%+v) = %q, want %q", tc.auth, got, tc.want)
		}
	}
}

func TestClientUsageCacheReusesAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	cache := clientUsageCache{now: func() time.Time { return now }}
	var calls int
	probe := func() clientUsageProbeResult {
		calls++
		return clientUsageProbeResult{err: "plus"}
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
		return clientUsageProbeResult{err: "pro"}
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
		if result.err != "pro" {
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
	got := make([]string, 0, len(auths))
	for _, auth := range auths {
		got = append(got, auth.ID)
	}
	want := []string{"z", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
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
		{ID: "codex-a", Provider: "codex", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "note": "work"}, Metadata: map[string]any{"email": "secret@example.com"}},
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
			observedAt: observedAt,
			responses:  []clientUsageUpstream{{Endpoint: clientUsageCodexURL, StatusCode: 200, Body: json.RawMessage(`{"plan_type":"plus"}`)}},
		}},
		"claude-a": {expiresAt: time.Now().Add(time.Hour), result: clientUsageProbeResult{
			err: "auth token refresh failed", observedAt: observedAt,
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
	if body := rec.Body.String(); containsAny(body, "secret@example.com", "codex-a", "claude-a", "remaining", "mode") {
		t.Fatalf("unexpected content in response: %s", body)
	}
	var resp clientUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Credentials) != 2 {
		t.Fatalf("credentials = %+v", resp.Credentials)
	}
	claude, codex := resp.Credentials[0], resp.Credentials[1]
	if claude.Name != "claude" || claude.Status != "error" || claude.Error != "auth token refresh failed" || claude.Responses == nil {
		t.Fatalf("claude credential = %+v", claude)
	}
	if codex.Name != "work" || codex.Provider != "codex" || codex.Status != "ok" || len(codex.Responses) != 1 || string(codex.Responses[0].Body) != `{"plan_type":"plus"}` {
		t.Fatalf("codex credential = %+v", codex)
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
