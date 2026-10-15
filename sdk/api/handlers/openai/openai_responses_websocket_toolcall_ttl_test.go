package openai

import (
	"encoding/json"
	"testing"
	"time"
)

// The idle TTL must not drop the tool cache of a WebSocket that is still
// connected; connected sessions are removed on disconnect instead.
func TestWebsocketToolOutputCacheTTLKeepsActiveSessions(t *testing.T) {
	refs := newWebsocketToolSessionRefCounter()
	cache := newWebsocketToolOutputCache(time.Hour, 8)
	cache.isActive = refs.active

	refs.acquire("connected")
	cache.record("connected", "call-1", json.RawMessage(`{"type":"function_call_output"}`))
	cache.record("idle", "call-2", json.RawMessage(`{"type":"function_call_output"}`))

	cache.mu.Lock()
	cache.cleanupLocked(time.Now().Add(2 * time.Hour))
	_, connectedKept := cache.sessions["connected"]
	_, idleKept := cache.sessions["idle"]
	cache.mu.Unlock()

	if !connectedKept {
		t.Fatal("TTL dropped the session of a connected WebSocket")
	}
	if idleKept {
		t.Fatal("TTL kept an idle session without a connected WebSocket")
	}
}
