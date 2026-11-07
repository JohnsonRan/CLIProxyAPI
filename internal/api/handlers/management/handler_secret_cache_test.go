package management

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func newBcryptHandler(t *testing.T, secret string) *Handler {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	return &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo)}
}

func TestAuthenticateManagementKey_BcryptResultIsCached(t *testing.T) {
	h := newBcryptHandler(t, "correct-secret")

	if h.verifiedKey.Load() != nil {
		t.Fatal("verified key cache must start empty")
	}
	allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
	if allowed {
		t.Fatal("wrong key accepted")
	}
	if h.verifiedKey.Load() != nil {
		t.Fatal("failed bcrypt must not populate the cache")
	}

	allowed, _, _ = h.AuthenticateManagementKey("127.0.0.1", true, "correct-secret")
	if !allowed {
		t.Fatal("correct key rejected")
	}
	cached := h.verifiedKey.Load()
	if cached == nil || cached.secretHash != h.cfg.RemoteManagement.SecretKey {
		t.Fatal("successful bcrypt must populate the cache for the current hash")
	}

	// Cached path: the correct key passes, a different key still fails.
	if allowed, _, _ = h.AuthenticateManagementKey("127.0.0.1", true, "correct-secret"); !allowed {
		t.Fatal("cached correct key rejected")
	}
	allowed, status, _ := h.AuthenticateManagementKey("127.0.0.1", true, "correct-secret-not")
	if allowed || status != http.StatusUnauthorized {
		t.Fatalf("wrong key accepted through the cache (allowed=%v status=%d)", allowed, status)
	}
}

func TestAuthenticateManagementKey_CacheInvalidatedWhenSecretChanges(t *testing.T) {
	h := newBcryptHandler(t, "old-secret")
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "old-secret"); !allowed {
		t.Fatal("old secret rejected")
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte("new-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	h.cfg.RemoteManagement.SecretKey = string(newHash)

	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "old-secret"); allowed {
		t.Fatal("old secret accepted after the hash changed")
	}
	if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "new-secret"); !allowed {
		t.Fatal("new secret rejected")
	}
}

func BenchmarkAuthenticateManagementKey_Cached(b *testing.B) {
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.DefaultCost)
	if err != nil {
		b.Fatalf("bcrypt: %v", err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	h := &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo)}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if allowed, _, _ := h.AuthenticateManagementKey("127.0.0.1", true, "secret"); !allowed {
			b.Fatal("rejected")
		}
	}
}
