package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const decisionsTestResponse = `{"id":"dec_1","object":"decision","model":"gpt-6-luna","answers":[{"type":"predicate","name":"damaged","probability":0.97}],"usage":{"input_tokens":41,"output_tokens":3,"total_tokens":44}}`

type decisionsUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *decisionsUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type decisionsUsageNoop struct{}

func (decisionsUsageNoop) HandleUsage(context.Context, usage.Record) {}

type decisionsUpstream struct {
	server *httptest.Server
	path   string
	auth   string
	body   string
}

func newDecisionsUpstream(t *testing.T, status int, response string) *decisionsUpstream {
	t.Helper()
	u := &decisionsUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.path = r.URL.Path
		u.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		u.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(u.server.Close)
	return u
}

func newDecisionsTestRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	body := []byte(`{"model":"luna","input":"The package arrived with a broken screen.","questions":[{"type":"predicate","name":"damaged","instructions":"Does the customer report a damaged item?"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: body}
	opts := cliproxyexecutor.Options{
		OriginalRequest: body,
		SourceFormat:    sdktranslator.FromString("openai-decisions"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:    "/v1/decisions",
			cliproxyexecutor.RequestedModelMetadataKey: "luna",
		},
	}
	return req, opts
}

func TestOpenAICompatExecutorDecisionsPassthrough(t *testing.T) {
	upstream := newDecisionsUpstream(t, http.StatusOK, decisionsTestResponse)
	exec := NewOpenAICompatExecutor("openai", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-decisions-compat",
		Provider:   "openai",
		Attributes: map[string]string{"base_url": upstream.server.URL + "/v1/", "api_key": "sk-compat"},
	}
	req, opts := newDecisionsTestRequest()

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if upstream.path != "/v1/decisions" {
		t.Fatalf("upstream path = %q, want /v1/decisions", upstream.path)
	}
	if upstream.auth != "Bearer sk-compat" {
		t.Fatalf("Authorization = %q", upstream.auth)
	}
	if model := gjson.Get(upstream.body, "model").String(); model != "gpt-6-luna" {
		t.Fatalf("upstream model = %q, want gpt-6-luna (alias resolved)", model)
	}
	if gjson.Get(upstream.body, "questions.0.name").String() != "damaged" || gjson.Get(upstream.body, "messages").Exists() {
		t.Fatalf("upstream body was not forwarded unchanged: %s", upstream.body)
	}
	if string(resp.Payload) != decisionsTestResponse {
		t.Fatalf("response payload = %s, want unchanged upstream body", resp.Payload)
	}
}

func TestOpenAICompatExecutorDecisionsAppliesPayloadRulesLast(t *testing.T) {
	upstream := newDecisionsUpstream(t, http.StatusOK, decisionsTestResponse)
	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-6-*", Protocol: "openai-decisions"}},
		Params: map[string]any{"model": "gpt-6-luna-2026-10-01"},
	}}
	cfg.Payload.Filter = []config.PayloadFilterRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-6-*", Protocol: "openai-decisions"}},
		Params: []string{"safety_identifier"},
	}}
	exec := NewOpenAICompatExecutor("openai", cfg)
	auth := &cliproxyauth.Auth{
		ID:         "auth-decisions-rules",
		Provider:   "openai",
		Attributes: map[string]string{"base_url": upstream.server.URL, "api_key": "sk-compat"},
	}
	req, opts := newDecisionsTestRequest()
	req.Payload = []byte(`{"model":"luna","safety_identifier":"user-1","input":"x","questions":[{"type":"predicate","instructions":"i"}]}`)

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if model := gjson.Get(upstream.body, "model").String(); model != "gpt-6-luna-2026-10-01" {
		t.Fatalf("upstream model = %q, want the payload override to win over the built-in model rewrite", model)
	}
	if gjson.Get(upstream.body, "safety_identifier").Exists() {
		t.Fatalf("payload filter rule was not applied: %s", upstream.body)
	}
}

func TestOpenAICompatExecutorDecisionsPreservesUpstreamError(t *testing.T) {
	const errBody = `{"error":{"message":"Decision API is not enabled for this user.","type":"invalid_request_error","param":null,"code":null}}`
	upstream := newDecisionsUpstream(t, http.StatusForbidden, errBody)
	exec := NewOpenAICompatExecutor("openai", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-decisions-error",
		Provider:   "openai",
		Attributes: map[string]string{"base_url": upstream.server.URL, "api_key": "sk-compat"},
	}
	req, opts := newDecisionsTestRequest()

	_, err := exec.Execute(context.Background(), auth, req, opts)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusForbidden {
		t.Fatalf("error = %v, want status 403", err)
	}
	if gjson.Get(err.Error(), "error.message").String() != "Decision API is not enabled for this user." {
		t.Fatalf("error body = %s, want upstream body", err.Error())
	}
}

func TestOpenAICompatExecutorDecisionsPublishesUsage(t *testing.T) {
	const authID = "auth-decisions-usage"
	capture := &decisionsUsageCapture{authID: authID, records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), decisionsUsageNoop{})
	})

	upstream := newDecisionsUpstream(t, http.StatusOK, decisionsTestResponse)
	exec := NewOpenAICompatExecutor("openai", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         authID,
		Provider:   "openai",
		Attributes: map[string]string{"base_url": upstream.server.URL, "api_key": "sk-compat"},
	}
	req, opts := newDecisionsTestRequest()
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case record := <-capture.records:
		if record.Failed {
			t.Fatal("usage record marked failed")
		}
		if record.Detail.InputTokens != 41 || record.Detail.OutputTokens != 3 {
			t.Fatalf("usage = %+v, want input 41 output 3", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for decisions usage record")
	}
}

func TestCodexExecutorDecisionsUsesAPIKeyCredential(t *testing.T) {
	upstream := newDecisionsUpstream(t, http.StatusOK, decisionsTestResponse)
	exec := NewCodexAutoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-decisions-codex-key",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url":   upstream.server.URL + "/v1",
			"api_key":    "sk-codex",
			"websockets": "true",
		},
	}
	req, opts := newDecisionsTestRequest()

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if upstream.path != "/v1/decisions" || upstream.auth != "Bearer sk-codex" {
		t.Fatalf("upstream path = %q auth = %q, want /v1/decisions with the API key", upstream.path, upstream.auth)
	}
	if gjson.Get(upstream.body, "input").String() == "" || gjson.Get(upstream.body, "instructions").Exists() {
		t.Fatalf("decisions body must not be translated to Responses: %s", upstream.body)
	}
	if string(resp.Payload) != decisionsTestResponse {
		t.Fatalf("response payload = %s", resp.Payload)
	}
}

func TestCodexExecutorDecisionsRejectsOAuthCredential(t *testing.T) {
	upstream := newDecisionsUpstream(t, http.StatusOK, decisionsTestResponse)
	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-decisions-codex-oauth",
		Provider:   "codex",
		Attributes: map[string]string{"base_url": upstream.server.URL},
		Metadata:   map[string]any{"access_token": "oauth-token", "refresh_token": "refresh"},
	}
	req, opts := newDecisionsTestRequest()

	_, err := exec.Execute(context.Background(), auth, req, opts)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("error = %v, want 501 for an OAuth credential", err)
	}
	if upstream.path != "" {
		t.Fatalf("OAuth credential reached the upstream at %q", upstream.path)
	}
}

// Endpoint-level failures must not cool down a credential that serves the same
// model for chat; credential and upstream health failures keep normal handling.
func TestOpenAIDecisionsErrorScope(t *testing.T) {
	cases := []struct {
		status        int
		requestScoped bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusMethodNotAllowed, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusNotImplemented, true},
		{http.StatusUnauthorized, false},
		{http.StatusPaymentRequired, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		upstream := newDecisionsUpstream(t, tc.status, `{"error":{"message":"x"}}`)
		for name, exec := range map[string]cliproxyauth.ProviderExecutor{
			"openai-compat": NewOpenAICompatExecutor("openai", &config.Config{}),
			"codex":         NewCodexExecutor(&config.Config{}),
		} {
			auth := &cliproxyauth.Auth{ID: "auth-decisions-scope", Provider: "x",
				Attributes: map[string]string{"base_url": upstream.server.URL, "api_key": "sk"}}
			req, opts := newDecisionsTestRequest()
			_, err := exec.Execute(context.Background(), auth, req, opts)
			if err == nil {
				t.Fatalf("%s %d: error = nil", name, tc.status)
			}
			scoped, _ := err.(cliproxyexecutor.RequestScopedError)
			if got := scoped != nil && scoped.IsRequestScoped(); got != tc.requestScoped {
				t.Fatalf("%s %d: request scoped = %v, want %v (err %v)", name, tc.status, got, tc.requestScoped, err)
			}
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != tc.status {
				t.Fatalf("%s %d: status lost: %v", name, tc.status, err)
			}
			if gjson.Get(err.Error(), "error.message").String() != "x" {
				t.Fatalf("%s %d: body not preserved: %s", name, tc.status, err.Error())
			}
		}
	}
}

func TestCodexExecutorDecisionsOAuthRejectionIsRequestScoped(t *testing.T) {
	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "auth-decisions-oauth-scope", Provider: "codex",
		Metadata: map[string]any{"access_token": "oauth-token", "refresh_token": "refresh"}}
	req, opts := newDecisionsTestRequest()
	_, err := exec.Execute(context.Background(), auth, req, opts)
	scoped, ok := err.(cliproxyexecutor.RequestScopedError)
	if !ok || !scoped.IsRequestScoped() {
		t.Fatalf("OAuth rejection = %v, want a request-scoped error so Home mode does not cool the credential down", err)
	}
}
