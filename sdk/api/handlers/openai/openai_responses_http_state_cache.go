package openai

import (
	"bytes"
	"hash/maphash"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
)

// responsesHTTPStateEntry is the continuation state of one HTTP Responses
// turn. Values passed to put and returned by get are owned by the caller.
type responsesHTTPStateEntry struct {
	// owner identifies the client credential that created the response; only
	// the same caller may continue it.
	owner              string
	request            []byte
	responseOutput     []byte
	pendingToolCallIDs []string
	authID             string
	updatedAt          time.Time
}

func (e responsesHTTPStateEntry) size() int {
	size := len(e.owner) + len(e.request) + len(e.responseOutput) + len(e.authID)
	for _, id := range e.pendingToolCallIDs {
		size += len(id)
	}
	return size
}

// responsesHTTPStateChunk is an immutable byte run shared by every cached
// entry that contains it.
type responsesHTTPStateChunk struct {
	data     string
	hash     uint64
	refs     int
	interned bool
}

// responsesHTTPStateJSON is a cached JSON value. Arrays are kept as their
// elements so that items repeated across turns are stored once; anything else
// is a single opaque chunk.
type responsesHTTPStateJSON struct {
	array bool
	parts []*responsesHTTPStateChunk
}

// responsesHTTPStateStored is the cached form of a responsesHTTPStateEntry.
// Every turn of a conversation repeats the whole history: turn N's request
// input is turn N-1's input, its output, and the new items. Storing each turn
// verbatim made a conversation cost O(turns^2) bytes, so the input and output
// items and the request without its input are interned by content instead.
type responsesHTTPStateStored struct {
	owner string
	// skeleton is the request with its input array removed; the input is
	// spliced back in at inputOffset. With inputOffset < 0 the skeleton is the
	// whole request.
	skeleton           *responsesHTTPStateChunk
	inputOffset        int
	input              responsesHTTPStateJSON
	output             responsesHTTPStateJSON
	pendingToolCallIDs []string
	authID             string
	updatedAt          time.Time
}

type responsesHTTPStateCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	// maxBytes bounds the bytes actually retained: each shared chunk counts
	// once, plus a per-entry overhead.
	maxBytes   int
	totalBytes int
	nowFunc    func() time.Time
	seed       maphash.Seed
	chunks     map[uint64]*responsesHTTPStateChunk
	entries    map[string]*responsesHTTPStateStored
	order      []string
}

var defaultResponsesHTTPStateCache = newResponsesHTTPStateCache(responsesHTTPStateCacheTTL, responsesHTTPStateCacheMaxEntries, responsesHTTPStateCacheMaxBytes)

func newResponsesHTTPStateCache(ttl time.Duration, maxEntries int, maxBytes int) *responsesHTTPStateCache {
	if ttl <= 0 {
		ttl = responsesHTTPStateCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = responsesHTTPStateCacheMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = responsesHTTPStateCacheMaxBytes
	}
	return &responsesHTTPStateCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		nowFunc:    time.Now,
		seed:       maphash.MakeSeed(),
		chunks:     make(map[uint64]*responsesHTTPStateChunk),
		entries:    make(map[string]*responsesHTTPStateStored),
	}
}

