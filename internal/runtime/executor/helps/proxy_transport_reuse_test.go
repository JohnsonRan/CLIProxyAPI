package helps

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestNewProxyAwareHTTPClientReusesProxyTransport(t *testing.T) {
	ResetProxyTransports()
	t.Cleanup(ResetProxyTransports)

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-global.example:8080"}}
	auth := &cliproxyauth.Auth{ID: "a"}
	first := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	second := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	if first.Transport == nil || first.Transport != second.Transport {
		t.Fatalf("transports differ for the same credential and proxy: %p vs %p", first.Transport, second.Transport)
	}
	otherKey := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ID: "b"}, 0)
	if otherKey.Transport == first.Transport {
		t.Fatal("different credentials must not share a transport")
	}
	other := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ID: "a", ProxyURL: "http://reuse-auth.example:8080"}, 0)
	if other.Transport == first.Transport {
		t.Fatal("different proxy URLs must not share a transport")
	}
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), "http://reuse-auth.example:8080")
	if client := NewProxyAwareHTTPClient(ctx, cfg, auth, 0); client.Transport != other.Transport {
		t.Fatal("request and credential proxy settings must resolve to the same pool")
	}
}

func TestNewUtlsHTTPClientReusesFallbackProxyTransport(t *testing.T) {
	ResetProxyTransports()
	t.Cleanup(ResetProxyTransports)

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-utls.example:8080"}}
	auth := &cliproxyauth.Auth{ID: "utls"}
	first := NewUtlsHTTPClient(context.Background(), cfg, auth, 0).Transport.(*fallbackRoundTripper)
	second := NewUtlsHTTPClient(context.Background(), cfg, auth, 0).Transport.(*fallbackRoundTripper)
	fallback := first.fallbackRoundTripper()
	if fallback != second.fallbackRoundTripper() {
		t.Fatal("fallback transports differ for the same credential and proxy")
	}
	if generic := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0); generic.Transport != fallback {
		t.Fatal("standard client paths must share the credential pool")
	}
	anthropic := first.anthropicRoundTripper()
	if anthropic != second.anthropicRoundTripper() || anthropic == fallback {
		t.Fatal("Anthropic must reuse its own fingerprint-specific pool")
	}
	other := NewUtlsHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ID: "other"}, 0).Transport.(*fallbackRoundTripper)
	if other.fallbackRoundTripper() == fallback || other.anthropicRoundTripper() == anthropic {
		t.Fatal("different credentials must not share either transport kind")
	}
}

func TestDefaultProxyPoolHonorsOverrides(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override *cliproxyauth.ProxyConnectionPoolOverride
	}{
		{"disabled", &cliproxyauth.ProxyConnectionPoolOverride{Enabled: boolPtr(false)}},
		{"zero idle", &cliproxyauth.ProxyConnectionPoolOverride{IdleConnTimeout: "0s"}},
		{"zero max", &cliproxyauth.ProxyConnectionPoolOverride{MaxIdleConnsPerHost: intPtr(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetProxyTransports()
			t.Cleanup(ResetProxyTransports)
			cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-override.example:8080"}}
			auth := &cliproxyauth.Auth{ID: "override", ProxyConnectionPool: tc.override}
			first := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
			second := NewUtlsHTTPClient(context.Background(), cfg, auth, 0).Transport.(*fallbackRoundTripper)
			if first.Transport == nil || first.Transport == second.fallbackRoundTripper() {
				t.Fatal("explicit opt-out or zero bounds must bypass the cache")
			}
			if got := proxyTransportCache.Len(); got != 0 {
				t.Fatalf("disabled pooling left %d cache entries", got)
			}
		})
	}
}

func TestDefaultProxyPoolRetiresOnReload(t *testing.T) {
	publisher := RegisterProxyPoolPublisher()
	t.Cleanup(func() { RetireProxyPoolPublisher(publisher) })
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-reload.example:8080"}}
	AdvanceProxyPoolGeneration(publisher, cfg)
	auth := &cliproxyauth.Auth{ID: "reload"}
	first := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0).Transport
	next := *cfg
	AdvanceProxyPoolGeneration(publisher, &next)
	stale := NewProxyAwareHTTPClient(context.Background(), cfg, auth, 0).Transport.(*http.Transport)
	if stale == first || !stale.DisableKeepAlives {
		t.Fatal("retired config must not reuse or repopulate its pool")
	}
	current := NewProxyAwareHTTPClient(context.Background(), &next, auth, 0).Transport
	if current == first || current == stale || current != NewProxyAwareHTTPClient(context.Background(), &next, auth, 0).Transport {
		t.Fatal("new config must own a new reusable pool")
	}
}

func TestNewProxyAwareHTTPClientKeepsProxyConnectionsAcrossRequests(t *testing.T) {
	ResetProxyTransports()
	t.Cleanup(ResetProxyTransports)

	var connections atomic.Int32
	proxyServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	proxyServer.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	proxyServer.Start()
	t.Cleanup(proxyServer.Close)

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: proxyServer.URL}}
	for i := 0; i < 3; i++ {
		client := NewProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
		resp, errDo := client.Get("http://upstream.invalid/v1/models")
		if errDo != nil {
			t.Fatalf("request %d: %v", i, errDo)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("proxy connections = %d, want 1", got)
	}
}

func TestNewProxyAwareHTTPClientInvalidProxyFallsBackToContextTransport(t *testing.T) {
	contextTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(contextTransport))
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "ftp://unsupported.example:21"}}
	client := NewProxyAwareHTTPClient(ctx, cfg, nil, 0)
	if client.Transport != contextTransport {
		t.Fatalf("transport = %#v, want context transport", client.Transport)
	}
}
