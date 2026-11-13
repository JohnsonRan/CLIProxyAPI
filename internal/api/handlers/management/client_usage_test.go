package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRedactClientUsageBody(t *testing.T) {
	raw := []byte(`{"email":"a@b.c","user_id":"u","plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12.50,"reset_at":1760000000}},"accounts":[{"accountId":"x","userId":"y","Email":"A@B.C","ACCOUNT_ID":"z","name":"<team>"}],"credits":{"balance":"0"}}`)
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

	ok := h.clientUsageRequest(context.Background(), auth, "tok", clientUsageCall{url: server.URL + "/ok"})
	if !clientUsageUpstreamOK(ok) || string(ok.Body) != `{"plan_type":"pro"}` || ok.Endpoint != server.URL+"/ok" {
		t.Fatalf("ok response = %+v body=%s", ok, ok.Body)
	}
	picked := h.clientUsageRequest(context.Background(), auth, "tok", clientUsageCall{url: server.URL + "/ok", fields: []string{"plan_type", "absent"}})
	if string(picked.Body) != `{"plan_type":"pro"}` {
		t.Fatalf("picked body = %s", picked.Body)
	}
	failed := h.clientUsageRequest(context.Background(), auth, "tok", clientUsageCall{url: server.URL + "/fail"})
	if clientUsageUpstreamOK(failed) || failed.StatusCode != http.StatusTooManyRequests || string(failed.Body) != `{"error":{"message":"slow down"}}` {
		t.Fatalf("failed response = %+v body=%s", failed, failed.Body)
	}
	html := h.clientUsageRequest(context.Background(), auth, "tok", clientUsageCall{url: server.URL + "/html"})
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
		{ID: "relay", Provider: "openai-compatibility", Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-up", "base_url": "https://up.example/v1"}},
	}
	auths := clientUsageAuths(list)
	got := make([]string, 0, len(auths))
	for _, auth := range auths {
		got = append(got, auth.ID)
	}
	want := []string{"z", "a", "b", "relay"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func serveClientUsage(t *testing.T, h *Handler, apiKey string) clientUsageResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	h.GetClientUsage(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "secret@example.com") {
		t.Fatalf("unexpected content in response: %s", body)
	}
	var resp clientUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func newClientUsageTestHandler(t *testing.T, keys []config.QuotaAPIKey, configure ...func(*config.QuotaAPIConfig)) *Handler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "codex-a.json", Provider: "codex", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, "note": "work"}, Metadata: map[string]any{"email": "secret@example.com"}},
		{ID: "claude-a.json", Provider: "claude", Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindOAuth}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.QuotaAPI.Keys = keys
	for _, apply := range configure {
		apply(&cfg.QuotaAPI)
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	observedAt := time.Now().UTC()
	h.clientUsage.entries = map[string]clientUsageCacheEntry{
		"codex-a.json": {expiresAt: time.Now().Add(time.Hour), result: clientUsageProbeResult{
			observedAt: observedAt,
			responses:  []clientUsageUpstream{{Endpoint: clientUsageCodexURL, StatusCode: 200, Body: json.RawMessage(`{"plan_type":"plus"}`)}},
			summary:    clientUsageSummary{Plan: "plus", Windows: []clientUsageWindow{{ID: "five_hour", UsedPercent: clientUsageFloat(40)}}},
		}},
		"claude-a.json": {expiresAt: time.Now().Add(time.Hour), result: clientUsageProbeResult{
			err: "auth token refresh failed", observedAt: observedAt,
		}},
		"deleted.json": {expiresAt: time.Now().Add(time.Hour)},
	}
	return h
}

// Keys see nothing unless credentials were selected for them.
func TestGetClientUsageDefaultInvisible(t *testing.T) {
	h := newClientUsageTestHandler(t, []config.QuotaAPIKey{{APIKey: "sk-other", Credentials: []string{"codex-a.json"}}})
	for _, key := range []string{"", "sk-unlisted"} {
		if resp := serveClientUsage(t, h, key); len(resp.Credentials) != 0 {
			t.Fatalf("key %q sees %+v", key, resp.Credentials)
		}
	}
}

