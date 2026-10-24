package usagestore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/inflight"
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

func TestInFlightRequestsListAttachedUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(InFlightMiddleware())
	var unfiltered, byModel, byOther []InFlightRequest
	r.POST("/v1/responses", func(c *gin.Context) {
		ctx := context.WithValue(context.Background(), "gin", c)
		now := time.Now()
		if got := inFlightRequests(now, StatsFilter{Model: []string{"gpt-x"}}); len(got) != 0 {
			t.Errorf("call without an upstream attempt must not match a model filter: %+v", got)
		}
		inflight.Attach(ctx, inflight.Upstream{Provider: "codex", Model: "gpt-x", AuthIndex: "a1", APIKey: "sk-client-key-123456", Stream: true})
		unfiltered = inFlightRequests(now, StatsFilter{})
		byModel = inFlightRequests(now, StatsFilter{Model: []string{"gpt-x"}, Provider: []string{"codex"}})
		byOther = inFlightRequests(now, StatsFilter{AuthIndex: []string{"a2"}})
		c.Status(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))

	if len(unfiltered) != 1 || len(byModel) != 1 || len(byOther) != 0 {
		t.Fatalf("listed %d unfiltered, %d by model, %d by other credential", len(unfiltered), len(byModel), len(byOther))
	}
	got := unfiltered[0]
	if got.Endpoint != "POST /v1/responses" || got.Transport != "sse" || got.Model != "gpt-x" || got.Provider != "codex" || got.AuthIndex != "a1" {
		t.Fatalf("unexpected in-flight request: %+v", got)
	}
	if got.APIKeyID == "" || got.APIKey == "sk-client-key-123456" {
		t.Fatalf("client key must be hashed and masked: %+v", got)
	}
	if rest := inFlightRequests(time.Now(), StatsFilter{}); len(rest) != 0 {
		t.Fatalf("finished call still listed: %+v", rest)
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
