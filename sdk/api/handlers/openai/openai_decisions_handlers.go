package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Decisions handles POST /v1/decisions (OpenAI Decisions API).
//
// A request carries a model, an input (text or user messages with text and
// image parts) and typed questions; the answer lists one typed answer per
// question plus usage. The body is forwarded unchanged to <base-url>/decisions
// of a codex-api-key or openai-compatibility credential; other providers are
// never selected. ChatGPT OAuth credentials are skipped: the Codex backend is
// not known to serve this API. Decisions never stream.
func (h *OpenAIAPIHandler) Decisions(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		writeDecisionsError(c, clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), fmt.Sprintf("Invalid request: %v", err), "")
		return
	}
	if !gjson.ValidBytes(rawJSON) || !gjson.ParseBytes(rawJSON).IsObject() {
		writeDecisionsError(c, http.StatusBadRequest, "Invalid request: body must be a JSON object", "")
		return
	}

	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" {
		writeDecisionsError(c, http.StatusBadRequest, "Missing required parameter: 'model'.", "missing_required_parameter")
		return
	}
	if input := gjson.GetBytes(rawJSON, "input"); !input.Exists() || input.Type == gjson.Null {
		writeDecisionsError(c, http.StatusBadRequest, "Missing required parameter: 'input'.", "missing_required_parameter")
		return
	}
	if questions := gjson.GetBytes(rawJSON, "questions"); !questions.IsArray() || len(questions.Array()) == 0 {
		writeDecisionsError(c, http.StatusBadRequest, "Missing or invalid required parameter: 'questions' must be a non-empty array.", "missing_required_parameter")
		return
	}
	if gjson.GetBytes(rawJSON, "stream").Bool() {
		writeDecisionsError(c, http.StatusBadRequest, "Streaming is not supported for decisions.", "unsupported_parameter")
		return
	}

	c.Header("Content-Type", "application/json")
	parent := coreauth.WithExcludedAuthKind(context.Background(), coreauth.AuthKindOAuth)
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, parent)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, handlers.OpenAIDecisionsEntryProtocol, modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		if decisionsHasNoEligibleCredential(errMsg.Error) {
			writeDecisionsError(c, http.StatusBadRequest, fmt.Sprintf("No credential can serve the Decisions API for model '%s'. It needs a codex-api-key or an openai-compatibility API key credential for that model; ChatGPT OAuth credentials are not used.", modelName), "model_not_supported")
			cliCancel(errMsg.Error)
			return
		}
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// decisionsHasNoEligibleCredential reports a selection that found no API-key
// credential of a Decisions-capable provider for the model.
func decisionsHasNoEligibleCredential(err error) bool {
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "provider_not_found"
}

func writeDecisionsError(c *gin.Context, status int, message, code string) {
	c.JSON(status, handlers.ErrorResponse{Error: handlers.ErrorDetail{
		Message: message,
		Type:    "invalid_request_error",
		Code:    code,
	}})
}
