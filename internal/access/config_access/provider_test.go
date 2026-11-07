package configaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func authenticateWith(t *testing.T, cfg *sdkconfig.SDKConfig, key string) (*sdkaccess.Result, *sdkaccess.AuthError) {
	t.Helper()
	Register(cfg)
	t.Cleanup(func() { Register(nil) })
	manager := sdkaccess.NewManager()
	manager.SetProviders(sdkaccess.RegisteredProviders())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return manager.Authenticate(context.Background(), req)
}

func TestDisabledClientKeyIsRejected(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{"live-key", "paused-key"},
		ClientKeys: []sdkconfig.ClientKeyConfig{
			{APIKey: "live-key"},
			{APIKey: "paused-key", Disabled: true},
		},
	}

	if res, errAuth := authenticateWith(t, cfg, "live-key"); errAuth != nil || res == nil || res.Principal != "live-key" {
		t.Fatalf("live-key: result=%v err=%v", res, errAuth)
	}
	for _, key := range []string{"paused-key", "unknown-key"} {
		_, errAuth := authenticateWith(t, cfg, key)
		if !sdkaccess.IsAuthErrorCode(errAuth, sdkaccess.AuthErrorCodeInvalidCredential) || errAuth.HTTPStatusCode() != http.StatusUnauthorized {
			t.Fatalf("%s: err=%v, want 401 invalid credential", key, errAuth)
		}
	}
}

// Disabling every key must not unregister the provider: with no provider the
// access manager lets every request through.
func TestAllClientKeysDisabledStillRejects(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{
		APIKeys:    []string{"paused-key"},
		ClientKeys: []sdkconfig.ClientKeyConfig{{APIKey: "paused-key", Disabled: true}},
	}

	res, errAuth := authenticateWith(t, cfg, "paused-key")
	if !sdkaccess.IsAuthErrorCode(errAuth, sdkaccess.AuthErrorCodeInvalidCredential) || res != nil {
		t.Fatalf("all keys disabled: result=%v err=%v, want rejection", res, errAuth)
	}
}
