package management

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	lmfpd "github.com/Ikaleio/lm-detector/sdk/go"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// noSDKTimeout disables the lmfpd request timer (a negative Timeout, since v0.2.0); the management request
// context is the only cancellation, as no timeout may follow an established upstream connection.
const noSDKTimeout = -time.Nanosecond

// fingerprintDetectRequest selects the model and API format used for one detection round.
type fingerprintDetectRequest struct {
	Model       string `json:"model"`
	API         string `json:"api"`
	APIKey      string `json:"api_key"`
	Effort      string `json:"effort"`
	ServiceTier string `json:"service_tier"`
	NoStream    bool   `json:"no_stream"`
	Strict      bool   `json:"strict"`
	Count       int    `json:"count"`
}

type fingerprintDetectResponse struct {
	*lmfpd.DetectResult
	BuiltAt string `json:"built_at"`
}

var (
	// fingerprintLoad is swapped in tests to avoid downloading reference data.
	fingerprintLoad = func(ctx context.Context) (*lmfpd.Detector, error) {
		return lmfpd.Load(ctx, lmfpd.LoadOptions{})
	}
)

type drainResponseWriter struct {
	w      http.ResponseWriter
	mu     sync.Mutex
	failed bool
}

func (d *drainResponseWriter) Header() http.Header {
	return d.w.Header()
}

func (d *drainResponseWriter) WriteHeader(statusCode int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed {
		return
	}
	d.w.WriteHeader(statusCode)
}

func (d *drainResponseWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed {
		return len(p), nil
	}
	n, err := d.w.Write(p)
	if err != nil {
		d.failed = true
		return len(p), nil
	}
	return n, nil
}

func (d *drainResponseWriter) Flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed {
		return
	}
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (d *drainResponseWriter) FlushError() error {
	d.Flush()
	return nil
}

func (d *drainResponseWriter) Unwrap() http.ResponseWriter {
	return d.w
}

type loopbackHandler struct {
	inner     http.Handler
	parentCtx context.Context
	inFlight  sync.WaitGroup
}

func (h *loopbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.inFlight.Add(1)
	defer h.inFlight.Done()

	// Detach from loopback client disconnect so streaming finishes draining upstream usage,
	// but cancel when the management parentCtx is cancelled.
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stop := context.AfterFunc(h.parentCtx, func() {
		cancel()
	})
	defer stop()
	defer cancel()

	dw := &drainResponseWriter{w: w}
	h.inner.ServeHTTP(dw, r.WithContext(reqCtx))
}

type loopbackServer struct {
	listener net.Listener
	server   *http.Server
	handler  *loopbackHandler
}

func startLoopbackServer(parentCtx context.Context, api http.Handler) (*loopbackServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	lb := &loopbackHandler{
		inner:     api,
		parentCtx: parentCtx,
	}
	server := &http.Server{Handler: lb}
	go func() {
		if errServe := server.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) && !errors.Is(errServe, net.ErrClosed) {
			log.WithError(errServe).Warn("management: fingerprint loopback server stopped")
		}
	}()
	return &loopbackServer{listener: listener, server: server, handler: lb}, nil
}

func (s *loopbackServer) BaseURL() string {
	return "http://" + s.listener.Addr().String() + "/v1"
}

func (s *loopbackServer) Close() {
	_ = s.listener.Close()
	s.handler.inFlight.Wait()
	_ = s.server.Close()
}

// FingerprintDetect returns a handler that runs one lmfpd detection round against a model served by this proxy.
// The challenges go through api (the server's own engine) on a loopback listener, so they take the normal
// client path: access keys, routing, executors and usage recording all see them as ordinary requests.
func (h *Handler) FingerprintDetect(api http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req fingerprintDetectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		req.Model = strings.TrimSpace(req.Model)
		if req.Model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "missing model"})
			return
		}
		apiKey := strings.TrimSpace(req.APIKey)
		if apiKey == "" {
			if cfg := h.configSnapshot(); cfg != nil {
				for _, key := range cfg.APIKeys {
					if key = strings.TrimSpace(key); key != "" {
						apiKey = key
						break
					}
				}
			}
		}
		if apiKey == "" {
			// lmfpd requires a key; without configured client keys the proxy does not check it.
			apiKey = "lmfpd"
		}

		ctx := c.Request.Context()
		detector, err := fingerprintLoad(ctx)
		if err != nil {
			log.WithError(err).Warn("management: load fingerprint detector data")
			c.JSON(http.StatusBadGateway, gin.H{"error": "fingerprint_data_unavailable", "message": err.Error()})
			return
		}

		lbServer, err := startLoopbackServer(ctx, api)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "loopback listen failed", "message": err.Error()})
			return
		}
		defer lbServer.Close()

		result, err := detector.Detect(ctx, lmfpd.DetectConfig{
			BaseURL:     lbServer.BaseURL(),
			APIKey:      apiKey,
			Model:       req.Model,
			API:         lmfpd.API(req.API),
			Effort:      req.Effort,
			ServiceTier: req.ServiceTier,
			NoStream:    req.NoStream,
			Strict:      req.Strict,
			Count:       req.Count,
			Timeout:     noSDKTimeout,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, fingerprintDetectResponse{DetectResult: result, BuiltAt: detector.BuiltAt()})
	}
}
