package usagestore

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

var inFlight atomic.Int64

// InFlight returns the number of model requests and WebSocket sessions currently being served.
func InFlight() int64 { return inFlight.Load() }

// InFlightMiddleware counts model calls while their handler runs: POST requests and WebSocket
// upgrades outside the management API. Counting is cheap and independent of the store switch.
func InFlightMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !countsAsModelCall(c.Request) {
			c.Next()
			return
		}
		inFlight.Add(1)
		defer inFlight.Add(-1)
		c.Next()
	}
}

func countsAsModelCall(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/v0/") || strings.HasPrefix(path, "/v8/") || strings.HasPrefix(path, "/management") {
		return false
	}
	if r.Method == http.MethodPost {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	for _, value := range r.Header.Values("Upgrade") {
		if strings.EqualFold(strings.TrimSpace(value), "websocket") {
			return true
		}
	}
	return false
}
