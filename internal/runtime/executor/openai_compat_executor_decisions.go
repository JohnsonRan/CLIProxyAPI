package executor

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// executeOpenAIDecisions forwards a /v1/decisions request unchanged to
// <base-url>/decisions of an OpenAI-compatible provider.
func (e *OpenAICompatExecutor) executeOpenAIDecisions(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}
	return helps.ExecuteOpenAIDecisions(ctx, e.cfg, e, auth, req, opts, baseURL, apiKey, func(status int, headers http.Header, body []byte) error {
		return newOpenAICompatStatusError(status, headers, body)
	})
}
