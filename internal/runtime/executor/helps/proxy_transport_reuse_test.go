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
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestNewProxyAwareHTTPClientReusesProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-global.example:8080"}}
	first := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ID: "a"}, 0)
	second := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ID: "b"}, 0)
	if first.Transport == nil || first.Transport != second.Transport {
		t.Fatalf("transports differ for the same proxy: %p vs %p", first.Transport, second.Transport)
	}

	other := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ProxyURL: "http://reuse-auth.example:8080"}, 0)
	if other.Transport == first.Transport {
		t.Fatal("different proxy URLs must not share a transport")
	}
}

func TestNewUtlsHTTPClientReusesFallbackProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://reuse-utls.example:8080"}}
	first := NewUtlsHTTPClient(context.Background(), cfg, nil, 0)
	second := NewUtlsHTTPClient(context.Background(), cfg, nil, 0)
	firstRT, okFirst := first.Transport.(*fallbackRoundTripper)
	secondRT, okSecond := second.Transport.(*fallbackRoundTripper)
	if !okFirst || !okSecond {
		t.Fatalf("transport types = %T, %T", first.Transport, second.Transport)
	}
	if firstRT.fallback != secondRT.fallback {
		t.Fatal("fallback transports differ for the same proxy")
	}
}

func TestNewProxyAwareHTTPClientKeepsProxyConnectionsAcrossRequests(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	contextTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(contextTransport))
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "ftp://unsupported.example:21"}}
	client := NewProxyAwareHTTPClient(ctx, cfg, nil, 0)
	if client.Transport != contextTransport {
		t.Fatalf("transport = %#v, want context transport", client.Transport)
	}
}
