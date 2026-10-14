package helps

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	internalcache "github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using a Chrome fingerprint for
// providers that require browser-like TLS. The HTTP version follows ALPN.
type utlsRoundTripper struct {
	dialer       proxy.Dialer
	rootCAs      *x509.CertPool
	sessionCache tls.ClientSessionCache
	poolH2       bool

	poolMu sync.Mutex
	h2Pool map[string]*http2.ClientConn
}

const (
	utlsConnectionAttempts           = 3
	chatGPTSessionCacheCapacity      = 32
	chatGPTRoundTripperCacheCapacity = 64
)

var chatGPTRoundTripperCache = internalcache.NewBoundedLRU[string, http.RoundTripper](
	chatGPTRoundTripperCacheCapacity,
	func(_ string, roundTripper http.RoundTripper) {
		if transport, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	},
)

func cachedChatGPTRoundTripper(proxyURL string) http.RoundTripper {
	return chatGPTRoundTripperCache.GetOrAdd(proxyURL, func() http.RoundTripper {
		return newUtlsRoundTripper(proxyURL)
	})
}

type utlsConnectionError struct{ err error }

func (e *utlsConnectionError) Error() string   { return e.err.Error() }
func (e *utlsConnectionError) Unwrap() error   { return e.err }
func (e *utlsConnectionError) StatusCode() int { return http.StatusBadGateway }
func (e *utlsConnectionError) RetryAfter() *time.Duration {
	delay := 5 * time.Second
	return &delay
}

