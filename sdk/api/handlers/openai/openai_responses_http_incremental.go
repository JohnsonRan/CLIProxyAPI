package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

// responsesHTTPStateOwner derives the cache owner from the authenticated client
// credential, so a leaked response id cannot continue another caller's session.
func responsesHTTPStateOwner(c *gin.Context) string {
	if c == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(c.GetString("accessProvider") + "\x00" + c.GetString("userApiKey")))
	return hex.EncodeToString(sum[:])
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
