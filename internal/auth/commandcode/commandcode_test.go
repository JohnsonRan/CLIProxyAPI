package commandcode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildAuthURL(t *testing.T) {
	raw := BuildAuthURL(54546, "state-1")
	u, errParse := url.Parse(raw)
	if errParse != nil {
		t.Fatalf("parse auth url: %v", errParse)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://commandcode.ai/studio/auth/cli" {
		t.Fatalf("auth endpoint = %q", got)
	}
	q := u.Query()
	if q.Get("callback") != "http://127.0.0.1:54546/callback" || q.Get("state") != "state-1" || q.Get("mode") != "redirect" {
		t.Fatalf("auth query = %v", q)
	}
}

func TestParseModelsKeepsChatCompletionModels(t *testing.T) {
	body := []byte(`{"object":"list","data":[
		{"id":"claude-sonnet-5-5","created":10,"name":"Claude","context_length":1000000,"supported_endpoints":["/messages"]},
		{"id":"deepseek/deepseek-v4-pro","created":11,"name":"DeepSeek V4 Pro","context_length":1000000,"supported_endpoints":["/chat/completions","/responses"]},
		{"id":"z-ai/glm-5.3-flash","supported_endpoints":["/chat/completions"]},
		{"id":"no-endpoints-listed"},
		{"id":"deepseek/deepseek-v4-pro","supported_endpoints":["/chat/completions"]},
		{"id":""}
	]}`)
	models := ParseModels(body, 99)
	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
		if !model.UserDefined || model.OwnedBy != ProviderKey || model.Type != ProviderKey {
			t.Errorf("model %q = %+v, want user-defined commandcode model", model.ID, model)
		}
	}
	if got := strings.Join(ids, ","); got != "deepseek/deepseek-v4-pro,z-ai/glm-5.3-flash,no-endpoints-listed" {
		t.Fatalf("model ids = %s", got)
	}
	if models[0].DisplayName != "DeepSeek V4 Pro" || models[0].ContextLength != 1000000 || models[0].Created != 11 {
		t.Fatalf("first model = %+v", models[0])
	}
	if models[1].Created != 99 {
		t.Fatalf("missing created should default to now, got %d", models[1].Created)
	}
}

func TestFetchAccount(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer user_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user":{"id":"u1","userName":"alice","email":"alice@example.com"},"org":{"id":"o1"}}`))
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteHost{target: server.URL, base: client.Transport}

	account, errFetch := FetchAccount(context.Background(), client, "user_good")
	if errFetch != nil {
		t.Fatalf("FetchAccount: %v", errFetch)
	}
	if gotAuth != "Bearer user_good" || gotPath != "/alpha/whoami" {
		t.Fatalf("request auth=%q path=%q", gotAuth, gotPath)
	}
	if account.UserID != "u1" || account.UserName != "alice" || account.Email != "alice@example.com" || account.OrgID != "o1" {
		t.Fatalf("account = %+v", account)
	}
	if got := CredentialFileName(account); got != "commandcode-alice@example.com.json" {
		t.Fatalf("file name = %q", got)
	}

	if _, errBad := FetchAccount(context.Background(), client, "user_bad"); !errors.Is(errBad, ErrInvalidAPIKey) {
		t.Fatalf("bad key error = %v, want ErrInvalidAPIKey", errBad)
	}
}

func TestParseAccountFallsBackToName(t *testing.T) {
	account := parseAccount([]byte(`{"user":{"id":"u2","name":"Bob B"}}`))
	if account.UserName != "Bob B" || account.Label() != "Bob B" {
		t.Fatalf("account = %+v", account)
	}
	if got := CredentialFileName(account); got != "commandcode-Bob-B.json" {
		t.Fatalf("file name = %q", got)
	}
}

// rewriteHost sends every request to the test server.
type rewriteHost struct {
	target string
	base   http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.target)
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
