package helps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	// OpenAIDecisionsFormat is the entry protocol of POST /v1/decisions requests.
	OpenAIDecisionsFormat = "openai-decisions"
	// openAIDecisionsPath is appended to the provider base URL.
	openAIDecisionsPath = "/decisions"
)

// IsOpenAIDecisionsRequest reports whether opts belong to a /v1/decisions request.
func IsOpenAIDecisionsRequest(opts cliproxyexecutor.Options) bool {
	return opts.SourceFormat.String() == OpenAIDecisionsFormat
}

// StatusErrorFunc builds an executor's error for a non-2xx upstream response.
type StatusErrorFunc func(status int, headers http.Header, body []byte) error

// openAIDecisionsRequestScopedError marks a Decisions API failure that says
// nothing about the credential's health on other endpoints. The auth manager
// then neither cools down the credential nor its model, which the same
// credential may serve for chat and Responses.
type openAIDecisionsRequestScopedError struct {
	error
}

func (e openAIDecisionsRequestScopedError) Unwrap() error { return e.error }

// IsRequestScoped implements cliproxyexecutor.RequestScopedError.
func (e openAIDecisionsRequestScopedError) IsRequestScoped() bool { return true }

// StatusCode forwards the wrapped error's HTTP status.
func (e openAIDecisionsRequestScopedError) StatusCode() int {
	var coder interface{ StatusCode() int }
	if errors.As(e.error, &coder) {
		return coder.StatusCode()
	}
	return 0
}

// OpenAIDecisionsRequestScoped wraps err so it never cools down the credential.
func OpenAIDecisionsRequestScoped(err error) error {
	if err == nil {
		return nil
	}
	return openAIDecisionsRequestScopedError{error: err}
}

// openAIDecisionsStatusIsCredentialHealth reports whether an upstream status
// describes the credential or the upstream rather than this endpoint: bad or
// expired key (401), billing (402), rate limit (429) and server errors keep
// their normal retry and cooldown handling. Every other status (a relay
// without /decisions answering 404 or 501, an account without access answering 403,
// an invalid request) is scoped to the decisions request.
func openAIDecisionsStatusIsCredentialHealth(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusPaymentRequired ||
		status == http.StatusTooManyRequests || (status >= http.StatusInternalServerError && status != http.StatusNotImplemented)
}

// ExecuteOpenAIDecisions forwards an OpenAI Decisions API request to
// <baseURL>/decisions with bearer authentication. Decisions are not chat
// messages, so the body skips translation and thinking: only the upstream model
// name is rewritten, then payload rules run as the final step. The upstream
// body and headers of a successful call are returned unchanged; a non-2xx
// response becomes statusError's error so retry, cooldown and failure usage
// follow the calling executor's rules.
func ExecuteOpenAIDecisions(ctx context.Context, cfg *config.Config, executor usageExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseURL, apiKey string, statusError StatusErrorFunc) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := NewExecutorUsageReporter(ctx, executor, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	payload := req.Payload
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = payload
	}
	if baseModel != "" {
		payload = SetStringIfDifferent(payload, "model", baseModel)
	}
	payload = ApplyPayloadConfigWithRequest(cfg, baseModel, OpenAIDecisionsFormat, opts.SourceFormat.String(), "", payload, original, PayloadRequestedModel(opts, req.Model), PayloadRequestPath(opts), opts.Headers)

	url := strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + openAIDecisionsPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	provider := ""
	if executor != nil {
		provider = executor.Identifier()
	}
	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      payload,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := reporter.TrackHTTPClient(NewProxyAwareHTTPClient(ctx, cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		RecordAPIResponseError(ctx, cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("%s executor: close decisions response body error: %v", provider, errClose)
		}
	}()
	RecordAPIResponseMetadata(ctx, cfg, httpResp.StatusCode, httpResp.Header.Clone())

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		RecordAPIResponseError(ctx, cfg, err)
		return resp, err
	}
	AppendAPIResponseChunk(ctx, cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		LogWithRequestID(ctx).Debugf("decisions request error, error status: %d, error message: %s", httpResp.StatusCode, SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		err = statusError(httpResp.StatusCode, httpResp.Header.Clone(), body)
		if !openAIDecisionsStatusIsCredentialHealth(httpResp.StatusCode) {
			err = OpenAIDecisionsRequestScoped(err)
		}
		return resp, err
	}

	reporter.ObserveResponseModel(body)
	reporter.Publish(ctx, ParseOpenAIUsage(body))
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}, nil
}
