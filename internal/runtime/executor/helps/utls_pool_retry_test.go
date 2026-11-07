package helps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestPrepareRetryOnFreshH2ConnOnlyRetriesUnsentRequests(t *testing.T) {
	newRequest := func(t *testing.T, ctx context.Context) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewBufferString("payload"))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	unusable := errors.New("http2: client conn not usable")

	t.Run("unusable connection replays the body", func(t *testing.T) {
		req := newRequest(t, context.Background())
		_, _ = io.ReadAll(req.Body)
		retry, ok := prepareRetryOnFreshH2Conn(req, unusable)
		if !ok {
			t.Fatal("expected a retry")
		}
		body, _ := io.ReadAll(retry.Body)
		if string(body) != "payload" {
			t.Fatalf("retried body = %q, want the full payload", body)
		}
	})

	t.Run("refused stream retries", func(t *testing.T) {
		if _, ok := prepareRetryOnFreshH2Conn(newRequest(t, context.Background()), http2.StreamError{Code: http2.ErrCodeRefusedStream}); !ok {
			t.Fatal("expected a retry for REFUSED_STREAM")
		}
	})

	t.Run("errors after the request may have been processed do not retry", func(t *testing.T) {
		for _, err := range []error{
			http2.StreamError{Code: http2.ErrCodeInternal},
			io.ErrUnexpectedEOF,
			errors.New("http2: server sent GOAWAY and closed the connection"),
		} {
			if _, ok := prepareRetryOnFreshH2Conn(newRequest(t, context.Background()), err); ok {
				t.Fatalf("retried after %v", err)
			}
		}
	})

	t.Run("body without GetBody does not retry", func(t *testing.T) {
		req := newRequest(t, context.Background())
		req.GetBody = nil
		if _, ok := prepareRetryOnFreshH2Conn(req, unusable); ok {
			t.Fatal("retried a request whose body cannot be replayed")
		}
	})

	t.Run("canceled request does not retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, ok := prepareRetryOnFreshH2Conn(newRequest(t, ctx), unusable); ok {
			t.Fatal("retried a canceled request")
		}
	})
}

func TestUtlsPooledH2ReplacedConnectionIsRetired(t *testing.T) {
	server, roots := recoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	transport := &utlsRoundTripper{
		rootCAs: roots,
		poolH2:  true,
		h2Pool:  make(map[string]*http2.ClientConn),
		dialer: contextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}),
	}
	t.Cleanup(transport.CloseIdleConnections)

	host, _, _ := net.SplitHostPort(server.Listener.Addr().String())
	addr := server.Listener.Addr().String()
	open := func() *http2.ClientConn {
		tlsConn, err := transport.connectWithRetry(context.Background(), host, addr)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := transport.newH2ClientConn(tlsConn)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	first, second := open(), open()
	transport.putPooledH2Conn(addr, first)
	transport.putPooledH2Conn(addr, second)

	deadline := time.Now().Add(5 * time.Second)
	for h2ConnUsable(first) {
		if time.Now().After(deadline) {
			t.Fatal("replaced pooled connection was never retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !h2ConnUsable(second) {
		t.Fatal("current pooled connection was retired")
	}
}