func TestGetClientUsageReturnsSelectedCredentialsOnly(t *testing.T) {
	h := newClientUsageTestHandler(t, []config.QuotaAPIKey{
		{APIKey: "sk-a", Credentials: []string{"codex-a.json", "missing.json"}},
		{APIKey: "sk-b", Credentials: []string{"claude-a.json"}},
	})
	resp := serveClientUsage(t, h, "sk-a")
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials = %+v", resp.Credentials)
	}
	codex := resp.Credentials[0]
	if codex.Name != "work" || codex.Provider != "codex" || codex.Status != "ok" || codex.Plan != "plus" || len(codex.Windows) != 1 || *codex.Windows[0].UsedPercent != 40 || codex.Responses != nil {
		t.Fatalf("codex credential = %+v", codex)
	}
	resp = serveClientUsage(t, h, "sk-b")
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials = %+v", resp.Credentials)
	}
	claude := resp.Credentials[0]
	if claude.Name != "claude" || claude.Status != "error" || claude.Error != "auth token refresh failed" || claude.Windows == nil {
		t.Fatalf("claude credential = %+v", claude)
	}
	if _, ok := h.clientUsage.entries["deleted.json"]; ok {
		t.Fatal("cache kept a removed credential")
	}
	if _, ok := h.clientUsage.entries["codex-a.json"]; !ok {
		t.Fatal("serving one key evicted another key's cached credential")
	}
}

func TestClientUsageCachePruneDepthEntries(t *testing.T) {
	cache := clientUsageCache{
		entries: map[string]clientUsageCacheEntry{
			"keep.json":      {expiresAt: time.Now().Add(time.Hour)},
			"keep.json\x001": {expiresAt: time.Now().Add(time.Hour)},
			"keep.json\x002": {expiresAt: time.Now().Add(time.Hour)},
			"drop.json":      {expiresAt: time.Now().Add(time.Hour)},
			"drop.json\x001": {expiresAt: time.Now().Add(time.Hour)},
			"drop.json\x003": {expiresAt: time.Now().Add(time.Hour)},
		},
	}
	cache.prune(map[string]struct{}{"keep.json": {}})
	if len(cache.entries) != 3 {
		t.Fatalf("prune kept %d entries, want 3", len(cache.entries))
	}
	for _, key := range []string{"keep.json", "keep.json\x001", "keep.json\x002"} {
		if _, ok := cache.entries[key]; !ok {
			t.Fatalf("prune dropped %q", key)
		}
	}
	for _, key := range []string{"drop.json", "drop.json\x001", "drop.json\x003"} {
		if _, ok := cache.entries[key]; ok {
			t.Fatalf("prune kept %q", key)
		}
	}
}

func TestGetClientUsageIncludeRaw(t *testing.T) {
	h := newClientUsageTestHandler(t, []config.QuotaAPIKey{
		{APIKey: "sk-raw", Credentials: []string{"codex-a.json"}},
	}, func(c *config.QuotaAPIConfig) {
		c.IncludeRaw = true
	})
	resp := serveClientUsage(t, h, "sk-raw")
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials = %+v", resp.Credentials)
	}
	codex := resp.Credentials[0]
	if len(codex.Responses) != 1 {
		t.Fatalf("responses length = %d, want 1", len(codex.Responses))
	}
	if string(codex.Responses[0].Body) != `{"plan_type":"plus"}` {
		t.Fatalf("response body = %s, want %s", codex.Responses[0].Body, `{"plan_type":"plus"}`)
	}
	if codex.Responses[0].Endpoint != clientUsageCodexURL {
		t.Fatalf("response endpoint = %q, want %q", codex.Responses[0].Endpoint, clientUsageCodexURL)
	}
}