func retryableUtlsConnectionError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	var certificateErr *tls.CertificateVerificationError
	if errors.As(err, &certificateErr) {
		return false
	}
	var unknownAuthErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthErr) {
		return false
	}
	var certInvalidErr x509.CertificateInvalidError
	if errors.As(err, &certInvalidErr) {
		return false
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return false
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	var operationErr *net.OpError
	return errors.As(err, &operationErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (t *utlsRoundTripper) connectWithRetry(ctx context.Context, host, addr string) (*tls.UConn, error) {
	for attempt := 1; attempt <= utlsConnectionAttempts; attempt++ {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		started := time.Now()
		connection, errConnect := t.createConnection(ctx, host, addr)
		if errConnect == nil {
			return connection, nil
		}
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		if !retryableUtlsConnectionError(errConnect) {
			return nil, errConnect
		}
		if attempt == utlsConnectionAttempts {
			return nil, &utlsConnectionError{err: errConnect}
		}
		phase := "dial"
		if strings.Contains(errConnect.Error(), "TLS handshake") {
			phase = "handshake"
		}
		log.WithFields(log.Fields{
			"host": host, "phase": phase, "attempt": attempt,
			"max_attempts": utlsConnectionAttempts, "elapsed_ms": time.Since(started).Milliseconds(),
		}).Warn("utls: retrying connection setup after transient failure")
		timer := time.NewTimer(time.Duration(100+rand.IntN(151)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("utls: connection attempts exhausted")
}

// utlsH2IdleTimeout closes pooled HTTP/2 connections that carry no streams,
// matching net/http's default idle connection lifetime.
const utlsH2IdleTimeout = 90 * time.Second

// retireH2Conn stops conn from taking new requests and closes it once its
// in-flight streams finish, without interrupting them.
func retireH2Conn(conn *http2.ClientConn) {
	if conn == nil {
		return
	}
	go func() {
		if errShutdown := conn.Shutdown(context.Background()); errShutdown != nil {
			_ = conn.Close()
		}
	}()
}

// h2ConnUsable reports whether conn can carry a new request. CanTakeNewRequest
// alone still reports true for a connection that was closed before its first
// request, so the closing state is checked as well.
func h2ConnUsable(conn *http2.ClientConn) bool {
	state := conn.State()
	return !state.Closed && !state.Closing && conn.CanTakeNewRequest()
}

func (t *utlsRoundTripper) newH2ClientConn(conn net.Conn) (*http2.ClientConn, error) {
	return (&http2.Transport{IdleConnTimeout: utlsH2IdleTimeout}).NewClientConn(conn)
}

func (t *utlsRoundTripper) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.poolMu.Lock()
	conns := make([]*http2.ClientConn, 0, len(t.h2Pool))
	for addr, conn := range t.h2Pool {
		delete(t.h2Pool, addr)
		if conn != nil {
			conns = append(conns, conn)
		}
	}
	t.poolMu.Unlock()
	for _, conn := range conns {
		retireH2Conn(conn)
	}
}

func (t *utlsRoundTripper) getPooledH2Conn(addr string) *http2.ClientConn {
	if t == nil || !t.poolH2 {
		return nil
	}
	t.poolMu.Lock()
	conn := t.h2Pool[addr]
	if conn != nil && h2ConnUsable(conn) {
		t.poolMu.Unlock()
		return conn
	}
	if conn != nil {
		delete(t.h2Pool, addr)
	}
	t.poolMu.Unlock()
	retireH2Conn(conn)
	return nil
}

func (t *utlsRoundTripper) putPooledH2Conn(addr string, conn *http2.ClientConn) {
	if t == nil || !t.poolH2 || conn == nil {
		return
	}
	t.poolMu.Lock()
	if t.h2Pool == nil {
		t.h2Pool = make(map[string]*http2.ClientConn)
	}
	previous := t.h2Pool[addr]
	t.h2Pool[addr] = conn
	t.poolMu.Unlock()
	if previous != conn {
		retireH2Conn(previous)
	}
}

// dropPooledH2Conn removes conn from the pool if it is still the pooled entry
// and retires it.
func (t *utlsRoundTripper) dropPooledH2Conn(addr string, conn *http2.ClientConn) {
	if t == nil || conn == nil {
		return
	}
	t.poolMu.Lock()
	if t.h2Pool[addr] == conn {
		delete(t.h2Pool, addr)
	}
	t.poolMu.Unlock()
	retireH2Conn(conn)
}

// prepareRetryOnFreshH2Conn decides whether a request that failed on a pooled
// connection may be sent again on a new one. Only failures where the server
// cannot have processed the request qualify: the connection became unusable
// before the stream started, or the server refused the stream. The body is
// rewound through GetBody; requests whose body cannot be replayed are not retried.
func prepareRetryOnFreshH2Conn(req *http.Request, err error) (*http.Request, bool) {
	if req == nil || err == nil || req.Context().Err() != nil {
		return nil, false
	}
	var streamErr http2.StreamError
	refused := errors.As(err, &streamErr) && streamErr.Code == http2.ErrCodeRefusedStream
	message := err.Error()
	unusable := strings.Contains(message, "client conn not usable") ||
		strings.Contains(message, "client conn is closed") ||
		strings.Contains(message, "graceful shutdown GOAWAY")
	if !refused && !unusable {
		return nil, false
	}
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, errBody := req.GetBody()
	if errBody != nil {
		return nil, false
	}
	retry := req.Clone(req.Context())
	retry.Body = body
	return retry, true
}

type closeConnectionBody struct {
	io.ReadCloser
	closeConnection func() error
	once            sync.Once
	err             error
}

func (b *closeConnectionBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		var errConnection error
		if b.closeConnection != nil {
			errConnection = b.closeConnection()
		}
		var errBody error
		if b.ReadCloser != nil {
			errBody = b.ReadCloser.Close()
		}
		b.err = errors.Join(errBody, errConnection)
	})
	return b.err
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{
		dialer:       dialer,
		sessionCache: tls.NewLRUClientSessionCache(chatGPTSessionCacheCapacity),
		poolH2:       true,
		h2Pool:       make(map[string]*http2.ClientConn),
	}
}

func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (*tls.UConn, error) {
	dialer := t.dialer
	if dialer == nil {
		dialer = proxy.Direct
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("utls: dialer does not support context cancellation")
	}
	conn, errDial := contextDialer.DialContext(ctx, "tcp", addr)
	if errDial != nil {
		return nil, fmt.Errorf("utls: dial upstream: %w", errDial)
	}

	tlsConfig := &tls.Config{
		ServerName:         host,
		RootCAs:            t.rootCAs,
		ClientSessionCache: t.sessionCache,
	}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	handshakeCtx, cancelHandshake := handshakeContext(ctx)
	defer cancelHandshake()
	if errHandshake := tlsConn.HandshakeContext(handshakeCtx); errHandshake != nil {
		errHandshake = classifyHandshakeError(ctx, handshakeCtx, errHandshake)
		if errors.Is(errHandshake, context.Canceled) || errors.Is(errHandshake, context.DeadlineExceeded) {
			return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
		}
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: TLS handshake: %w; close connection: %v", errHandshake, errClose)
		}
		return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
	}

	return tlsConn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	if h2Conn := t.getPooledH2Conn(addr); h2Conn != nil {
		resp, err := h2Conn.RoundTrip(req)
		if err == nil && resp != nil {
			if resp.Body == nil {
				resp.Body = http.NoBody
			}
			return resp, nil
		}
		t.dropPooledH2Conn(addr, h2Conn)
		if err == nil {
			return nil, fmt.Errorf("utls: upstream returned an empty response")
		}
		retry, ok := prepareRetryOnFreshH2Conn(req, err)
		if !ok {
			return nil, err
		}
		req = retry
	}

	tlsConn, err := t.connectWithRetry(req.Context(), hostname, addr)
	if err != nil {
		return nil, err
	}
	if t.poolH2 && tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		h2Conn, errClientConn := t.newH2ClientConn(tlsConn)
		if errClientConn != nil {
			_ = tlsConn.Close()
			return nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
		}
		t.putPooledH2Conn(addr, h2Conn)
		resp, errRT := h2Conn.RoundTrip(req)
		if errRT != nil {
			t.dropPooledH2Conn(addr, h2Conn)
			return nil, errRT
		}
		if resp == nil {
			return nil, fmt.Errorf("utls: upstream returned an empty response")
		}
		if resp.Body == nil {
			resp.Body = http.NoBody
		}
		return resp, nil
	}
	return roundTripUtlsConnection(req, tlsConn)
}

