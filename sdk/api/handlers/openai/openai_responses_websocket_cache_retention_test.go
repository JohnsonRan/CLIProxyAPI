package openai

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDefaultWebsocketToolCachesExpireIdleSessions(t *testing.T) {
	for name, defaults := range map[string]*websocketToolOutputCache{
		"output": defaultWebsocketToolOutputCache,
		"call":   defaultWebsocketToolCallCache,
	} {
		t.Run(name, func(t *testing.T) {
			if defaults.ttl != time.Hour {
				t.Fatalf("default %s cache ttl = %v, want %v", name, defaults.ttl, time.Hour)
			}
			cache := newWebsocketToolOutputCache(defaults.ttl, defaults.maxPerSession)
			item := json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"synthetic"}`)
			cache.record("idle", "call-1", item)
			cache.record("active", "call-1", item)

			cache.mu.Lock()
			cache.sessions["idle"].lastSeen = time.Now().Add(-time.Hour - time.Minute)
			cache.mu.Unlock()

			cache.record("heartbeat", "call-2", item)
			if _, ok := cache.get("idle", "call-1"); ok {
				t.Error("session idle for more than one hour still retains its tool item")
			}
			if got, ok := cache.get("active", "call-1"); !ok || string(got) != string(item) {
				t.Fatal("recent replay item was not preserved")
			}
		})
	}
}
