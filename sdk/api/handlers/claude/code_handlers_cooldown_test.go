package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestClaudeCooldownMessageMatchesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Cooldowns longer than coreauth.QuotaExhaustedCooldownThreshold are a final
	// usage limit: no Retry-After, X-Should-Retry: false, and the reset time is
	// in the usage-limit message instead of the "reset after" hint.
	for _, tt := range []struct {
		name        string
		duration    time.Duration
		header      string
		wantMessage string
	}{
		{name: "reported delay", duration: 250 * time.Second, header: "250", wantMessage: "cooling down via provider claude (reset after 4m10s)"},
		{name: "round up fractional second", duration: 60*time.Second + 100*time.Millisecond, header: "61", wantMessage: "cooling down via provider claude (reset after 1m1s)"},
		{name: "usage limit", duration: 8250 * time.Second, wantMessage: "Usage limit reached: all credentials for model claude-sonnet-4-5-20250929 have exhausted their quota via provider claude. Try again in 2h17m30s."},
		{name: "largest duration", duration: time.Duration(1<<63 - 1), wantMessage: "Try again in 2562047h47m16.854775807s."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: http.StatusTooManyRequests,
				Error:      coreauth.NewModelCooldownError("claude-sonnet-4-5-20250929", "claude", tt.duration),
			})

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", recorder.Code)
			}
			if got := recorder.Header().Get("Retry-After"); got != tt.header {
				t.Fatalf("Retry-After = %q, want %q", got, tt.header)
			}
			wantShouldRetry := ""
			if tt.header == "" {
				wantShouldRetry = "false"
			}
			if got := recorder.Header().Get("X-Should-Retry"); got != wantShouldRetry {
				t.Fatalf("X-Should-Retry = %q, want %q", got, wantShouldRetry)
			}
			if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.type").String(); got != "rate_limit_error" {
				t.Fatalf("error.type = %q, want rate_limit_error", got)
			}
			message := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.message").String()
			if !strings.Contains(message, tt.wantMessage) {
				t.Fatalf("error.message = %q, want it to contain %q", message, tt.wantMessage)
			}
			if tt.header == "" && strings.Contains(message, "reset after") {
				t.Fatalf("error.message = %q, want no second reset hint on a usage limit", message)
			}
		})
	}
}

func TestClaudeCooldownMessageInCommittedStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Header("Content-Type", "text/event-stream")
	if _, errWrite := c.Writer.Write([]byte(": keep-alive\n\n")); errWrite != nil {
		t.Fatalf("write keep-alive: %v", errWrite)
	}
	c.Writer.Flush()

	handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("claude-sonnet-4-5-20250929", "claude", 8250*time.Second),
	}
	close(errs)
	handler.forwardClaudeStream(c, c.Writer, func(error) {}, nil, errs)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "event: error\ndata: ") {
		t.Fatalf("status = %d, body = %q; want a terminal SSE error", recorder.Code, body)
	}
	_, event, _ := strings.Cut(body, "event: error\ndata: ")
	message := ojson.Get(strings.TrimSpace(event), "error.message").String()
	if !strings.Contains(message, "Usage limit reached") || !strings.Contains(message, "Try again in 2h17m30s.") {
		t.Fatalf("SSE error.message = %q, want usage limit message with reset in 2h17m30s", message)
	}
}

func TestClaudeCooldownLeavesOrdinary429MessageUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil))
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"error":{"type":"rate_limit_error","message":"upstream quota exceeded"}}`),
		Addon:      http.Header{"Retry-After": {"300"}},
	})
	if got := recorder.Header().Get("Retry-After"); got != "300" {
		t.Fatalf("Retry-After = %q, want passed-through 300", got)
	}
	if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.message").String(); got != "upstream quota exceeded" {
		t.Fatalf("error.message = %q, want original upstream message", got)
	}
}

func TestClaudeCooldownTrustedRetryAfterWinsOverPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil))
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("claude-model", "claude", 60*time.Second+100*time.Millisecond),
		Addon:      http.Header{"Retry-After": {"300"}},
	})
	if got := recorder.Header().Get("Retry-After"); got != "61" {
		t.Fatalf("Retry-After = %q, want trusted 61", got)
	}
	if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.message").String(); !strings.Contains(got, "reset after 1m1s") {
		t.Fatalf("error.message = %q, want trusted retry delay", got)
	}
}

func TestClaudeCooldownSkipsZeroDelayAndOtherStatuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name     string
		status   int
		duration time.Duration
	}{
		{name: "zero delay", status: http.StatusTooManyRequests},
		{name: "other status", status: http.StatusServiceUnavailable, duration: time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode: tt.status,
				Error:      coreauth.NewModelCooldownError("claude-model", "claude", tt.duration),
			})
			if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.message").String(); strings.Contains(got, "reset after") {
				t.Fatalf("error.message = %q, want no reset hint", got)
			}
		})
	}
}

func TestClaudeCooldownDirectResponseIsUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := `{"type":"error","error":{"type":"rate_limit_error","message":"original response"}}`
	handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode:     http.StatusTooManyRequests,
		Error:          coreauth.NewModelCooldownError("claude-model", "claude", time.Minute),
		DirectResponse: true,
		Body:           []byte(body),
	})
	if got := recorder.Body.String(); got != body {
		t.Fatalf("direct response = %q, want %q", got, body)
	}
}

func TestClaudeCooldownMessagesRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunks := make(chan coreexecutor.StreamChunk)
	close(chunks)
	executor := &preFTKeepAliveClaudeExecutor{chunks: chunks}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	const model = "issue-7285-cooldown-model"
	const authID = "issue-7285-cooldown-auth"
	// Stay below coreauth.QuotaExhaustedCooldownThreshold so the cooldown keeps
	// Retry-After; longer cooldowns are covered by TestClaudeCooldownMessageMatchesRetryAfter.
	next := time.Now().Add(250 * time.Second)
	auth := &coreauth.Auth{
		ID:             authID,
		Provider:       executor.Identifier(),
		Status:         coreauth.StatusActive,
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next},
		LastError:      &coreauth.Error{Code: "rate_limit_error", Message: "upstream quota exceeded", HTTPStatus: http.StatusTooManyRequests},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)

	for _, streaming := range []bool{false, true} {
		t.Run(strconv.FormatBool(streaming), func(t *testing.T) {
			requestBody := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":` + strconv.FormatBool(streaming) + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
			}
			seconds, errSeconds := strconv.ParseInt(recorder.Header().Get("Retry-After"), 10, 64)
			if errSeconds != nil || seconds <= 0 {
				t.Fatalf("Retry-After = %q, want positive seconds", recorder.Header().Get("Retry-After"))
			}
			message := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.message").String()
			if !strings.Contains(message, "upstream quota exceeded") || !strings.Contains(message, "reset after "+(time.Duration(seconds)*time.Second).String()) {
				t.Fatalf("error.message = %q, want upstream cause and reset matching Retry-After %d", message, seconds)
			}
		})
	}
}