// roundTripUtlsConnection selects the HTTP protocol before sending the request.
// Empty ALPN is HTTP/1.1, including when a TLS-inspecting proxy omits ALPN.
func roundTripUtlsConnection(req *http.Request, tlsConn *tls.UConn) (*http.Response, error) {
	closeConnection := func() error {
		// The HTTP/1.1 transport may already have closed its non-pooled
		// connection after reading the response or canceling the request.
		if errClose := tlsConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return errClose
		}
		return nil
	}
	var resp *http.Response
	var err error
	switch protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol {
	case "h2":
		h2Conn, errClientConn := (&http2.Transport{}).NewClientConn(tlsConn)
		if errClientConn != nil {
			err = fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
			break
		}
		closeConnection = h2Conn.Close
		resp, err = h2Conn.RoundTrip(req)
	case "", "http/1.1":
		// Reuse the already-handshaken uTLS connection. A fresh, non-pooling
		// transport retains net/http's cancellation and request-body handling
		// without changing the TLS fingerprint or redialing through another path.
		transport := &http.Transport{
			DisableKeepAlives: true,
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return tlsConn, nil
			},
		}
		resp, err = transport.RoundTrip(req)
		transport.CloseIdleConnections()
	default:
		err = fmt.Errorf("utls: unsupported negotiated protocol %q", protocol)
	}
	if err != nil {
		if errClose := closeConnection(); errClose != nil {
			log.Debugf("utls: close connection after round trip failure: %v", errClose)
		}
		return nil, err
	}
	if resp == nil {
		if errClose := closeConnection(); errClose != nil {
			log.Debugf("utls: close connection after empty response: %v", errClose)
		}
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &closeConnectionBody{
		ReadCloser:      resp.Body,
		closeConnection: closeConnection,
	}
	return resp, nil
}

// claudeCodeSessionCacheCapacity bounds the per-transport TLS session cache for
// the Anthropic inference plane.
const claudeCodeSessionCacheCapacity = 32

// newClaudeCodeTLSConfig builds the uTLS config for one inference-plane dial.
//
// OmitEmptyPsk keeps the pre_shared_key extension silent until a session is
// cached, so an unresumed ClientHello stays byte-identical to the captured
// native handshake. PreferSkipResumptionOnNilExtension turns uTLS's HelloCustom
// "resume without the matching extension" panic into a skipped resumption.
func newClaudeCodeTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return newClaudeCodeTLSConfigWithRoots(host, sessionCache, nil)
}

func newClaudeCodeTLSConfigWithRoots(host string, sessionCache tls.ClientSessionCache, roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		RootCAs:                            roots,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

// claudeCodeTLSClientHelloSpec reproduces the deterministic Node/OpenSSL
// ClientHello emitted by Claude Code 2.1.220 on macOS arm64. Keep this spec in
// sync with a fresh native capture whenever the advertised Claude Code version
// changes.
func claudeCodeTLSClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []uint8{0},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SessionTicketExtension{},
			&tls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&tls.StatusRequestExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{
				tls.ECDSAWithP256AndSHA256,
				tls.PSSWithSHA256,
				tls.PKCS1WithSHA256,
				tls.ECDSAWithP384AndSHA384,
				tls.PSSWithSHA384,
				tls.PKCS1WithSHA384,
				tls.PSSWithSHA512,
				tls.PKCS1WithSHA512,
				tls.PKCS1WithSHA1,
			}},
			&tls.SCTExtension{},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
			// pre_shared_key MUST be the final extension (RFC 8446 4.2.11), after
			// padding. It contributes zero bytes until a cached session exists.
			&tls.UtlsPreSharedKeyExtension{},
		},
	}
}

const claudeCodeRoundTripperCacheCapacity = 64

var claudeCodeRoundTripperCache = internalcache.NewBoundedLRU[string, http.RoundTripper](
	claudeCodeRoundTripperCacheCapacity,
	func(_ string, roundTripper http.RoundTripper) {
		if transport, ok := roundTripper.(interface{ CloseIdleConnections() }); ok {
			transport.CloseIdleConnections()
		}
	},
)

var claudeCodeMessagesHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

var claudeCodeCountTokensHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

