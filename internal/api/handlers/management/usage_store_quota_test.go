package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityQuotaReportFallsBackAcrossEndpoints(t *testing.T) {
	var gotAuth, gotBody string
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"groups":[]}`))
	}))
	defer working.Close()
	saved := usageStoreAntigravityQuotaURLs
	usageStoreAntigravityQuotaURLs = []string{failing.URL, working.URL}
	defer func() { usageStoreAntigravityQuotaURLs = saved }()

	h := &Handler{}
	auth := &coreauth.Auth{ID: "ag1", Provider: "antigravity", Metadata: map[string]any{
		"access_token": "tok",
		"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		"project_id":   "proj-1",
	}}
	body, err := h.AntigravityQuotaReport(context.Background(), auth)
	if err != nil || string(body) != `{"groups":[]}` {
		t.Fatalf("report = %q, %v", body, err)
	}
	if gotAuth != "Bearer tok" || gotBody != `{"project":"proj-1"}` {
		t.Fatalf("request auth %q body %q", gotAuth, gotBody)
	}

	delete(auth.Metadata, "project_id")
	if _, err = h.AntigravityQuotaReport(context.Background(), auth); err == nil {
		t.Fatal("missing project ID must fail before any request")
	}
}
