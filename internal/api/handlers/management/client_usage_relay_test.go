package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// serveClientUsageHTTP exposes h.GetClientUsage the way the /v1 group does:
// the client key comes from the Bearer token.
func serveClientUsageHTTP(h *Handler) *httptest.Server {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/usage", func(c *gin.Context) {
		c.Set("userApiKey", strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		h.GetClientUsage(c)
	})
	return httptest.NewServer(router)
}

func relayTestAuth(id, baseURL, key string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "openai-compatibility",
		Label:    "friend",
		Attributes: map[string]string{
			coreauth.AttributeAPIKey: key,
			"base_url":               baseURL,
		},
	}
}

func newRelayTestHandler(t *testing.T, auths []*coreauth.Auth, keys []config.QuotaAPIKey, hideNames bool) *Handler {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.QuotaAPI.Keys = keys
	cfg.QuotaAPI.HideNames = hideNames
	return NewHandlerWithoutConfigFilePath(cfg, manager)
}

func TestClientUsageRelayURL(t *testing.T) {
	cases := map[string]string{
		"https://up.example/v1":       "https://up.example/v1/usage",
		"https://up.example/v1/":      "https://up.example/v1/usage",
		"https://up.example":          "https://up.example/v1/usage",
		"https://up.example/proxy/V1": "https://up.example/proxy/v1/usage",
		"http://10.0.0.2:8317/?x=1":   "http://10.0.0.2:8317/v1/usage",
	}
	for base, want := range cases {
		if got, err := clientUsageRelayURL(base); err != nil || got != want {
			t.Fatalf("relay URL(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "not a url"} {
		if _, err := clientUsageRelayURL(bad); err == nil {
			t.Fatalf("relay URL(%q) accepted", bad)
		}
	}
}

func TestClientUsageRequestDepth(t *testing.T) {
	cases := []struct {
		header string
		want   int
	}{
		{"", 0},
		{"garbage", 0},
		{"-1", 0},
		{"-99", 0},
		{"0", 0},
		{"1", 1},
		{"2", 2},
		{"3", 3},
		{"4", clientUsageMaxDepth},
		{"100", clientUsageMaxDepth},
		{"99999999", clientUsageMaxDepth},
	}
	for _, tc := range cases {
		if got := clientUsageRequestDepth(tc.header); got != tc.want {
			t.Fatalf("clientUsageRequestDepth(%q) = %d, want %d", tc.header, got, tc.want)
		}
	}
}

// A downstream proxy relays the quota its upstream exposes to the key it was
// given, selected by auth index, and passes the depth along.
func TestGetClientUsageRelaysUpstreamProxy(t *testing.T) {
	upstream := newClientUsageTestHandler(t, []config.QuotaAPIKey{{APIKey: "sk-friend", Credentials: []string{"codex-a.json"}}}, func(c *config.QuotaAPIConfig) {
		c.IncludeRaw = true
	})
	var sawDepth string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDepth = r.Header.Get(clientUsageDepthHeader)
		serveClientUsageHTTP(upstream).Config.Handler.ServeHTTP(w, r)
	}))
	defer upstreamServer.Close()

	relay := relayTestAuth("relay-1", upstreamServer.URL+"/v1", "sk-friend")
	index := relay.EnsureIndex()
	downstream := newRelayTestHandler(t, []*coreauth.Auth{relay}, []config.QuotaAPIKey{{APIKey: "sk-me", Credentials: []string{index}}}, false)

	resp := serveClientUsage(t, downstream, "sk-me")
	if resp.Version != clientUsageVersion || len(resp.Credentials) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	node := resp.Credentials[0]
	if node.Provider != clientUsageRelayProvider || node.Name != "friend" || node.Status != "ok" || len(node.Upstream) != 1 {
		t.Fatalf("relay credential = %+v", node)
	}
	account := node.Upstream[0]
	if account.Name != "work" || account.Provider != "codex" || account.Plan != "plus" || len(account.Windows) != 1 || *account.Windows[0].UsedPercent != 40 {
		t.Fatalf("relayed account = %+v", account)
	}
	// The upstream's raw responses stay with the upstream.
	if account.Responses != nil {
		t.Fatalf("relayed raw responses = %+v", account.Responses)
	}
	if sawDepth != "1" {
		t.Fatalf("upstream saw depth %q, want 1", sawDepth)
	}
}