func (c *responsesHTTPStateCache) now() time.Time {
	if c != nil && c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

// responsesHTTPStateRefSize approximates the bookkeeping cost of one chunk
// reference, so entries made of many small shared items still count.
const responsesHTTPStateRefSize = 16

func (s *responsesHTTPStateStored) overhead() int {
	size := len(s.owner) + len(s.authID) + responsesHTTPStateRefSize*(1+len(s.input.parts)+len(s.output.parts))
	for _, id := range s.pendingToolCallIDs {
		size += len(id)
	}
	return size
}

func (c *responsesHTTPStateCache) put(responseID string, entry responsesHTTPStateEntry) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" || len(entry.request) == 0 {
		return
	}
	now := c.now()

	// Hashing and splitting read the caller's buffers only; chunks copy the
	// bytes they keep.
	skeleton, inputOffset, inputRaw := splitResponsesHTTPStateRequest(entry.request)
	inputParts := splitResponsesHTTPStateJSON(inputRaw)
	outputParts := splitResponsesHTTPStateJSON(entry.responseOutput)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)
	if previous, exists := c.entries[responseID]; exists {
		c.removeLocked(responseID, previous)
	}
	if entry.size() > c.maxBytes {
		return
	}
	stored := &responsesHTTPStateStored{
		owner:              entry.owner,
		skeleton:           c.internLocked(skeleton.data, skeleton.owned),
		inputOffset:        inputOffset,
		input:              c.internJSONLocked(inputParts),
		output:             c.internJSONLocked(outputParts),
		pendingToolCallIDs: append([]string(nil), entry.pendingToolCallIDs...),
		authID:             entry.authID,
		updatedAt:          now,
	}
	c.order = append(c.order, responseID)
	c.entries[responseID] = stored
	c.totalBytes += stored.overhead()

	for len(c.order) > 0 && (len(c.order) > c.maxEntries || c.totalBytes > c.maxBytes) {
		evict := c.order[0]
		c.order[0] = ""
		c.order = c.order[1:]
		if evicted, ok := c.entries[evict]; ok {
			delete(c.entries, evict)
			c.releaseLocked(evicted)
		}
	}
}

// removeLocked drops responseID from the entries, the byte total, and the order.
func (c *responsesHTTPStateCache) removeLocked(responseID string, stored *responsesHTTPStateStored) {
	delete(c.entries, responseID)
	c.releaseLocked(stored)
	for i, key := range c.order {
		if key == responseID {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *responsesHTTPStateCache) get(responseID string, owner string) (responsesHTTPStateEntry, bool) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return responsesHTTPStateEntry{}, false
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)
	stored, ok := c.entries[responseID]
	if !ok || stored.owner != owner {
		return responsesHTTPStateEntry{}, false
	}
	stored.updatedAt = now
	return responsesHTTPStateEntry{
		owner:              stored.owner,
		request:            stored.request(),
		responseOutput:     stored.output.bytes(),
		pendingToolCallIDs: append([]string(nil), stored.pendingToolCallIDs...),
		authID:             stored.authID,
		updatedAt:          stored.updatedAt,
	}, true
}

func (c *responsesHTTPStateCache) cleanupLocked(now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	kept := c.order[:0]
	for _, key := range c.order {
		stored, ok := c.entries[key]
		if !ok {
			continue
		}
		if now.Sub(stored.updatedAt) > c.ttl {
			delete(c.entries, key)
			c.releaseLocked(stored)
			continue
		}
		kept = append(kept, key)
	}
	clear(c.order[len(kept):])
	c.order = kept
}

// internLocked returns the shared chunk holding data, adding it when absent.
// data may reference a caller buffer and is copied unless owned. On a hash
// collision with different content the chunk stays private.
func (c *responsesHTTPStateCache) internLocked(data string, owned bool) *responsesHTTPStateChunk {
	hash := maphash.String(c.seed, data)
	if chunk, ok := c.chunks[hash]; ok && chunk.data == data {
		chunk.refs++
		return chunk
	}
	if !owned {
		data = strings.Clone(data)
	}
	chunk := &responsesHTTPStateChunk{data: data, hash: hash, refs: 1}
	if _, taken := c.chunks[hash]; !taken {
		chunk.interned = true
		c.chunks[hash] = chunk
	}
	c.totalBytes += len(chunk.data)
	return chunk
}

func (c *responsesHTTPStateCache) internJSONLocked(parts responsesHTTPStateParts) responsesHTTPStateJSON {
	value := responsesHTTPStateJSON{array: parts.array, parts: make([]*responsesHTTPStateChunk, 0, len(parts.raw))}
	for _, raw := range parts.raw {
		value.parts = append(value.parts, c.internLocked(raw, false))
	}
	return value
}

func (c *responsesHTTPStateCache) releaseChunkLocked(chunk *responsesHTTPStateChunk) {
	if chunk == nil {
		return
	}
	chunk.refs--
	if chunk.refs > 0 {
		return
	}
	c.totalBytes -= len(chunk.data)
	if chunk.interned && c.chunks[chunk.hash] == chunk {
		delete(c.chunks, chunk.hash)
	}
}

