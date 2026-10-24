package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Upstream quota endpoints and client headers mirror the management panel's
// quota probes so the upstream sees the same traffic as a panel refresh.
const (
	clientUsageCodexURL             = "https://chatgpt.com/backend-api/wham/usage"
	clientUsageCodexSubscriptionURL = "https://chatgpt.com/backend-api/subscriptions"
	clientUsageCodexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	clientUsageCodexUserAgent       = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	clientUsageClaudeURL            = "https://api.anthropic.com/api/oauth/usage"
	clientUsageClaudeProfileURL     = "https://api.anthropic.com/api/oauth/profile"
	clientUsageClaudeResetGrantsURL = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	clientUsageClaudeUserAgent      = "claude-cli/2.1.280 (external, cli)"
	clientUsageClaudeBeta           = "oauth-2025-04-20"
	clientUsageAntigravityTierURL   = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	clientUsageXAIWeeklyURL         = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	clientUsageXAIMonthlyURL        = "https://cli-chat-proxy.grok.com/v1/billing"
	clientUsageXAIUserURL           = "https://cli-chat-proxy.grok.com/v1/user?include=subscription"
	clientUsageXAISettingsURL       = "https://cli-chat-proxy.grok.com/v1/settings"
	clientUsageXAIClientVersion     = "1.0.46"
	clientUsageXAIUserAgent         = "grok-pager/" + clientUsageXAIClientVersion + " grok-shell/" + clientUsageXAIClientVersion + " (macos; aarch64)"
	clientUsageXAITokenAuth         = "xai-grok-cli"
)

var clientUsageAntigravityURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// Identity endpoints answer with names, emails and organization details; only
// the plan fields below are kept from them, in the summary and the raw body.
var (
	clientUsageClaudeProfileFields = []string{
		"organization.organization_type", "organization.subscription_status",
		"account.has_claude_max", "account.has_claude_pro",
	}
	clientUsageClaudeResetGrantFields = []string{"cedar_ember"}
	clientUsageAntigravityTierFields  = []string{
		"currentTier.id", "currentTier.name", "paidTier.id", "paidTier.name",
		"current_tier.id", "current_tier.name", "paid_tier.id", "paid_tier.name",
	}
	clientUsageXAIUserFields     = []string{"subscriptionTier", "subscription_tier"}
	clientUsageXAISettingsFields = []string{"subscription_tier_display", "subscriptionTierDisplay"}
)

// clientUsageRedactedKeys are account identifiers removed, at any depth and
// in any letter case, from upstream bodies before they reach client API key
// holders. Keys are stored lower-cased.
var clientUsageRedactedKeys = map[string]struct{}{
	"email":      {},
	"user_id":    {},
	"userid":     {},
	"account_id": {},
	"accountid":  {},
}

// clientUsageCall describes one upstream quota request. Optional calls add
// detail (plan, resets) but do not decide whether the credential's quota was
// read. Fields, when set, reduce the body to those paths.
type clientUsageCall struct {
	kind     string
	optional bool
	method   string
	url      string
	body     []byte
	headers  map[string]string
	fields   []string
}

// clientUsageRequests runs calls concurrently and returns their responses in
// call order.
func (h *Handler) clientUsageRequests(ctx context.Context, auth *coreauth.Auth, token string, calls []clientUsageCall) []clientUsageUpstream {
	responses := make([]clientUsageUpstream, len(calls))
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			responses[index] = h.clientUsageRequest(ctx, auth, token, calls[index])
		}(i)
	}
	wg.Wait()
	return responses
}

