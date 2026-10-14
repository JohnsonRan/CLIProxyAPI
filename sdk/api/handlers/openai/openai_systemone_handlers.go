package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

// systemOneHandlerType is the entry protocol for TypeSafe System One decision requests.
const systemOneHandlerType = "systemone"

// SystemOne handles POST /v1/systemone.
//
// The request and response follow TypeSafe's System One shape
// (https://docs.typesafe.ai/api): a state plus typed questions in, typed answers out.
// The body is passed through unchanged to an OpenAI-compatible provider whose
// base URL serves <base-url>/systemone, such as https://api.typesafe.ai/v1 or
// https://api.commandcode.ai/provider/v1. System One never streams.
func (h *OpenAIAPIHandler) SystemOne(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		writeSystemOneError(c, clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), fmt.Sprintf("Invalid request: %v", err), "")
		return
	}
	if !gjson.ValidBytes(rawJSON) || !gjson.ParseBytes(rawJSON).IsObject() {
		writeSystemOneError(c, http.StatusBadRequest, "Invalid request: body must be a JSON object", "")
		return
	}

	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" {
		writeSystemOneError(c, http.StatusBadRequest, "Missing required parameter 'model'", "missing_required_parameter")
		return
	}
	if state := gjson.GetBytes(rawJSON, "state"); !state.Exists() || state.Type == gjson.Null {
		writeSystemOneError(c, http.StatusBadRequest, "Missing required parameter 'state'", "missing_required_parameter")
		return
	}
	if questions := gjson.GetBytes(rawJSON, "questions"); !questions.IsObject() || len(questions.Map()) == 0 {
		writeSystemOneError(c, http.StatusBadRequest, "Missing or invalid required parameter 'questions'", "missing_required_parameter")
		return
	}

	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, systemOneHandlerType, modelName, rawJSON, h.GetAlt(c))
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func writeSystemOneError(c *gin.Context, status int, message, code string) {
	c.JSON(status, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: message,
			Type:    "invalid_request_error",
			Code:    code,
		},
	})
}
