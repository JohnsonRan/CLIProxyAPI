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
	var sawDepth, sawVia string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDepth = r.Header.Get(clientUsageDepthHeader)
		sawVia = r.Header.Get(clientUsageViaHeader)
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
	if sawVia == "" || sawVia != downstream.clientUsageInstanceID() {
		t.Fatalf("upstream saw via %q, want the downstream instance ID", sawVia)
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

func TestClientUsageRequestTrace(t *testing.T) {
	cases := []struct {
		name, depth, via string
		wantDepth        int
		wantVia          []string
		wantLoop         bool
	}{
		{name: "direct", wantVia: nil},
		{name: "relayed", depth: "2", via: " a , ,b ", wantDepth: 2, wantVia: []string{"a", "b"}},
		{name: "own id", depth: "1", via: "a,self", wantDepth: 1, wantLoop: true},
		{name: "too many hops", via: "a,b,c,d", wantLoop: true},
		{name: "oversized", via: strings.Repeat("x", clientUsageMaxViaBytes+1), wantLoop: true},
	}
	for _, tc := range cases {
		trace, loop := clientUsageRequestTrace(tc.depth, tc.via, "self")
		if loop != tc.wantLoop {
			t.Fatalf("%s: loop = %v, want %v", tc.name, loop, tc.wantLoop)
		}
		if loop {
			continue
		}
		if trace.depth != tc.wantDepth || strings.Join(trace.via, ",") != strings.Join(tc.wantVia, ",") {
			t.Fatalf("%s: trace = %+v", tc.name, trace)
		}
	}
}

// loopTestServer serves h's GET /v1/usage once h is set, optionally without
// the Via header, the way a proxy that predates it forwards a request.
func loopTestServer(h **Handler, stripVia bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stripVia {
			r.Header.Del(clientUsageViaHeader)
		}
		serveClientUsageHTTP(*h).Config.Handler.ServeHTTP(w, r)
	}))
}

// A relay whose base URL and key lead back to the same proxy stops at the
// first hop with a loop error, instead of nesting itself to the depth limit.
func TestGetClientUsageRelayToSelfStopsAtFirstHop(t *testing.T) {
	var self *Handler
	server := loopTestServer(&self, false)
	defer server.Close()
	self = newRelayTestHandler(t, []*coreauth.Auth{relayTestAuth("to-self", server.URL+"/v1", "sk-me")}, []config.QuotaAPIKey{{APIKey: "sk-me", Credentials: []string{"to-self"}}}, false)

	resp := serveClientUsage(t, self, "sk-me")
	if len(resp.Credentials) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	node := resp.Credentials[0]
	if node.Status != "error" || node.Error != clientUsageLoopError || len(node.Upstream) != 0 {
		t.Fatalf("relay credential = %+v", node)
	}
}

// Two proxies relaying each other stop as soon as the request reaches the
// proxy it started from.
func TestGetClientUsageRelayLoopStopsAtOrigin(t *testing.T) {
	var a, b *Handler
	serverA := loopTestServer(&a, false)
	defer serverA.Close()
	serverB := loopTestServer(&b, false)
	defer serverB.Close()
	a = newRelayTestHandler(t, []*coreauth.Auth{relayTestAuth("to-b", serverB.URL, "sk-b")}, []config.QuotaAPIKey{{APIKey: "sk-a", Credentials: []string{"to-b"}}}, false)
	b = newRelayTestHandler(t, []*coreauth.Auth{relayTestAuth("to-a", serverA.URL, "sk-a")}, []config.QuotaAPIKey{{APIKey: "sk-b", Credentials: []string{"to-a"}}}, false)

	resp := serveClientUsage(t, a, "sk-a")
	if len(resp.Credentials) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	toB := resp.Credentials[0]
	if toB.Status != "ok" || len(toB.Upstream) != 1 {
		t.Fatalf("to-b = %+v", toB)
	}
	if toA := toB.Upstream[0]; toA.Status != "error" || toA.Error != clientUsageLoopError || len(toA.Upstream) != 0 {
		t.Fatalf("to-a = %+v", toA)
	}
}

// Through proxies that do not forward Via, a loop still ends at the depth
// limit instead of recursing or waiting on its own in-flight probe.
func TestGetClientUsageRelayLoopWithoutViaStopsAtDepthLimit(t *testing.T) {
	var a, b *Handler
	serverA := loopTestServer(&a, true)
	defer serverA.Close()
	serverB := loopTestServer(&b, true)
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
