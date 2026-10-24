package usagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/inflight"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// maxInFlightListed caps the in-flight calls returned by the realtime endpoint.
const maxInFlightListed = 100

// InFlightRequest is a downstream call still being served. The upstream fields stay empty
// until an executor starts an attempt (and for calls that never reach one, e.g. count_tokens).
type InFlightRequest struct {
	ID        uint64 `json:"id"`
	StartedAt int64  `json:"started_at"` // unix ms
	ElapsedMs int64  `json:"elapsed_ms"`
	Endpoint  string `json:"endpoint"`
	Transport string `json:"transport"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	AuthIndex string `json:"auth_index"`
	APIKeyID  string `json:"api_key_id"`
	APIKey    string `json:"api_key"`
}

// InFlight returns the number of downstream API calls and WebSocket sessions currently being served.
func InFlight() int64 { return int64(inflight.Count()) }

// InFlightMiddleware tracks downstream API calls while their handler runs: POST requests and
// WebSocket upgrades outside the management API. That is mostly model calls, but auxiliary POSTs
// (count_tokens, search, plugin routes) count too. Cheap and independent of the store switch.
func InFlightMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		webSocket := isWebSocketUpgrade(c.Request)
		if !countsAsModelCall(c.Request, webSocket) {
			c.Next()
			return
		}
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		end := inflight.Begin(c, c.Request.Method+" "+path, webSocket)
		defer end()
		c.Next()
	}
}

func countsAsModelCall(r *http.Request, webSocket bool) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/v0/") || strings.HasPrefix(path, "/v8/") || strings.HasPrefix(path, "/management") {
		return false
	}
	return r.Method == http.MethodPost || (r.Method == http.MethodGet && webSocket)
}

func isWebSocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, value := range r.Header.Values("Upgrade") {
		if strings.EqualFold(strings.TrimSpace(value), "websocket") {
			return true
		}
	}
	return false
}

// inFlightRequests lists the calls being served that match the dimension filters, newest first.
// A call whose upstream attempt has not started yet only matches when no filter is set.
func inFlightRequests(now time.Time, filter StatsFilter) []InFlightRequest {
	out := make([]InFlightRequest, 0)
	for _, r := range inflight.List() {
		apiKey := strings.TrimSpace(r.APIKey)
		apiKeyID := ""
		if apiKey != "" {
			sum := sha256.Sum256([]byte(apiKey))
			apiKeyID = hex.EncodeToString(sum[:6])
		}
		if !matchesAny(filter.Model, r.Model) || !matchesAny(filter.Provider, r.Provider) ||
			!matchesAny(filter.APIKeyID, apiKeyID) || !matchesAny(filter.AuthIndex, r.AuthIndex) {
			continue
		}
		out = append(out, InFlightRequest{
			ID:        r.ID,
			StartedAt: r.StartedAt.UnixMilli(),
			ElapsedMs: max(now.Sub(r.StartedAt).Milliseconds(), 0),
			Endpoint:  r.Endpoint,
			Transport: transportOf(r.WebSocket, r.Stream),
			Provider:  strings.TrimSpace(r.Provider),
			Model:     strings.TrimSpace(r.Model),
			AuthIndex: strings.TrimSpace(r.AuthIndex),
			APIKeyID:  apiKeyID,
			APIKey:    util.HideAPIKey(apiKey),
		})
		if len(out) == maxInFlightListed {
			break
		}
	}
	return out
}

func matchesAny(values []string, v string) bool {
	return len(values) == 0 || slices.Contains(values, strings.TrimSpace(v))
}