func (c *responsesHTTPStateCache) releaseLocked(stored *responsesHTTPStateStored) {
	c.totalBytes -= stored.overhead()
	c.releaseChunkLocked(stored.skeleton)
	for _, chunk := range stored.input.parts {
		c.releaseChunkLocked(chunk)
	}
	for _, chunk := range stored.output.parts {
		c.releaseChunkLocked(chunk)
	}
}

// request rebuilds the cached request into a new buffer.
func (s *responsesHTTPStateStored) request() []byte {
	skeleton := s.skeleton.data
	if s.inputOffset < 0 {
		return []byte(skeleton)
	}
	out := make([]byte, 0, len(skeleton)+s.input.len())
	out = append(out, skeleton[:s.inputOffset]...)
	out = s.input.appendTo(out)
	return append(out, skeleton[s.inputOffset:]...)
}

func (v responsesHTTPStateJSON) len() int {
	if !v.array {
		n := 0
		for _, chunk := range v.parts {
			n += len(chunk.data)
		}
		return n
	}
	n := 2 + max(len(v.parts)-1, 0)
	for _, chunk := range v.parts {
		n += len(chunk.data)
	}
	return n
}

func (v responsesHTTPStateJSON) appendTo(out []byte) []byte {
	if !v.array {
		for _, chunk := range v.parts {
			out = append(out, chunk.data...)
		}
		return out
	}
	out = append(out, '[')
	for i, chunk := range v.parts {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, chunk.data...)
	}
	return append(out, ']')
}

// bytes rebuilds the value into a new buffer; an empty value yields nil.
func (v responsesHTTPStateJSON) bytes() []byte {
	if !v.array && len(v.parts) == 0 {
		return nil
	}
	return v.appendTo(make([]byte, 0, v.len()))
}

// responsesHTTPStateView reads b as a string without copying. The view must
// not outlive put, which copies whatever it keeps.
func responsesHTTPStateView(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// responsesHTTPStateParts are the pieces of a value before interning. raw
// strings reference the caller's buffer.
type responsesHTTPStateParts struct {
	array bool
	raw   []string
}

// splitResponsesHTTPStateJSON splits a JSON array into its elements. Arrays
// are rebuilt as "[" + elements joined by "," + "]", which reproduces the
// compact arrays the merge path writes byte for byte; whitespace between
// elements of other arrays is dropped. Anything that is not a JSON array is
// kept whole.
func splitResponsesHTTPStateJSON(data []byte) responsesHTTPStateParts {
	if len(data) == 0 {
		return responsesHTTPStateParts{}
	}
	root := util.ParseGJSONBytesNoCopy(data)
	if !root.IsArray() || len(root.Raw) != len(bytes.TrimSpace(data)) {
		return responsesHTTPStateParts{raw: []string{responsesHTTPStateView(data)}}
	}
	parts := responsesHTTPStateParts{array: true}
	root.ForEach(func(_, item gjson.Result) bool {
		parts.raw = append(parts.raw, item.Raw)
		return true
	})
	return parts
}

type responsesHTTPStateSkeleton struct {
	data  string
	owned bool
}

// splitResponsesHTTPStateRequest separates the input array from the rest of
// the request. inputOffset is where the input goes back into skeleton, or -1
// when the request has no input array and skeleton is the whole request.
func splitResponsesHTTPStateRequest(request []byte) (skeleton responsesHTTPStateSkeleton, inputOffset int, input []byte) {
	root := util.ParseGJSONBytesNoCopy(request)
	if root.IsObject() {
		value := root.Get("input")
		end := value.Index + len(value.Raw)
		if value.IsArray() && value.Index > 0 && end <= len(request) &&
			responsesHTTPStateView(request[value.Index:end]) == value.Raw {
			built := make([]byte, 0, len(request)-len(value.Raw))
			built = append(built, request[:value.Index]...)
			built = append(built, request[end:]...)
			return responsesHTTPStateSkeleton{data: responsesHTTPStateView(built), owned: true}, value.Index, request[value.Index:end]
		}
	}
	return responsesHTTPStateSkeleton{data: responsesHTTPStateView(request)}, -1, nil
}
