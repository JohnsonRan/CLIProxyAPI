package openai

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	responsesHTTPStateCacheMaxEntries         = 1024
	responsesHTTPStateCacheTTL                = 30 * time.Minute
	responsesHTTPPreviousResponseIDContextKey = "responsesHTTPPreviousResponseID"
	responsesHTTPPinnedAuthIDContextKey       = "responsesHTTPPinnedAuthID"
	responsesHTTPExpandedFromCacheContextKey  = "responsesHTTPExpandedFromCache"
)

type responsesHTTPStateEntry struct {
	request            []byte
	responseOutput     []byte
	pendingToolCallIDs []string
	authID             string
	updatedAt          time.Time
}

type responsesHTTPStateCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	nowFunc    func() time.Time
	entries    map[string]responsesHTTPStateEntry
	order      []string
}

var defaultResponsesHTTPStateCache = newResponsesHTTPStateCache(responsesHTTPStateCacheTTL, responsesHTTPStateCacheMaxEntries)

func newResponsesHTTPStateCache(ttl time.Duration, maxEntries int) *responsesHTTPStateCache {
	if ttl <= 0 {
		ttl = responsesHTTPStateCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = responsesHTTPStateCacheMaxEntries
	}
	return &responsesHTTPStateCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		nowFunc:    time.Now,
		entries:    make(map[string]responsesHTTPStateEntry),
	}
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

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)
	if _, exists := c.entries[responseID]; !exists {
		c.order = append(c.order, responseID)
	}
	c.entries[responseID] = entry

	for len(c.order) > c.maxEntries {
		evict := c.order[0]
		c.order[0] = ""
		c.order = c.order[1:]
		delete(c.entries, evict)
	}
}

func (c *responsesHTTPStateCache) get(responseID string) (responsesHTTPStateEntry, bool) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return responsesHTTPStateEntry{}, false
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupLocked(now)
	entry, ok := c.entries[responseID]
	if !ok {
		return responsesHTTPStateEntry{}, false
	}
	entry.updatedAt = now
	c.entries[responseID] = entry
	return responsesHTTPStateEntry{
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
	entry, ok := defaultResponsesHTTPStateCache.get(prevID)
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
		request:            canonicalReq,
		responseOutput:     outputRaw,
		pendingToolCallIDs: pendingCallIDs,
		authID:             strings.TrimSpace(selectedAuthID),
	})
	return resp
}

func (h *OpenAIResponsesAPIHandler) recordHTTPStreamIncrementalCompleted(requestJSON []byte, completedPayload []byte, selectedAuthID string) {
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