// clientUsageRequest sends one quota request and records the upstream
// response, whatever its status. Errors never include credentials.
func (h *Handler) clientUsageRequest(ctx context.Context, auth *coreauth.Auth, token string, call clientUsageCall) clientUsageUpstream {
	result := clientUsageUpstream{Endpoint: call.url, kind: call.kind, optional: call.optional}
	var reader io.Reader
	if call.body != nil {
		reader = bytes.NewReader(call.body)
	}
	method := call.method
	if method == "" {
		method = http.MethodGet
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, call.url, reader)
	if errRequest != nil {
		result.Error = "build quota request failed"
		return result
	}
	for key, value := range call.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, errDo := client.Do(req)
	if errDo != nil {
		log.WithFields(log.Fields{"provider": clientUsageProvider(auth), "auth_id": auth.ID, "endpoint": call.url}).WithError(errDo).Debug("client usage: upstream quota request failed")
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
	if call.fields != nil {
		redacted = pickClientUsageFields(redacted, call.fields)
	}
	result.Body = redacted
	return result
}

func clientUsageUpstreamOK(response clientUsageUpstream) bool {
	return response.Error == "" && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}

// pickClientUsageFields keeps only the given paths of a JSON object.
func pickClientUsageFields(body json.RawMessage, fields []string) json.RawMessage {
	parsed := gjson.ParseBytes(body)
	out := []byte("{}")
	if !parsed.IsObject() {
		return out
	}
	for _, field := range fields {
		if value := parsed.Get(field); value.Exists() {
			if updated, errSet := sjson.SetRawBytes(out, field, []byte(value.Raw)); errSet == nil {
				out = updated
			}
		}
	}
	return out
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
			if _, redacted := clientUsageRedactedKeys[strings.ToLower(key)]; redacted {
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
	accountID := clientUsageString(auth.Metadata, "account_id", "chatgpt_account_id", "chatgptAccountId")
	if accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	resetHeaders := map[string]string{"Accept": "application/json", "OpenAI-Beta": "codex-1", "Originator": "Codex Desktop"}
	for key, value := range headers {
		resetHeaders[key] = value
	}
	calls := []clientUsageCall{
		{kind: clientUsageKindUsage, url: clientUsageCodexURL, headers: headers},
		{kind: clientUsageKindResetCredits, optional: true, url: clientUsageCodexResetCreditsURL, headers: resetHeaders},
	}
	if accountID != "" {
		calls = append(calls, clientUsageCall{
			kind: clientUsageKindSubscription, optional: true,
			url:     clientUsageCodexSubscriptionURL + "?account_id=" + url.QueryEscape(accountID),
			headers: headers,
			fields:  []string{"plan_type", "active_until", "activeUntil", "will_renew"},
		})
	}
	return h.clientUsageRequests(ctx, auth, token, calls)
}

func (h *Handler) probeClaudeClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{
		"Content-Type":   "application/json",
		"User-Agent":     clientUsageClaudeUserAgent,
		"Anthropic-Beta": clientUsageClaudeBeta,
	}
	usageHeaders := map[string]string{"Accept": "application/json"}
	for key, value := range headers {
		usageHeaders[key] = value
	}
	return h.clientUsageRequests(ctx, auth, token, []clientUsageCall{
		{kind: clientUsageKindUsage, url: clientUsageClaudeURL, headers: usageHeaders},
		{kind: clientUsageKindProfile, optional: true, url: clientUsageClaudeProfileURL, headers: headers, fields: clientUsageClaudeProfileFields},
		{kind: clientUsageKindResetGrants, optional: true, url: clientUsageClaudeResetGrantsURL, headers: headers, fields: clientUsageClaudeResetGrantFields},
	})
}

// probeAntigravityClientUsage tries the panel's quota endpoints in order and
// stops at the first successful one; failed attempts are kept in the result.
// The subscription tier is read alongside.
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
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": misc.AntigravityUserAgent()}

	tierDone := make(chan clientUsageUpstream, 1)
	go func() {
		tierDone <- h.clientUsageRequest(ctx, auth, token, clientUsageCall{
			kind: clientUsageKindTier, optional: true, method: http.MethodPost, url: clientUsageAntigravityTierURL,
			body: []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`), headers: headers, fields: clientUsageAntigravityTierFields,
		})
	}()
	responses := make([]clientUsageUpstream, 0, len(clientUsageAntigravityURLs)+1)
	for _, targetURL := range clientUsageAntigravityURLs {
		response := h.clientUsageRequest(ctx, auth, token, clientUsageCall{kind: clientUsageKindUsage, method: http.MethodPost, url: targetURL, body: body, headers: headers})
		responses = append(responses, response)
		if clientUsageUpstreamOK(response) || ctx.Err() != nil {
			break
		}
	}
	return append(responses, <-tierDone), nil
}

// probeXAIClientUsage returns both billing views the panel reads, the
// credit-based weekly period and the monthly subscription usage, plus the
// subscription tier.
func (h *Handler) probeXAIClientUsage(ctx context.Context, auth *coreauth.Auth, token string) []clientUsageUpstream {
	headers := map[string]string{
		"Accept":                "*/*",
		"User-Agent":            clientUsageXAIUserAgent,
		"X-Xai-Token-Auth":      clientUsageXAITokenAuth,
		"X-Grok-Client-Version": clientUsageXAIClientVersion,
	}
	// The panel reads the subscription without the user header.
	planHeaders := make(map[string]string, len(headers))
	for key, value := range headers {
		planHeaders[key] = value
	}
	if userID := clientUsageString(auth.Metadata, "sub", "subject", "user_id", "userId"); userID != "" {
		headers["X-Userid"] = userID
	}
	return h.clientUsageRequests(ctx, auth, token, []clientUsageCall{
		{kind: clientUsageKindBillingWeek, url: clientUsageXAIWeeklyURL, headers: headers},
		{kind: clientUsageKindBillingMonth, url: clientUsageXAIMonthlyURL, headers: headers},
		{kind: clientUsageKindUser, optional: true, url: clientUsageXAIUserURL, headers: planHeaders, fields: clientUsageXAIUserFields},
		{kind: clientUsageKindSettings, optional: true, url: clientUsageXAISettingsURL, headers: planHeaders, fields: clientUsageXAISettingsFields},
	})
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
