package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesHTTPStateCacheMaxEntries = 1024
	// responsesHTTPStateCacheMaxBytes bounds the transcripts kept for HTTP
	// continuation; long Codex sessions carry megabytes per entry.
	responsesHTTPStateCacheMaxBytes           = 256 << 20
	responsesHTTPStateCacheTTL                = 30 * time.Minute
	responsesHTTPPreviousResponseIDContextKey = "responsesHTTPPreviousResponseID"
	responsesHTTPPinnedAuthIDContextKey       = "responsesHTTPPinnedAuthID"
	responsesHTTPExpandedFromCacheContextKey  = "responsesHTTPExpandedFromCache"
)

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

type responsesHTTPStateCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	totalBytes int
	nowFunc    func() time.Time
	entries    map[string]responsesHTTPStateEntry
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
		entries:    make(map[string]responsesHTTPStateEntry),
	}
}

// responsesHTTPStateOwner derives the cache owner from the authenticated client
// credential, so a leaked response id cannot continue another caller's session.
func responsesHTTPStateOwner(c *gin.Context) string {
	if c == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(c.GetString("accessProvider") + "\x00" + c.GetString("userApiKey")))
	return hex.EncodeToString(sum[:])
}

func (c *responsesHTTPStateCache) now() time.Time {
	if c != nil && c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

func (c *responsesHTTPStateCache) put(responseID string, entry responsesHTTPStateEntry) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" || len(entry.request) == 0 {
		return
	}
	now := c.now()
	entry.updatedAt = now
	entry.request = bytes.Clone(entry.request)
	entry.responseOutput = bytes.Clone(entry.responseOutput)
	entry.pendingToolCallIDs = append([]string(nil), entry.pendingToolCallIDs...)
	entrySize := entry.size()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)
	if previous, exists := c.entries[responseID]; exists {
		c.removeLocked(responseID, previous)
	}
	if entrySize > c.maxBytes {
		return
	}
	c.order = append(c.order, responseID)
	c.entries[responseID] = entry
	c.totalBytes += entrySize

	for len(c.order) > 0 && (len(c.order) > c.maxEntries || c.totalBytes > c.maxBytes) {
		evict := c.order[0]
		c.order[0] = ""
		c.order = c.order[1:]
		if evicted, ok := c.entries[evict]; ok {
			delete(c.entries, evict)
			c.totalBytes -= evicted.size()
		}
	}
}

// removeLocked drops responseID from the entries, the byte total, and the order.
func (c *responsesHTTPStateCache) removeLocked(responseID string, entry responsesHTTPStateEntry) {
	delete(c.entries, responseID)
	c.totalBytes -= entry.size()
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
	entry, ok := c.entries[responseID]
	if !ok || entry.owner != owner {
		return responsesHTTPStateEntry{}, false
	}
	entry.updatedAt = now
	c.entries[responseID] = entry
	return responsesHTTPStateEntry{
		owner:              entry.owner,
		request:            bytes.Clone(entry.request),
		responseOutput:     bytes.Clone(entry.responseOutput),
		pendingToolCallIDs: append([]string(nil), entry.pendingToolCallIDs...),
		authID:             entry.authID,
		updatedAt:          entry.updatedAt,
	}, true
}

func (c *responsesHTTPStateCache) cleanupLocked(now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	kept := c.order[:0]
	for _, key := range c.order {
		entry, ok := c.entries[key]
		if !ok {
			continue
		}
		if now.Sub(entry.updatedAt) > c.ttl {
			delete(c.entries, key)
			c.totalBytes -= entry.size()
			continue
		}
		kept = append(kept, key)
	}
	c.order = kept
}

func (h *OpenAIResponsesAPIHandler) prepareHTTPResponsesIncrementalRequest(c *gin.Context, rawJSON []byte) []byte {
	if len(rawJSON) == 0 {
		return rawJSON
	}
	prevID := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
	if prevID == "" {
		return rawJSON
	}
	if c != nil {
		c.Set(responsesHTTPPreviousResponseIDContextKey, prevID)
	}
	entry, ok := defaultResponsesHTTPStateCache.get(prevID, responsesHTTPStateOwner(c))
	if !ok {
		return rawJSON
	}
	if c != nil && entry.authID != "" {
		c.Set(responsesHTTPPinnedAuthIDContextKey, entry.authID)
	}

	streamResult := gjson.GetBytes(rawJSON, "stream")
	wasStreaming := streamResult.Type == gjson.True

	withType := coerceResponsesWebsocketStringInput(rawJSON)
	if !gjson.GetBytes(withType, "type").Exists() {
		withType, _ = sjson.SetBytes(withType, "type", wsRequestTypeCreate)
	}
	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
		withType,
		entry.request,
		entry.responseOutput,
		prevID,
		entry.pendingToolCallIDs,
		false,
		true,
	)
	if errMsg != nil || len(normalized) == 0 {
		return rawJSON
	}
	if !wasStreaming {
		if streamResult.Exists() {
			normalized, _ = sjson.SetBytes(normalized, "stream", false)
		} else {
			normalized, _ = sjson.DeleteBytes(normalized, "stream")
		}
	}
	if c != nil {
		c.Set(responsesHTTPExpandedFromCacheContextKey, true)
	}
	return normalized
}

func httpIncrementalPreviousResponseIDFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GetString(responsesHTTPPreviousResponseIDContextKey))
}

func (h *OpenAIResponsesAPIHandler) enrichHTTPIncrementalContext(c *gin.Context, ctx context.Context, selectedAuthID *string) context.Context {
	if c != nil {
		if pinnedAuthID := strings.TrimSpace(c.GetString(responsesHTTPPinnedAuthIDContextKey)); pinnedAuthID != "" {
			ctx = handlers.WithPinnedAuthID(ctx, pinnedAuthID)
		}
	}
	ctx = handlers.WithSelectedAuthIDCallback(ctx, func(authID string) {
		if selectedAuthID != nil {
			*selectedAuthID = strings.TrimSpace(authID)
		}
	})
	return ctx
}

func (h *OpenAIResponsesAPIHandler) finalizeHTTPNonStreamIncrementalResponse(c *gin.Context, requestJSON []byte, resp []byte, selectedAuthID string) []byte {
	if len(resp) == 0 {
		return resp
	}
	if prevID := httpIncrementalPreviousResponseIDFromContext(c); prevID != "" {
		prevField := gjson.GetBytes(resp, "previous_response_id")
		if !prevField.Exists() || prevField.Type == gjson.Null || strings.TrimSpace(prevField.String()) == "" {
			if updated, errSet := sjson.SetBytes(resp, "previous_response_id", prevID); errSet == nil {
				resp = updated
			}
		}
	}
	responseID := strings.TrimSpace(gjson.GetBytes(resp, "id").String())
	if responseID == "" {
		return resp
	}
	outputRaw := []byte("[]")
	if out := gjson.GetBytes(resp, "output"); out.Exists() && out.IsArray() {
		outputRaw = []byte(out.Raw)
	}
	pendingCallIDs := extractPendingToolCallIDsFromOutput(outputRaw)
	canonicalReq := canonicalHTTPIncrementalRequestSnapshot(requestJSON)
	defaultResponsesHTTPStateCache.put(responseID, responsesHTTPStateEntry{
		owner:              responsesHTTPStateOwner(c),
		request:            canonicalReq,
		responseOutput:     outputRaw,
		pendingToolCallIDs: pendingCallIDs,
		authID:             strings.TrimSpace(selectedAuthID),
	})
	return resp
}

func (h *OpenAIResponsesAPIHandler) recordHTTPStreamIncrementalCompleted(owner string, requestJSON []byte, completedPayload []byte, selectedAuthID string) {
	if len(completedPayload) == 0 {
		return
	}
	responseID := strings.TrimSpace(gjson.GetBytes(completedPayload, "response.id").String())
	if responseID == "" {
		return
	}
	outputRaw := []byte("[]")
	if out := gjson.GetBytes(completedPayload, "response.output"); out.Exists() && out.IsArray() {
		outputRaw = []byte(out.Raw)
	}
	pendingCallIDs := extractPendingToolCallIDsFromOutput(outputRaw)
	canonicalReq := canonicalHTTPIncrementalRequestSnapshot(requestJSON)
	defaultResponsesHTTPStateCache.put(responseID, responsesHTTPStateEntry{
		owner:              owner,
		request:            canonicalReq,
		responseOutput:     outputRaw,
		pendingToolCallIDs: pendingCallIDs,
		authID:             strings.TrimSpace(selectedAuthID),
	})
}

func canonicalHTTPIncrementalRequestSnapshot(requestJSON []byte) []byte {
	snapshot := coerceResponsesWebsocketStringInput(bytes.Clone(requestJSON))
	if !gjson.GetBytes(snapshot, "input").Exists() {
		snapshot, _ = sjson.SetRawBytes(snapshot, "input", []byte("[]"))
	}
	snapshot, _ = sjson.DeleteBytes(snapshot, "previous_response_id")
	return snapshot
}

func extractPendingToolCallIDsFromOutput(outputRaw []byte) []string {
	parsed := gjson.ParseBytes(outputRaw)
	if !parsed.IsArray() {
		return nil
	}
	var ids []string
	for _, item := range parsed.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call":
			if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
				ids = append(ids, callID)
			}
		}
	}
	return ids
}
