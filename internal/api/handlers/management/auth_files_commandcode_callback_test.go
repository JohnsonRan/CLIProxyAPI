package management

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCommandCodeCallbackHandlerAcceptsStudioFormPost(t *testing.T) {
	authDir := t.TempDir()
	state := "commandcode-form-state"
	RegisterOAuthSession(state, "commandcode")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	form := url.Values{"apiKey": {"user_secret"}, "state": {state}, "userId": {"u1"}, "userName": {"alice"}, "keyName": {"cli"}}
	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	commandCodeCallbackHandler(authDir).ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s, want 303", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/callback/complete?state="+state {
		t.Fatalf("Location = %q", location)
	}
	apiKey, errWait := waitForCommandCodeCallback(authDir, state)
	if errWait != nil || apiKey != "user_secret" {
		t.Fatalf("waitForCommandCodeCallback = %q, %v", apiKey, errWait)
	}

	complete := httptest.NewRecorder()
	commandCodeCallbackHandler(authDir).ServeHTTP(complete, httptest.NewRequest(http.MethodGet, "/callback/complete?state="+state, nil))
	if complete.Code != http.StatusOK || !strings.Contains(complete.Body.String(), "Authentication successful") {
		t.Fatalf("complete status = %d body=%s", complete.Code, complete.Body.String())
	}
}

func TestCommandCodeCallbackHandlerAcceptsJSONPost(t *testing.T) {
	authDir := t.TempDir()
	state := "commandcode-json-state"
	RegisterOAuthSession(state, "commandcode")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	body := `{"apiKey":"user_secret","state":"` + state + `","userId":"u1","userName":"alice","keyName":"cli"}`
	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://commandcode.ai")
	rec := httptest.NewRecorder()
	commandCodeCallbackHandler(authDir).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "https://commandcode.ai" {
		t.Fatalf("Access-Control-Allow-Origin = %q", origin)
	}
	apiKey, errWait := waitForCommandCodeCallback(authDir, state)
	if errWait != nil || apiKey != "user_secret" {
		t.Fatalf("waitForCommandCodeCallback = %q, %v", apiKey, errWait)
	}
}

func TestCommandCodeCallbackHandlerAnswersPrivateNetworkPreflight(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/callback", nil)
	req.Header.Set("Origin", "https://commandcode.ai")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	commandCodeCallbackHandler(t.TempDir()).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("status = %d headers=%v", rec.Code, rec.Header())
	}
}

func TestCommandCodeCallbackHandlerRejectsUnknownState(t *testing.T) {
	form := url.Values{"apiKey": {"user_secret"}, "state": {"commandcode-unknown-state"}}
	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	commandCodeCallbackHandler(t.TempDir()).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
}
