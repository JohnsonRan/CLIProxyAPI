package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCommandCodeCallbackRedirectURLCarriesAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)

	state := "commandcode-state-test"
	RegisterOAuthSession(state, "commandcode")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	body, _ := json.Marshal(map[string]string{
		"provider":     "commandcode",
		"redirect_url": "http://127.0.0.1:54546/callback?apiKey=user_secret&state=" + state + "&userId=u1&userName=alice&keyName=cli",
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PostOAuthCallback(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body=%s", rec.Code, rec.Body.String())
	}

	apiKey, errWait := waitForCommandCodeCallback(authDir, state)
	if errWait != nil {
		t.Fatalf("waitForCommandCodeCallback: %v", errWait)
	}
	if apiKey != "user_secret" {
		t.Fatalf("api key = %q", apiKey)
	}
	if _, errStat := os.Stat(filepath.Join(authDir, ".oauth-commandcode-"+state+".oauth")); !os.IsNotExist(errStat) {
		t.Fatalf("callback file should be removed after reading, stat err = %v", errStat)
	}
}

func TestCommandCodeAPIKeyIgnoredForOtherProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	state := "codex-state-apikey-test"
	RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	body, _ := json.Marshal(map[string]string{
		"provider":     "codex",
		"redirect_url": "http://localhost:1455/auth/callback?apiKey=user_secret&state=" + state,
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PostOAuthCallback(c)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "code or error is required") {
		t.Fatalf("status = %d body=%s, want 400 code or error is required", rec.Code, rec.Body.String())
	}
}

func TestBuildCommandCodeAuthRecordSavesLoginMetadata(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)
	account := &commandcodeauth.Account{UserID: "u1", UserName: "alice", Email: "alice@example.com", OrgID: "o1"}
	record := buildCommandCodeAuthRecord("user_secret", account, time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
	if record.Provider != "commandcode" || record.Label != "alice@example.com" || record.FileName != "commandcode-alice@example.com.json" {
		t.Fatalf("record = %+v", record)
	}

	savedPath, errSave := h.saveTokenRecord(context.Background(), record)
	if errSave != nil {
		t.Fatalf("saveTokenRecord: %v", errSave)
	}
	data, errRead := os.ReadFile(savedPath)
	if errRead != nil {
		t.Fatalf("read saved file: %v", errRead)
	}
	var saved map[string]any
	if errJSON := json.Unmarshal(data, &saved); errJSON != nil {
		t.Fatalf("parse saved file: %v", errJSON)
	}
	want := map[string]string{
		"type":             "commandcode",
		"api_key":          "user_secret",
		"base_url":         "https://api.commandcode.ai/provider/v1",
		"auth_kind":        "oauth",
		"email":            "alice@example.com",
		"user_id":          "u1",
		"user_name":        "alice",
		"org_id":           "o1",
		"authenticated_at": "2026-10-16T00:00:00Z",
	}
	for key, value := range want {
		if got, _ := saved[key].(string); got != value {
			t.Errorf("saved[%q] = %q, want %q", key, got, value)
		}
	}
	if _, ok := saved["prefix"]; ok {
		t.Errorf("saved file should not force a model prefix, got %v", saved["prefix"])
	}
}
