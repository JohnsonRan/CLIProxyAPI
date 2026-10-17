package openai

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// conversationTurn builds the canonical request and output of turn n of a
// linear conversation, the way HTTP continuation expands it: every turn's
// input is the previous input, the previous output and the new items.
func conversationTurns(turns int, itemSize int) (requests [][]byte, outputs [][]byte) {
	tools := `"tools":[{"type":"function","name":"shell","parameters":{"type":"object","description":"` + strings.Repeat("t", 4096) + `"}}]`
	var history []string
	item := func(kind string, n int) string {
		return fmt.Sprintf(`{"type":"message","role":%q,"content":[{"type":"input_text","text":"%d:%s"}]}`, kind, n, strings.Repeat("x", itemSize))
	}
	for n := 0; n < turns; n++ {
		history = append(history, item("user", n))
		request := `{"model":"gpt-6",` + tools + `,"input":[` + strings.Join(history, ",") + `],"stream":true}`
		output := `[` + item("assistant", n) + `]`
		requests = append(requests, []byte(request))
		outputs = append(outputs, []byte(output))
		history = append(history, item("assistant", n))
	}
	return requests, outputs
}

func TestResponsesHTTPStateCacheRoundTripsEntries(t *testing.T) {
	cache := newResponsesHTTPStateCache(0, 0, 0)
	cases := map[string]responsesHTTPStateEntry{
		"compact":        {request: []byte(`{"model":"m","input":[{"a":1},{"b":[1,2]}],"stream":true}`), responseOutput: []byte(`[{"type":"message"}]`)},
		"empty input":    {request: []byte(`{"model":"m","input":[]}`), responseOutput: []byte(`[]`)},
		"no input":       {request: []byte(`{"model":"m"}`)},
		"input first":    {request: []byte(`{"input":[{"a":1}],"model":"m"}`)},
		"not json":       {request: []byte(`xxxx`), responseOutput: []byte(`yyyy`)},
		"object output":  {request: []byte(`{"input":[1]}`), responseOutput: []byte(`{"not":"an array"}`)},
		"string input":   {request: []byte(`{"input":"hello"}`)},
		"nested inputs":  {request: []byte(`{"meta":{"input":[9]},"input":[{"input":[1]}]}`)},
		"tool call ids":  {request: []byte(`{"input":[1]}`), pendingToolCallIDs: []string{"call_1", "call_2"}, authID: "auth-a"},
		"spaced output":  {request: []byte(`{"input":[1]}`), responseOutput: []byte(` [1] `)},
		"whitespace obj": {request: []byte("{ \"input\" : [ 1 , 2 ] }")},
	}
	for name, entry := range cases {
		cache.put(name, entry)
		got, ok := cache.get(name, "")
		if !ok {
			t.Fatalf("%s: entry missing", name)
		}
		wantRequest := entry.request
		if name == "whitespace obj" {
			// Elements of a non-compact array are rejoined compactly.
			wantRequest = []byte("{ \"input\" : [1,2] }")
		}
		if !bytes.Equal(got.request, wantRequest) {
			t.Fatalf("%s: request = %q, want %q", name, got.request, wantRequest)
		}
		if !bytes.Equal(got.responseOutput, entry.responseOutput) {
			t.Fatalf("%s: output = %q, want %q", name, got.responseOutput, entry.responseOutput)
		}
		if strings.Join(got.pendingToolCallIDs, ",") != strings.Join(entry.pendingToolCallIDs, ",") || got.authID != entry.authID {
			t.Fatalf("%s: metadata = %+v, want %+v", name, got, entry)
		}
	}
}

func TestResponsesHTTPStateCacheReturnsCallerOwnedBuffers(t *testing.T) {
	cache := newResponsesHTTPStateCache(0, 0, 0)
	request := []byte(`{"input":[{"a":1}],"model":"m"}`)
	output := []byte(`[{"b":2}]`)
	cache.put("resp_1", responsesHTTPStateEntry{request: request, responseOutput: output})
	// Mutating the caller's buffers after put must not reach the cache.
	copy(request, bytes.Repeat([]byte("z"), len(request)))
	copy(output, bytes.Repeat([]byte("z"), len(output)))

	first, _ := cache.get("resp_1", "")
	first.request[0] = 'z'
	second, _ := cache.get("resp_1", "")
	if string(second.request) != `{"input":[{"a":1}],"model":"m"}` || string(second.responseOutput) != `[{"b":2}]` {
		t.Fatalf("cached entry was modified: %q %q", second.request, second.responseOutput)
	}
}