func TestGetClientUsageRelayHidesNamesAndReportsMissingEndpoint(t *testing.T) {
	upstream := newClientUsageTestHandler(t, []config.QuotaAPIKey{{APIKey: "sk-friend", Credentials: []string{"codex-a.json"}}})
	upstreamServer := serveClientUsageHTTP(upstream)
	defer upstreamServer.Close()
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()

	downstream := newRelayTestHandler(t, []*coreauth.Auth{
		relayTestAuth("relay-1", upstreamServer.URL, "sk-friend"),
		relayTestAuth("relay-2", plain.URL+"/v1", "sk-other"),
	}, []config.QuotaAPIKey{{APIKey: "sk-me", Credentials: []string{"relay-1", "relay-2"}}}, true)

	resp := serveClientUsage(t, downstream, "sk-me")
	if len(resp.Credentials) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	byID := map[string]clientUsageCredential{}
	for i, credential := range resp.Credentials {
		byID[[]string{"relay-1", "relay-2"}[i]] = credential
	}
	named := byID["relay-1"]
	if named.Name != clientUsageRelayProvider || len(named.Upstream) != 1 || named.Upstream[0].Name != "codex" {
		t.Fatalf("hidden names leaked: %+v", named)
	}
	missing := byID["relay-2"]
	if missing.Status != "error" || missing.Error != "upstream quota endpoint returned 404" || missing.Upstream != nil {
		t.Fatalf("plain upstream = %+v", missing)
	}
}

func TestGetClientUsageRelayRejectsMissingOrOldVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/no-version/v1/usage":
			_, _ = w.Write([]byte(`{"credentials":[]}`))
		case "/old-version/v1/usage":
			_, _ = w.Write([]byte(`{"version":1,"credentials":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	downstream := newRelayTestHandler(t, []*coreauth.Auth{
		relayTestAuth("no-version", server.URL+"/no-version/v1", "sk-test"),
		relayTestAuth("old-version", server.URL+"/old-version/v1", "sk-test"),
	}, []config.QuotaAPIKey{{APIKey: "sk-me", Credentials: []string{"no-version", "old-version"}}}, false)

	resp := serveClientUsage(t, downstream, "sk-me")
	if len(resp.Credentials) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	for _, cred := range resp.Credentials {
		if cred.Status != "error" || cred.Error != "upstream does not report quota" {
			t.Fatalf("credential %s = %+v, want error 'upstream does not report quota'", cred.Name, cred)
		}
	}
}

// Two proxies relaying each other stop at the depth limit instead of
// recursing or waiting on their own in-flight probe.
func TestGetClientUsageRelayLoopTerminates(t *testing.T) {
	var a, b *Handler
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveClientUsageHTTP(a).Config.Handler.ServeHTTP(w, r)
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveClientUsageHTTP(b).Config.Handler.ServeHTTP(w, r)
	}))
	defer serverB.Close()
	a = newRelayTestHandler(t, []*coreauth.Auth{relayTestAuth("to-b", serverB.URL, "sk-b")}, []config.QuotaAPIKey{{APIKey: "sk-a", Credentials: []string{"to-b"}}}, false)
	b = newRelayTestHandler(t, []*coreauth.Auth{relayTestAuth("to-a", serverA.URL, "sk-a")}, []config.QuotaAPIKey{{APIKey: "sk-b", Credentials: []string{"to-a"}}}, false)

	done := make(chan clientUsageResponse, 1)
	go func() { done <- serveClientUsage(t, a, "sk-a") }()
	select {
	case resp := <-done:
		node := resp.Credentials[0]
		for depth := 0; depth < clientUsageMaxDepth; depth++ {
			if len(node.Upstream) != 1 {
				t.Fatalf("depth %d node = %+v", depth, node)
			}
			node = node.Upstream[0]
		}
		if node.Status != "error" || node.Error != "relay depth limit reached" {
			t.Fatalf("innermost node = %+v", node)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay loop did not terminate")
	}
}
