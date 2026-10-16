package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Upstream quota endpoints and client headers mirror the management panel's
// quota probes so the upstream sees the same traffic as a panel refresh.
const (
	clientUsageCodexURL         = "https://chatgpt.com/backend-api/wham/usage"
	clientUsageCodexUserAgent   = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	clientUsageClaudeURL        = "https://api.anthropic.com/api/oauth/usage"
	clientUsageClaudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
	clientUsageClaudeBeta       = "oauth-2025-04-20"
	clientUsageAntigravityUA    = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	clientUsageXAIWeeklyURL     = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	clientUsageXAIMonthlyURL    = "https://cli-chat-proxy.grok.com/v1/billing"
	clientUsageXAIClientVersion = "1.0.46"
	clientUsageXAIUserAgent     = "grok-pager/" + clientUsageXAIClientVersion + " grok-shell/" + clientUsageXAIClientVersion + " (macos; aarch64)"
	clientUsageXAITokenAuth     = "xai-grok-cli"
)

var clientUsageAntigravityURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// clientUsageRedactedKeys are account identifiers removed, at any depth, from
// upstream bodies before they reach client API key holders.
var clientUsageRedactedKeys = map[string]struct{}{
	"email":      {},
	"user_id":    {},
	"userId":     {},
	"account_id": {},
	"accountId":  {},
}

// clientUsageRequest sends one quota request and records the upstream
// response, whatever its status. Errors never include credentials.
func (h *Handler) clientUsageRequest(ctx context.Context, auth *coreauth.Auth, token, method, targetURL string, body []byte, headers map[string]string) clientUsageUpstream {
	result := clientUsageUpstream{Endpoint: targetURL}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if errRequest != nil {
		result.Error = "build quota request failed"
		return result
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, errDo := client.Do(req)
	if errDo != nil {
		result.Error = "upstream quota request failed"
		return result
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("client usage: response body close error: %v", errClose)
		}
	}()
	result.StatusCode = resp.StatusCode
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, clientUsageMaxResponseBytes+1))
	if errRead != nil || len(payload) > clientUsageMaxResponseBytes {
		result.Error = "upstream quota response unreadable"
		return result
	}
	redacted, errRedact := redactClientUsageBody(payload)
	if errRedact != nil {
		result.Error = "upstream quota response is not JSON"
		return result
	}
	result.Body = redacted
	return result
}

func clientUsageUpstreamOK(response clientUsageUpstream) bool {
	return response.Error == "" && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

// redactClientUsageBody re-encodes a JSON body without the keys in
// clientUsageRedactedKeys. Numbers keep their literal form.
func redactClientUsageBody(payload []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, errDecode
	}
	if _, errToken := decoder.Token(); !errors.Is(errToken, io.EOF) {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(redactClientUsageValue(value)); errEncode != nil {
		return nil, errEncode
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes())), nil
}

func redactClientUsageValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if _, redacted := clientUsageRedactedKeys[key]; redacted {
				delete(typed, key)
				continue
			}
			typed[key] = redactClientUsageValue(item)
		}
	case []any:
		for i, item := range typed {
			typed[i] = redactClientUsageValue(item)
		}
	}
	return value
}

func (h *Handler) probeCodexClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": clientUsageCodexUserAgent}
	if accountID := clientUsageString(auth.Metadata, "account_id", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	return []clientUsageUpstream{h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageCodexURL, nil, headers)}
}

func (h *Handler) probeClaudeClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{
		"Content-Type":   "application/json",
		"User-Agent":     clientUsageClaudeUserAgent,
		"Anthropic-Beta": clientUsageClaudeBeta,
	}
	return []clientUsageUpstream{h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageClaudeURL, nil, headers)}
}

// probeAntigravityClientUsage tries the panel's endpoints in order and stops
// at the first successful one; failed attempts are kept in the result.
func (h *Handler) probeAntigravityClientUsage(ctx context.Context, auth *coreauth.Auth, token string) ([]clientUsageUpstream, error) {
	projectID := clientUsageString(auth.Metadata, "project_id", "projectId")
	if projectID == "" && auth.Attributes != nil {
		projectID = firstNonEmptyValue(auth.Attributes["project_id"], auth.Attributes["projectId"], auth.Attributes["gemini_virtual_project"])
	}
	if projectID == "" {
		return nil, fmt.Errorf("credential project ID is unavailable")
	}
	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		return nil, fmt.Errorf("build quota request failed")
	}
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": clientUsageAntigravityUA}
	responses := make([]clientUsageUpstream, 0, len(clientUsageAntigravityURLs))
	for _, targetURL := range clientUsageAntigravityURLs {
		response := h.clientUsageRequest(ctx, auth, token, http.MethodPost, targetURL, body, headers)
		responses = append(responses, response)
		if clientUsageUpstreamOK(response) || ctx.Err() != nil {
			break
		}
	}
	return responses, nil
}

// probeXAIClientUsage returns both billing views the panel reads: the
// credit-based weekly period and the monthly subscription usage.
func (h *Handler) probeXAIClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{
		"Accept":                "*/*",
		"User-Agent":            clientUsageXAIUserAgent,
		"X-Xai-Token-Auth":      clientUsageXAITokenAuth,
		"X-Grok-Client-Version": clientUsageXAIClientVersion,
	}
	if userID := clientUsageString(auth.Metadata, "sub", "subject", "user_id", "userId"); userID != "" {
		headers["X-Userid"] = userID
	}
	return []clientUsageUpstream{
		h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageXAIWeeklyURL, nil, headers),
		h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageXAIMonthlyURL, nil, headers),
	}
}

func clientUsageString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
