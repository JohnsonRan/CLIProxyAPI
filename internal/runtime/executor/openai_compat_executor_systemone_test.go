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

type systemOneUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *systemOneUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c == nil || record.AuthID != c.authID {
		return
	}
	select {
	case c.records <- record:
	default:
	}
}

type systemOneUsageNoop struct{}

func (systemOneUsageNoop) HandleUsage(context.Context, usage.Record) {}

const systemOneTestResponse = `{"model":"jev-1.13.0","answers":{"is_urgent":{"type":"noul","noul":0.95}},"usage":{"input_tokens":296,"output_tokens":20}}`

func newSystemOneTestRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	body := []byte(`{"model":"jev","state":"Help! My payouts have been failing for 3 days.","questions":{"is_urgent":{"type":"noul","instructions":"Does this convey urgency?"}}}`)
	req := cliproxyexecutor.Request{Model: "jev-latest", Payload: body}
	opts := cliproxyexecutor.Options{
		OriginalRequest: body,
		SourceFormat:    sdktranslator.FromString("systemone"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:    "/v1/systemone",
			cliproxyexecutor.RequestedModelMetadataKey: "jev",
		},
	}
	return req, opts
}

func TestOpenAICompatExecutorSystemOnePassthrough(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(systemOneTestResponse))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("typesafe", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-systemone",
		Provider:   "typesafe",
		Attributes: map[string]string{"base_url": server.URL + "/v1/", "api_key": "ts-secret"},
	}
	req, opts := newSystemOneTestRequest()

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Fatalf("upstream path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer ts-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if model := gjson.Get(gotBody, "model").String(); model != "jev-latest" {
		t.Fatalf("upstream model = %q, want jev-latest (alias resolved)", model)
	}
	if gjson.Get(gotBody, "questions.is_urgent.type").String() != "noul" || !gjson.Get(gotBody, "state").Exists() {
		t.Fatalf("upstream body lost System One fields: %s", gotBody)
	}
	if gjson.Get(gotBody, "messages").Exists() {
		t.Fatalf("upstream body was translated to chat: %s", gotBody)
	}
	if string(resp.Payload) != systemOneTestResponse {
		t.Fatalf("response payload = %s, want unchanged upstream body", resp.Payload)
	}
}

func TestOpenAICompatExecutorSystemOneAppliesPayloadRulesLast(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(systemOneTestResponse))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "jev-*", Protocol: "systemone"}},
		Params: map[string]any{"model": "jev-1.13"},
	}}
	cfg.Payload.Filter = []config.PayloadFilterRule{{
		Models: []config.PayloadModelRule{{Name: "jev-*", Protocol: "systemone"}},
		Params: []string{"metadata"},
	}}
	exec := NewOpenAICompatExecutor("typesafe", cfg)
	auth := &cliproxyauth.Auth{
		ID:         "auth-systemone-rules",
		Provider:   "typesafe",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "ts-secret"},
	}
	req, opts := newSystemOneTestRequest()
	req.Payload = []byte(`{"model":"jev","metadata":{"trace":"x"},"state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`)

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if model := gjson.Get(gotBody, "model").String(); model != "jev-1.13" {
		t.Fatalf("upstream model = %q, want payload override jev-1.13 to win over built-in model rewrite", model)
	}
	if gjson.Get(gotBody, "metadata").Exists() {
		t.Fatalf("payload filter rule was not applied: %s", gotBody)
	}
}

func TestOpenAICompatExecutorSystemOnePreservesUpstreamError(t *testing.T) {
	const errBody = `{"detail":[{"loc":["body","questions"],"msg":"field required"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(errBody))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("typesafe", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "auth-systemone-error",
		Provider:   "typesafe",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "ts-secret"},
	}
	req, opts := newSystemOneTestRequest()

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 422")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want status 422", err)
	}
}

func TestOpenAICompatExecutorSystemOnePublishesUsage(t *testing.T) {
	const authID = "auth-systemone-usage"
	capture := &systemOneUsageCapture{authID: authID, records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), systemOneUsageNoop{})
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(systemOneTestResponse))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("typesafe", &config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         authID,
		Provider:   "typesafe",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "ts-secret"},
	}
	req, opts := newSystemOneTestRequest()
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case record := <-capture.records:
		if record.Failed {
			t.Fatal("usage record marked failed")
		}
		if record.Detail.InputTokens != 296 || record.Detail.OutputTokens != 20 {
			t.Fatalf("usage = %+v, want input 296 output 20", record.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for systemone usage record")
	}
}