func claudeCodeRequestHeaderOrder(_, requestTarget string) []string {
	if strings.HasPrefix(requestTarget, "/v1/messages/count_tokens") {
		return claudeCodeCountTokensHeaderOrder
	}
	return claudeCodeMessagesHeaderOrder
}

func cachedClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	return claudeCodeRoundTripperCache.GetOrAdd(proxyURL, func() http.RoundTripper {
		return newClaudeCodeRoundTripper(proxyURL)
	})
}

func newClaudeCodeRoundTripper(proxyURL string) http.RoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("claude tls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return newClaudeCodeRoundTripperForDialer(dialer)
}

// newClaudeCodeRoundTripperForDialer builds the Claude Code transport on top
// of an already-resolved dialer. It is split out from
// newClaudeCodeRoundTripper so tests can inject in-memory connections.
func newClaudeCodeRoundTripperForDialer(dialer proxy.Dialer) *http.Transport {
	return newClaudeCodeRoundTripperForDialerAndRoots(dialer, nil)
}

func newClaudeCodeRoundTripperForDialerAndRoots(dialer proxy.Dialer, roots *x509.CertPool) *http.Transport {
	// The cache is scoped to this round tripper, which is already keyed by proxy,
	// so resumption never crosses proxy boundaries.
	sessionCache := tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity)

	transport := &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var (
				conn net.Conn
				err  error
			)
			if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
				conn, err = contextDialer.DialContext(ctx, network, addr)
			} else {
				conn, err = dialer.Dial(network, addr)
			}
			if err != nil {
				return nil, fmt.Errorf("claude tls: dial upstream: %w", err)
			}

			host, _, errSplit := net.SplitHostPort(addr)
			if errSplit != nil {
				if errClose := conn.Close(); errClose != nil {
					log.Debugf("claude tls: close failed connection: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: split upstream address: %w", errSplit)
			}
			tlsConn := tls.UClient(conn, newClaudeCodeTLSConfigWithRoots(host, sessionCache, roots), tls.HelloCustom)
			if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after preset failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: apply Claude Code ClientHello: %w", errPreset)
			}
			handshakeCtx, cancelHandshake := handshakeContext(ctx)
			defer cancelHandshake()
			if errHandshake := tlsConn.HandshakeContext(handshakeCtx); errHandshake != nil {
				errHandshake = classifyHandshakeError(ctx, handshakeCtx, errHandshake)
				if errClose := tlsConn.Close(); errClose != nil {
					log.Debugf("claude tls: close connection after handshake failure: %v", errClose)
				}
				return nil, fmt.Errorf("claude tls: handshake upstream: %w", errHandshake)
			}
			return httpwire.NewOrderedRequestConn(tlsConn, claudeCodeRequestHeaderOrder), nil
		},
	}
	return transport
}

// fallbackRoundTripper uses provider-specific TLS fingerprints for protected
// HTTPS hosts and falls back to the standard transport for all other requests.
type fallbackRoundTripper struct {
	anthropic http.RoundTripper
	chrome    http.RoundTripper
	fallback  http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if IsAnthropicUpstreamURL(req.URL) {
		return f.anthropic.RoundTrip(req)
	}
	if req.URL.Scheme == "https" && strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return f.chrome.RoundTrip(req)
	}
	return f.fallback.RoundTrip(req)
}

// NewUtlsHTTPClient creates an HTTP client using provider-specific TLS
// fingerprints for protected hosts. It uses Claude Code's Node/OpenSSL profile
// for Anthropic and a Chrome profile for ChatGPT, with a standard-transport
// fallback for other hosts.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) (clientOut *http.Client) {
	// Carry the opt-in TLS establishment bound (requests
	// tls-handshake-timeout-seconds) on a request-scoped wrapper. This
	// client is shared, so the bound intentionally reaches both the Claude
	// Code Anthropic dial path and the Chrome ChatGPT/Codex dial path; the
	// standard fallback transport ignores the context value. The uTLS dial
	// paths read it per dial, so cached per-proxy transports need no rebuild
	// when the value changes. A disabled bound (0) leaves requests untouched.
	var handshakeTimeout time.Duration
	if cfg != nil {
		handshakeTimeout = cfg.TLSHandshakeTimeout()
	}
	defer func() {
		clientOut = httptransport.Decorate(ctx, clientOut)
		if clientOut != nil && handshakeTimeout > 0 {
			clientOut.Transport = &tlsHandshakeTimeoutRoundTripper{base: clientOut.Transport, timeout: handshakeTimeout}
		}
	}()
	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var chromeRT http.RoundTripper = cachedChatGPTRoundTripper(proxyURL)
	var anthropicRT http.RoundTripper = cachedClaudeCodeRoundTripper(proxyURL)
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		chromeRT = ctxRoundTripper
		anthropicRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			anthropic: anthropicRT,
			chrome:    chromeRT,
			fallback:  standardTransport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
