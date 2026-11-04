package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
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

// responsesHTTPPreviousResponseNotFoundError reports a previous_response_id the
// proxy cannot expand. No executor forwards previous_response_id upstream, so
// sending the delta alone would silently drop the conversation history; the
// client must resend the full input instead, as with OpenAI's own API.
func responsesHTTPPreviousResponseNotFoundError(prevID string) *interfaces.ErrorMessage {
	doc := ojson.NewDoc([]byte(`{"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`))
	_ = doc.Set("error.message", fmt.Sprintf("Previous response with id '%s' not found.", prevID))
	return &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(doc.String())}
}

// prepareHTTPResponsesIncrementalRequest expands previous_response_id into the
// cached history. It fails when the id is unknown to the caller or the delta
// cannot be merged, instead of forwarding a context-less request.
func (h *OpenAIResponsesAPIHandler) prepareHTTPResponsesIncrementalRequest(c *gin.Context, rawJSON []byte) ([]byte, *interfaces.ErrorMessage) {
	if len(rawJSON) == 0 {
		return rawJSON, nil
	}
	prevID := strings.TrimSpace(ojson.GetBytesNoCopy(rawJSON, "previous_response_id").String())
	if prevID == "" {
		return rawJSON, nil
	}
	if c != nil {
		c.Set(responsesHTTPPreviousResponseIDContextKey, prevID)
	}
	entry, ok := defaultResponsesHTTPStateCache.get(prevID, responsesHTTPStateOwner(c))
	if !ok {
		return nil, responsesHTTPPreviousResponseNotFoundError(prevID)
	}
	if c != nil && entry.authID != "" {
		c.Set(responsesHTTPPinnedAuthIDContextKey, entry.authID)
	}

	streamResult := ojson.GetBytesNoCopy(rawJSON, "stream")
	wasStreaming := streamResult.Type == ojson.True

	withType := coerceResponsesWebsocketStringInput(rawJSON)
	if !ojson.GetBytesNoCopy(withType, "type").Exists() {
		doc := ojson.NewDoc(withType)
		_ = doc.Set("type", wsRequestTypeCreate)
		withType = doc.Bytes()
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
	if errMsg != nil {
		return nil, errMsg
	}
	if len(normalized) == 0 {
		return nil, responsesHTTPPreviousResponseNotFoundError(prevID)
	}
	if !wasStreaming {
		doc := ojson.NewDoc(normalized)
		if streamResult.Exists() {
			_ = doc.Set("stream", false)
		} else {
			_ = doc.Delete("stream")
		}
		normalized = doc.Bytes()
	}
	if c != nil {
		c.Set(responsesHTTPExpandedFromCacheContextKey, true)
	}
	return normalized, nil
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
		prevField := ojson.GetBytesNoCopy(resp, "previous_response_id")
		if !prevField.Exists() || prevField.Type == ojson.Null || strings.TrimSpace(prevField.String()) == "" {
			doc := ojson.NewDoc(resp)
			if errSet := doc.Set("previous_response_id", prevID); errSet == nil {
				resp = doc.Bytes()
			}
		}
	}
	responseID := strings.TrimSpace(ojson.GetBytesNoCopy(resp, "id").String())
	if responseID == "" {
		return resp
	}
	outputRaw := []byte("[]")
	if out := ojson.GetBytesNoCopy(resp, "output"); out.Exists() && out.IsArray() {
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
	responseID := strings.TrimSpace(ojson.GetBytesNoCopy(completedPayload, "response.id").String())
	if responseID == "" {
		return
	}
	outputRaw := []byte("[]")
	if out := ojson.GetBytesNoCopy(completedPayload, "response.output"); out.Exists() && out.IsArray() {
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
	doc := ojson.NewDoc(snapshot)
	if !doc.Get("input").Exists() {
		_ = doc.SetRawBytes("input", []byte("[]"))
	}
	_ = doc.Delete("previous_response_id")
	return doc.Bytes()
}

func extractPendingToolCallIDsFromOutput(outputRaw []byte) []string {
	parsed := ojson.ParseBytes(outputRaw)
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
