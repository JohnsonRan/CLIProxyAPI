package helps

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func recoveryTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return server, roots
}

func TestUtlsRetriesFailedHandshakeBeforeSendingBody(t *testing.T) {
	for _, dialTimeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "connection reset", true: "dial timeout"}[dialTimeout], func(t *testing.T) {
			var requests atomic.Int32
			server, roots := recoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil || string(body) != "send this exactly once" {
					t.Errorf("request body: %q, %v", body, errRead)
				}
				_, _ = w.Write([]byte("ok"))
			}))
			var dials atomic.Int32
			transport := &utlsRoundTripper{rootCAs: roots}
			transport.dialer = contextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					if dialTimeout {
						return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
					}
					client, remote := net.Pipe()
					go func() {
						defer func() { _ = remote.Close() }()
						_, _ = remote.Read(make([]byte, 4096))
					}()
					return client, nil
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewBufferString("send this exactly once"))
			resp, errRoundTrip := transport.RoundTrip(req)
			if errRoundTrip != nil {
				t.Fatal(errRoundTrip)
			}
			payload, errRead := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if errRead != nil || string(payload) != "ok" {
				t.Fatalf("response: %q, %v", payload, errRead)
			}
			if requests.Load() != 1 || dials.Load() != 2 {
				t.Fatalf("requests=%d dials=%d", requests.Load(), dials.Load())
			}
		})
	}
}

func TestUtlsPooledH2ReusesConnectionAcrossRequests(t *testing.T) {
	var requests, dials atomic.Int32
	server, roots := recoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	transport := &utlsRoundTripper{
		rootCAs: roots,
		poolH2:  true,
		h2Pool:  make(map[string]*http2.ClientConn),
		dialer: contextDialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}),
	}
	t.Cleanup(transport.CloseIdleConnections)

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewBufferString("hello"))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("round %d error: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if requests.Load() != 3 || dials.Load() != 1 {
		t.Fatalf("requests=%d dials=%d, want 3 requests over 1 pooled h2 dial", requests.Load(), dials.Load())
	}
}

func TestUtlsConnectionRetriesAreBoundedAndClassified(t *testing.T) {
	var dials atomic.Int32
	transport := &utlsRoundTripper{dialer: contextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.EOF}
	})}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://chatgpt.com", nil)
	_, errRoundTrip := transport.RoundTrip(req)
	if dials.Load() != utlsConnectionAttempts {
		t.Fatalf("error=%v attempts=%d", errRoundTrip, dials.Load())
	}
	// Exhausted dial retries must stay a plain transport error: an HTTP status or
	// Retry-After hint would make the conductor cool the credential.
	var opErr *net.OpError
	if !errors.As(errRoundTrip, &opErr) {
		t.Fatalf("expected *net.OpError, got %T: %v", errRoundTrip, errRoundTrip)
	}
	var status interface{ StatusCode() int }
	if errors.As(errRoundTrip, &status) {
		t.Fatalf("transport error must not carry an HTTP status: %v", errRoundTrip)
	}
	var retryAfter interface{ RetryAfter() *time.Duration }
	if errors.As(errRoundTrip, &retryAfter) {
		t.Fatalf("transport error must not carry a retry hint: %v", errRoundTrip)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
