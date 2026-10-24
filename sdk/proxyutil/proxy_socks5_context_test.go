package proxyutil

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestBuildHTTPTransportSOCKS5DialHonorsContext(t *testing.T) {
	t.Parallel()

	// A SOCKS5 proxy that accepts the TCP connection but never answers the
	// handshake. Without context support the dial blocks until the proxy closes.
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	accepted := make(chan net.Conn, 1)
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept == nil {
			accepted <- conn
		}
	}()

	transport, _, errBuild := BuildHTTPTransport("socks5://" + listener.Addr().String())
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport: %v", errBuild)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, errDial := transport.DialContext(ctx, "tcp", "upstream.example:443")
		if conn != nil {
			_ = conn.Close()
		}
		done <- errDial
	}()

	select {
	case errDial := <-done:
		if errDial == nil {
			t.Fatal("expected dial through an unresponsive SOCKS5 proxy to fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOCKS5 dial ignored context cancellation")
	}
}
