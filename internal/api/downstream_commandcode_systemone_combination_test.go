package api

// Downstream combination test owned by the ci branch: it compiles only on the
// integrated tree, where patch/commandcode-oauth (Command Code catalog and
// provider) meets patch/systemone-endpoint (POST /v1/systemone).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestSystemOneRouteServesCommandCodeOAuthCatalog(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"is_urgent":{"type":"noul","noul":0.95}},"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	// The public catalog does not list typesafe/jev; the Command Code catalog
	// parser adds it, and the credential must serve it on /v1/systemone.
	models := commandcodeauth.ParseModels([]byte(`{"data":[{"id":"deepseek/deepseek-v4-pro","supported_endpoints":["/chat/completions"]}]}`), 1)
	var jev *registry.ModelInfo
	for _, model := range models {
		if model.ID == "typesafe/jev" {
			jev = model
		}
	}
	if jev == nil {
		t.Fatalf("command code catalog models = %v, want typesafe/jev", models)
	}

	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewOpenAICompatExecutor(commandcodeauth.ProviderKey, server.cfg))
	credential := &auth.Auth{
		ID:       "commandcode-systemone-route-auth",
		Provider: commandcodeauth.ProviderKey,
		Status:   auth.StatusActive,
		Attributes: map[string]string{
			auth.AttributeAuthKind: auth.AuthKindOAuth,
			auth.AttributeAPIKey:   "user_oauth_key",
			"base_url":             upstream.URL + "/provider/v1",
		},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(credential.ID, commandcodeauth.ProviderKey, models)
	t.Cleanup(func() { modelRegistry.UnregisterClient(credential.ID) })

	body := `{"model":"typesafe/jev","state":"My payouts have been failing for 3 days.","questions":{"is_urgent":{"type":"noul","instructions":"Does this convey urgency?"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/provider/v1/systemone" {
		t.Fatalf("upstream path = %q, want /provider/v1/systemone", gotPath)
	}
	if gotAuth != "Bearer user_oauth_key" {
		t.Fatalf("upstream Authorization = %q", gotAuth)
	}
	if gjson.Get(gotBody, "model").String() != "typesafe/jev" {
		t.Fatalf("upstream body = %s", gotBody)
	}
	if got := gjson.GetBytes(rr.Body.Bytes(), "answers.is_urgent.noul").Float(); got != 0.95 {
		t.Fatalf("response body = %s", rr.Body.String())
	}
}
