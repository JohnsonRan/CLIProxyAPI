package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The panel toggles a client key by writing `disabled: true` onto its
// access.api-keys entry; the flag must round-trip and reach the loaded config.
func TestConfigV8ClientKeyDisabledRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nserver: {port: 8317}\naccess: {api-keys: [live-key]}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.Handle(http.MethodGet, "/v8/management/config/*path", h.ConfigV8)
	router.Handle(http.MethodPut, "/v8/management/config/*path", h.ConfigV8)

	put := httptest.NewRecorder()
	router.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/v8/management/config/access/api-keys",
		strings.NewReader(`["live-key",{"api-key":"paused-key","name":"Bob","disabled":true}]`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v8/management/config/access/api-keys", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	if !strings.Contains(get.Body.String(), `"disabled":true`) {
		t.Fatalf("GET lost the disabled flag: %s", get.Body.String())
	}
	var probe any
	if errJSON := json.Unmarshal(get.Body.Bytes(), &probe); errJSON != nil {
		t.Fatalf("GET body is not JSON: %v", errJSON)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded.APIKeys, ","); got != "live-key,paused-key" {
		t.Fatalf("APIKeys=%q", got)
	}
	disabled := map[string]bool{}
	for _, entry := range loaded.ClientKeys {
		disabled[entry.APIKey] = entry.Disabled
	}
	if !disabled["paused-key"] || disabled["live-key"] {
		t.Fatalf("disabled flags=%v", disabled)
	}
}
