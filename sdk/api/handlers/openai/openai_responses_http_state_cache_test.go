package openai

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ginContextWithClientKey(provider, key string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("accessProvider", provider)
	c.Set("userApiKey", key)
	return c
}

func TestResponsesHTTPStateCacheOnlyServesTheCreatingCaller(t *testing.T) {
	cache := newResponsesHTTPStateCache(0, 0, 0)
	owner := responsesHTTPStateOwner(ginContextWithClientKey("config-api-key", "key-a"))
	cache.put("resp_1", responsesHTTPStateEntry{owner: owner, request: []byte(`{"instructions":"secret"}`), authID: "auth-a"})

	if _, ok := cache.get("resp_1", responsesHTTPStateOwner(ginContextWithClientKey("config-api-key", "key-b"))); ok {
		t.Fatal("another client key continued a response it did not create")
	}
	if _, ok := cache.get("resp_1", responsesHTTPStateOwner(ginContextWithClientKey("other-provider", "key-a"))); ok {
		t.Fatal("the same key from another access provider continued the response")
	}
	entry, ok := cache.get("resp_1", owner)
	if !ok || entry.authID != "auth-a" {
		t.Fatalf("creating caller lost access: ok=%v entry=%+v", ok, entry)
	}
}

func TestResponsesHTTPStateCacheBoundsRetainedBytes(t *testing.T) {
	cache := newResponsesHTTPStateCache(0, 100, 1000)
	payloadFor := func(fill string) []byte { return bytes.Repeat([]byte(fill), 300) }
	payload := payloadFor("d")
	for i, id := range []string{"resp_1", "resp_2", "resp_3", "resp_4"} {
		// Distinct contents: identical ones would be stored once.
		cache.put(id, responsesHTTPStateEntry{request: payloadFor(string(rune('a' + i)))})
	}
	if cache.totalBytes > 1000 {
		t.Fatalf("totalBytes = %d, want <= 1000", cache.totalBytes)
	}
	if _, ok := cache.get("resp_1", ""); ok {
		t.Fatal("oldest entry should have been evicted to honour the byte cap")
	}
	if _, ok := cache.get("resp_4", ""); !ok {
		t.Fatal("newest entry should be retained")
	}

	cache.put("resp_huge", responsesHTTPStateEntry{request: bytes.Repeat([]byte("x"), 2000)})
	if _, ok := cache.get("resp_huge", ""); ok {
		t.Fatal("an entry larger than the whole cap must not be cached")
	}

	// Replacing an entry must not double count it.
	before := cache.totalBytes
	cache.put("resp_4", responsesHTTPStateEntry{request: payload})
	if cache.totalBytes != before {
		t.Fatalf("totalBytes after replace = %d, want %d", cache.totalBytes, before)
	}
	if len(cache.order) != len(cache.entries) {
		t.Fatalf("order has %d keys for %d entries", len(cache.order), len(cache.entries))
	}
}