func TestResponsesHTTPStateCacheSharesHistoryAcrossTurns(t *testing.T) {
	const turns, itemSize = 40, 2048
	requests, outputs := conversationTurns(turns, itemSize)
	cache := newResponsesHTTPStateCache(0, 0, 0)
	logical := 0
	for n := range requests {
		cache.put(fmt.Sprintf("resp_%d", n), responsesHTTPStateEntry{request: requests[n], responseOutput: outputs[n]})
		logical += len(requests[n]) + len(outputs[n])
	}
	for n := range requests {
		got, ok := cache.get(fmt.Sprintf("resp_%d", n), "")
		if !ok || !bytes.Equal(got.request, requests[n]) || !bytes.Equal(got.responseOutput, outputs[n]) {
			t.Fatalf("turn %d did not round-trip", n)
		}
	}
	// Every distinct item is kept once: 2 items per turn plus one skeleton.
	unique := 0
	for _, chunk := range cache.chunks {
		unique += len(chunk.data)
	}
	if unique > 2*turns*(itemSize+128)+8192 {
		t.Fatalf("retained %d unique bytes, want about %d", unique, 2*turns*itemSize)
	}
	if cache.totalBytes >= logical/10 {
		t.Fatalf("retained %d bytes for %d logical bytes, want sharing", cache.totalBytes, logical)
	}
	t.Logf("logical %d bytes, retained %d bytes", logical, cache.totalBytes)
}

func TestResponsesHTTPStateCacheReleasesSharedChunks(t *testing.T) {
	requests, outputs := conversationTurns(10, 256)
	cache := newResponsesHTTPStateCache(0, 3, 0)
	for n := range requests {
		cache.put(fmt.Sprintf("resp_%d", n), responsesHTTPStateEntry{request: requests[n], responseOutput: outputs[n]})
	}
	if len(cache.entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(cache.entries))
	}
	// The oldest kept turn still needs its whole history.
	got, ok := cache.get("resp_7", "")
	if !ok || !bytes.Equal(got.request, requests[7]) {
		t.Fatal("eviction of older turns dropped history a newer turn shares")
	}

	for _, id := range []string{"resp_7", "resp_8", "resp_9"} {
		cache.removeLocked(id, cache.entries[id])
	}
	if cache.totalBytes != 0 || len(cache.chunks) != 0 || len(cache.order) != 0 {
		t.Fatalf("after removing every entry: totalBytes=%d chunks=%d order=%d", cache.totalBytes, len(cache.chunks), len(cache.order))
	}
}

func TestResponsesHTTPStateCacheKeepsCollidingChunksApart(t *testing.T) {
	cache := newResponsesHTTPStateCache(0, 0, 0)
	cache.put("resp_1", responsesHTTPStateEntry{request: []byte(`{"input":[{"a":1}]}`)})
	// Force the next item onto an occupied hash slot with different content.
	var occupied *responsesHTTPStateChunk
	for _, chunk := range cache.chunks {
		if chunk.data == `{"a":1}` {
			occupied = chunk
		}
	}
	collider := cache.internLocked(`{"b":2}`, false)
	cache.releaseChunkLocked(collider)
	delete(cache.chunks, occupied.hash)
	cache.chunks[collider.hash] = occupied
	occupied.hash = collider.hash

	cache.put("resp_2", responsesHTTPStateEntry{request: []byte(`{"input":[{"b":2}]}`)})
	got, _ := cache.get("resp_2", "")
	if string(got.request) != `{"input":[{"b":2}]}` {
		t.Fatalf("colliding chunk returned the wrong content: %q", got.request)
	}
	got, _ = cache.get("resp_1", "")
	if string(got.request) != `{"input":[{"a":1}]}` {
		t.Fatalf("existing chunk was overwritten: %q", got.request)
	}
	cache.removeLocked("resp_2", cache.entries["resp_2"])
	if cache.chunks[occupied.hash] != occupied {
		t.Fatal("releasing a private chunk removed the interned one")
	}
}

func BenchmarkResponsesHTTPStateCachePutGet(b *testing.B) {
	requests, outputs := conversationTurns(40, 2048)
	b.ReportAllocs()
	for b.Loop() {
		cache := newResponsesHTTPStateCache(0, 0, 0)
		for n := range requests {
			id := fmt.Sprintf("resp_%d", n)
			cache.put(id, responsesHTTPStateEntry{request: requests[n], responseOutput: outputs[n]})
			cache.get(id, "")
		}
	}
}

// Concurrent conversations share chunks (the same tools skeleton and, between
// turns, the same items) under one lock; run with -race.
func TestResponsesHTTPStateCacheConcurrentConversations(t *testing.T) {
	requests, outputs := conversationTurns(12, 128)
	cache := newResponsesHTTPStateCache(0, 64, 1<<20)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := range requests {
				id := fmt.Sprintf("resp_%d_%d", worker, n)
				cache.put(id, responsesHTTPStateEntry{request: requests[n], responseOutput: outputs[n]})
				if got, ok := cache.get(id, ""); ok && !bytes.Equal(got.request, requests[n]) {
					t.Errorf("worker %d turn %d: request did not round-trip", worker, n)
				}
			}
		}(worker)
	}
	wg.Wait()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	for id, stored := range cache.entries {
		cache.removeLocked(id, stored)
	}
	if cache.totalBytes != 0 || len(cache.chunks) != 0 {
		t.Fatalf("after removing every entry: totalBytes=%d chunks=%d", cache.totalBytes, len(cache.chunks))
	}
}
