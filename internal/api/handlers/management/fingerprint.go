package management

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	lmfpd "github.com/Ikaleio/lm-detector/sdk/go"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

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
	fingerprintDetectorMu sync.Mutex
	fingerprintDetector   *lmfpd.Detector
	// fingerprintLoad is swapped in tests to avoid downloading reference data.
	fingerprintLoad = func(ctx context.Context) (*lmfpd.Detector, error) {
		return lmfpd.Load(ctx, lmfpd.LoadOptions{})
	}
)

// loadFingerprintDetector loads the reference data once; a failed load is retried on the next call.
func loadFingerprintDetector(ctx context.Context) (*lmfpd.Detector, error) {
	fingerprintDetectorMu.Lock()
	defer fingerprintDetectorMu.Unlock()
	if fingerprintDetector != nil {
		return fingerprintDetector, nil
	}
	detector, err := fingerprintLoad(ctx)
	if err != nil {
		return nil, err
	}
	fingerprintDetector = detector
	return detector, nil
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
		detector, err := loadFingerprintDetector(ctx)
		if err != nil {
			log.WithError(err).Warn("management: load fingerprint detector data")
			c.JSON(http.StatusBadGateway, gin.H{"error": "fingerprint_data_unavailable", "message": err.Error()})
			return
		}

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "loopback listen failed", "message": err.Error()})
			return
		}
		server := &http.Server{Handler: api}
		go func() {
			if errServe := server.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
				log.WithError(errServe).Warn("management: fingerprint loopback server stopped")
			}
		}()
		defer func() {
			if errClose := server.Close(); errClose != nil {
				log.WithError(errClose).Warn("management: close fingerprint loopback server")
			}
		}()

		result, err := detector.Detect(ctx, lmfpd.DetectConfig{
			BaseURL:     "http://" + listener.Addr().String() + "/v1",
			APIKey:      apiKey,
			Model:       req.Model,
			API:         lmfpd.API(req.API),
			Effort:      req.Effort,
			ServiceTier: req.ServiceTier,
			NoStream:    req.NoStream,
			Strict:      req.Strict,
			Count:       req.Count,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, fingerprintDetectResponse{DetectResult: result, BuiltAt: detector.BuiltAt()})
	}
}
