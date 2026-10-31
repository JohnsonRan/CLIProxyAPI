package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// codexDecisionsDefaultBaseURL serves /decisions for codex-api-key credentials
// without a base-url. The ChatGPT Codex backend is not an OpenAI API endpoint.
const codexDecisionsDefaultBaseURL = "https://api.openai.com/v1"

// executeOpenAIDecisions forwards a /v1/decisions request unchanged to
// <base-url>/decisions with a codex-api-key credential. ChatGPT OAuth (and
// SIWC) credentials are not known to serve the Decisions API; the handler
// excludes them from local selection, and this guard rejects any that still
// arrive (Home mode selects credentials itself) without cooling them down.
// Upstream errors keep their status and body: the ChatGPT-specific Codex error
// classification does not apply to the OpenAI API.
func (e *CodexExecutor) executeOpenAIDecisions(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth == nil || auth.AuthKind() != cliproxyauth.AuthKindAPIKey {
		return cliproxyexecutor.Response{}, helps.OpenAIDecisionsRequestScoped(statusErr{code: http.StatusNotImplemented, msg: "the Decisions API requires an OpenAI API key credential (codex-api-key)"})
	}
	apiKey, baseURL := codexCreds(auth)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = codexDecisionsDefaultBaseURL
	}
	return helps.ExecuteOpenAIDecisions(ctx, e.cfg, e, auth, req, opts, baseURL, apiKey, func(status int, headers http.Header, body []byte) error {
		return newOpenAICompatStatusError(status, headers, body)
	})
}
