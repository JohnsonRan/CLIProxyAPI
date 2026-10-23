package usagestore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestInFlightMiddlewareCountsModelCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(InFlightMiddleware())
	var seen int64
	handler := func(c *gin.Context) {
		seen = InFlight()
		c.Status(http.StatusOK)
	}
	r.POST("/v1/chat/completions", handler)
	r.GET("/v1/responses", handler)
	r.GET("/v1/models", handler)
	r.POST("/v8/management/config", handler)

	for _, tc := range []struct {
		method, path string
		upgrade      bool
		want         int64
	}{
		{http.MethodPost, "/v1/chat/completions", false, 1},
		{http.MethodGet, "/v1/responses", true, 1},
		{http.MethodGet, "/v1/models", false, 0},
		{http.MethodPost, "/v8/management/config", false, 0},
	} {
		seen = -1
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.upgrade {
			req.Header.Set("Upgrade", "websocket")
		}
		r.ServeHTTP(httptest.NewRecorder(), req)
		if seen != tc.want {
			t.Fatalf("%s %s: in-flight during handler = %d, want %d", tc.method, tc.path, seen, tc.want)
		}
	}
	if InFlight() != 0 {
		t.Fatalf("counter must return to zero, got %d", InFlight())
	}
}

func TestEventTransport(t *testing.T) {
	ws := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{WebSocket: true})
	if got := eventFromRecord(ws, coreusage.Record{Model: "m", Stream: true}).Transport; got != "ws" {
		t.Fatalf("websocket transport = %q", got)
	}
	if got := eventFromRecord(context.Background(), coreusage.Record{Model: "m", Stream: true}).Transport; got != "sse" {
		t.Fatalf("stream transport = %q", got)
	}
	if got := eventFromRecord(context.Background(), coreusage.Record{Model: "m"}).Transport; got != "http" {
		t.Fatalf("plain transport = %q", got)
	}

	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.Enqueue(eventFromRecord(ws, coreusage.Record{Model: "m"}))
	s.Close()
	s, err = Open(s.path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	events, err := s.Events(context.Background(), EventFilter{})
	if err != nil || len(events) != 1 || events[0].Transport != "ws" {
		t.Fatalf("stored transport: %v %+v", err, events)
	}
}
