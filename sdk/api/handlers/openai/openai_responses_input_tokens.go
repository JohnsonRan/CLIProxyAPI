package openai

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesInputTokenCountPaths lists where the built-in executors report a token
// count when no translator converts it into the Responses format. The count is
// the provider's own number (upstream count endpoint or executor estimate), so
// the handler only normalizes its location and never re-estimates it.
var responsesInputTokenCountPaths = []string{
	"input_tokens",                // Claude, Kimi, Devin
	"response.usage.input_tokens", // Codex, xAI, Meta
	"usage.input_tokens",
	"usage.prompt_tokens", // OpenAI-compatible providers
	"totalTokens",         // Gemini, Vertex, AI Studio, Antigravity
	"total_tokens",
	"usage.total_tokens",
}

// InputTokens handles POST /v1/responses/input_tokens. It counts the input of a
// Responses request with the provider that would serve it and answers in the
// OpenAI shape {"object":"response.input_tokens","input_tokens":N}.
func (h *OpenAIResponsesAPIHandler) InputTokens(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// Count the payload generation would process.
	rawJSON = h.prepareCodexMultiAgentV2Tools(c, rawJSON)
	rawJSON = h.prepareCodexOrphanDelegation(c, rawJSON)
	if gjson.GetBytes(rawJSON, "stream").Exists() {
		if updated, errDelete := sjson.DeleteBytes(rawJSON, "stream"); errDelete == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	resp, upstreamHeaders, errMsg := h.ExecuteCountWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	count, ok := responsesInputTokenCount(resp)
	if !ok {
		errMsg = &interfaces.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      fmt.Errorf("token count response for model %s did not include an input token count", modelName),
		}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = fmt.Fprintf(c.Writer, `{"object":"response.input_tokens","input_tokens":%d}`, count)
	cliCancel()
}

func responsesInputTokenCount(response []byte) (int64, bool) {
	if !gjson.ValidBytes(response) {
		return 0, false
	}
	for _, path := range responsesInputTokenCountPaths {
		value := gjson.GetBytes(response, path)
		if value.Type == gjson.Number && value.Int() >= 0 {
			return value.Int(), true
		}
	}
	return 0, false
}
