package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// commandCodeQuotaTestAuth mirrors the attributes the file synthesizer gives
// a Command Code login: an API key, but OAuth kind.
func commandCodeQuotaTestAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "commandcode",
		Attributes: map[string]string{
			coreauth.AttributeAPIKey:   "user_cc_key",
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
		},
		Metadata: map[string]any{"type": "commandcode", "api_key": "user_cc_key", "email": "a@b.c"},
	}
}

func TestClientUsageAuthsIncludesCommandCodeOAuth(t *testing.T) {
	apiKeyOnly := &coreauth.Auth{ID: "cc-key", Provider: "commandcode", Attributes: map[string]string{coreauth.AttributeAPIKey: "user_cc_key"}}
	auths := clientUsageAuths([]*coreauth.Auth{apiKeyOnly, commandCodeQuotaTestAuth("cc.json")})
	if len(auths) != 1 || auths[0].ID != "cc.json" || clientUsageProvider(auths[0]) != "commandcode" {
		t.Fatalf("auths = %+v", auths)
	}
}

func TestProbeClientUsageCommandCodeReadsCreditsAndSubscription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user_cc_key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/alpha/billing/credits":
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":12.5},"windowLimits":{"fiveHour":{"cap":5,"used":1}}}`))
		case "/alpha/billing/subscriptions":
			_, _ = w.Write([]byte(`{"planId":"individual-pro-v1","userId":"u1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	creditsURL, subscriptionURL := clientUsageCommandCodeCreditsURL, clientUsageCommandCodeSubscriptionURL
	clientUsageCommandCodeCreditsURL = server.URL + "/alpha/billing/credits"
	clientUsageCommandCodeSubscriptionURL = server.URL + "/alpha/billing/subscriptions"
	defer func() {
		clientUsageCommandCodeCreditsURL, clientUsageCommandCodeSubscriptionURL = creditsURL, subscriptionURL
	}()

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	result := h.probeClientUsage(commandCodeQuotaTestAuth("cc.json"))
	if result.err != "" || len(result.responses) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if got := string(result.responses[0].Body); got != `{"credits":{"monthlyCredits":12.5},"windowLimits":{"fiveHour":{"cap":5,"used":1}}}` {
		t.Fatalf("credits body = %s", got)
	}
	if got := string(result.responses[1].Body); got != `{"planId":"individual-pro-v1"}` {
		t.Fatalf("subscription body = %s", got)
	}
}
