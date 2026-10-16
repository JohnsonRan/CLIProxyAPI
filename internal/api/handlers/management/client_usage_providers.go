package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Upstream quota endpoints and client headers mirror the management panel's
// quota probes so the upstream sees the same traffic as a panel refresh.
const (
	clientUsageCodexURL          = "https://chatgpt.com/backend-api/wham/usage"
	clientUsageCodexUserAgent    = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	clientUsageClaudeURL         = "https://api.anthropic.com/api/oauth/usage"
	clientUsageClaudeUserAgent   = "claude-cli/2.1.280 (external, cli)"
	clientUsageClaudeBeta        = "oauth-2025-04-20"
	clientUsageAntigravityUA     = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	clientUsageXAIWeeklyURL      = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	clientUsageXAIMonthlyURL     = "https://cli-chat-proxy.grok.com/v1/billing"
	clientUsageXAIClientVersion  = "1.0.46"
	clientUsageXAIUserAgent      = "grok-pager/" + clientUsageXAIClientVersion + " grok-shell/" + clientUsageXAIClientVersion + " (macos; aarch64)"
	clientUsageXAITokenAuth      = "xai-grok-cli"
	clientUsageXAISuperGrokCents = 15_000
	clientUsageXAIHeavyCents     = 150_000

	clientUsageHour = int64(60 * 60)
	clientUsageDay  = 24 * clientUsageHour
	clientUsageWeek = 7 * clientUsageDay
)

var clientUsageAntigravityURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// clientUsageRequest sends one quota request. Errors never include upstream
// bodies or credentials because they are shown to client API key holders.
func (h *Handler) clientUsageRequest(ctx context.Context, auth *coreauth.Auth, token, method, targetURL string, body []byte, headers map[string]string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if errRequest != nil {
		return nil, fmt.Errorf("build quota request failed")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("upstream quota request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("client usage: response body close error: %v", errClose)
		}
	}()
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, clientUsageMaxResponseBytes+1))
	if errRead != nil || len(payload) > clientUsageMaxResponseBytes {
		return nil, fmt.Errorf("upstream quota response unreadable")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("upstream quota endpoint returned HTTP %d", resp.StatusCode)
	}
	if !gjson.ValidBytes(payload) {
		return nil, fmt.Errorf("upstream quota response is not JSON")
	}
	return payload, nil
}

func (h *Handler) probeCodexClientUsage(ctx context.Context, auth *coreauth.Auth, token string, observedAt time.Time) (string, []clientUsageWindow, error) {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": clientUsageCodexUserAgent}
	if accountID := clientUsageString(auth.Metadata, "account_id", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	body, errRequest := h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageCodexURL, nil, headers)
	if errRequest != nil {
		return "", nil, errRequest
	}
	plan, windows := parseCodexClientUsage(body, observedAt)
	return plan, windows, nil
}

func parseCodexClientUsage(body []byte, observedAt time.Time) (string, []clientUsageWindow) {
	root := gjson.ParseBytes(body)
	windows := appendCodexClientWindows(nil, root.Get("rate_limit"), "", observedAt)
	windows = appendCodexClientWindows(windows, root.Get("code_review_rate_limit"), "code-review", observedAt)
	root.Get("additional_rate_limits").ForEach(func(_, item gjson.Result) bool {
		scope := firstNonEmptyValue(item.Get("limit_name").String(), item.Get("metered_feature").String())
		if scope != "" {
			windows = appendCodexClientWindows(windows, item.Get("rate_limit"), scope, observedAt)
		}
		return true
	})
	return strings.TrimSpace(root.Get("plan_type").String()), windows
}

func appendCodexClientWindows(windows []clientUsageWindow, limit gjson.Result, scope string, observedAt time.Time) []clientUsageWindow {
	for _, key := range []string{"primary_window", "secondary_window"} {
		window := limit.Get(key)
		used, okUsed := clientUsageNumber(window.Get("used_percent"))
		seconds := window.Get("limit_window_seconds").Int()
		if !okUsed || seconds <= 0 {
			continue
		}
		var resetAt *time.Time
		if unix := window.Get("reset_at").Int(); unix > 0 {
			value := time.Unix(unix, 0).UTC()
			resetAt = &value
		} else if after := window.Get("reset_after_seconds"); after.Exists() && after.Int() >= 0 {
			value := observedAt.Add(time.Duration(after.Int()) * time.Second)
			resetAt = &value
		}
		windows = append(windows, newClientUsageWindow(seconds, scope, used, resetAt))
	}
	return windows
}

func (h *Handler) probeClaudeClientUsage(ctx context.Context, auth *coreauth.Auth, token string) ([]clientUsageWindow, error) {
	headers := map[string]string{
		"Content-Type":   "application/json",
		"User-Agent":     clientUsageClaudeUserAgent,
		"Anthropic-Beta": clientUsageClaudeBeta,
	}
	body, errRequest := h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageClaudeURL, nil, headers)
	if errRequest != nil {
		return nil, errRequest
	}
	return parseClaudeClientUsage(body), nil
}

var clientUsageClaudeWindows = []struct {
	key     string
	seconds int64
	scope   string
}{
	{"five_hour", 5 * clientUsageHour, ""},
	{"seven_day", clientUsageWeek, ""},
	{"seven_day_oauth_apps", clientUsageWeek, "oauth-apps"},
	{"seven_day_opus", clientUsageWeek, "opus"},
	{"seven_day_sonnet", clientUsageWeek, "sonnet"},
	{"seven_day_cowork", clientUsageWeek, "cowork"},
	{"iguana_necktie", clientUsageWeek, "fable"},
}

func parseClaudeClientUsage(body []byte) []clientUsageWindow {
	root := gjson.ParseBytes(body)
	var windows []clientUsageWindow
	for _, spec := range clientUsageClaudeWindows {
		window := root.Get(spec.key)
		used, ok := clientUsageNumber(window.Get("utilization"))
		if !ok {
			continue
		}
		windows = append(windows, newClientUsageWindow(spec.seconds, spec.scope, used, clientUsageTime(window.Get("resets_at").String())))
	}
	return windows
}

func (h *Handler) probeAntigravityClientUsage(ctx context.Context, auth *coreauth.Auth, token string) ([]clientUsageWindow, error) {
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
	lastErr := fmt.Errorf("upstream returned no quota windows")
	for _, targetURL := range clientUsageAntigravityURLs {
		payload, errRequest := h.clientUsageRequest(ctx, auth, token, http.MethodPost, targetURL, body, headers)
		if errRequest != nil {
			lastErr = errRequest
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if windows := parseAntigravityClientUsage(payload); len(windows) > 0 {
			return windows, nil
		}
	}
	return nil, lastErr
}

var clientUsageAntigravityBuckets = map[string]struct {
	seconds int64
	scope   string
}{
	"gemini-5h":     {5 * clientUsageHour, "gemini"},
	"3p-5h":         {5 * clientUsageHour, "third-party"},
	"gemini-weekly": {clientUsageWeek, "gemini"},
	"3p-weekly":     {clientUsageWeek, "third-party"},
}

func parseAntigravityClientUsage(body []byte) []clientUsageWindow {
	var windows []clientUsageWindow
	seen := make(map[string]struct{})
	gjson.GetBytes(body, "groups").ForEach(func(_, group gjson.Result) bool {
		group.Get("buckets").ForEach(func(_, bucket gjson.Result) bool {
			id := strings.TrimSpace(bucket.Get("bucketId").String())
			spec, ok := clientUsageAntigravityBuckets[id]
			if !ok || bucket.Get("disabled").Bool() {
				return true
			}
			if _, dup := seen[id]; dup {
				return true
			}
			remaining, okRemaining := clientUsageFraction(bucket.Get("remainingFraction"))
			if !okRemaining {
				return true
			}
			seen[id] = struct{}{}
			windows = append(windows, newClientUsageWindow(spec.seconds, spec.scope, (1-remaining)*100, clientUsageTime(bucket.Get("resetTime").String())))
			return true
		})
		return true
	})
	return windows
}

func (h *Handler) probeXAIClientUsage(ctx context.Context, auth *coreauth.Auth, token string) (string, []clientUsageWindow, error) {
	headers := map[string]string{
		"Accept":                "*/*",
		"User-Agent":            clientUsageXAIUserAgent,
		"X-Xai-Token-Auth":      clientUsageXAITokenAuth,
		"X-Grok-Client-Version": clientUsageXAIClientVersion,
	}
	if userID := clientUsageString(auth.Metadata, "sub", "subject", "user_id", "userId"); userID != "" {
		headers["X-Userid"] = userID
	}
	weekly, errWeekly := h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageXAIWeeklyURL, nil, headers)
	if errWeekly == nil {
		if window, ok := parseXAIWeeklyClientUsage(weekly); ok {
			return "", []clientUsageWindow{window}, nil
		}
	}
	monthly, errMonthly := h.clientUsageRequest(ctx, auth, token, http.MethodGet, clientUsageXAIMonthlyURL, nil, headers)
	if errMonthly != nil {
		return "", nil, errMonthly
	}
	plan, window, ok := parseXAIMonthlyClientUsage(monthly)
	if !ok {
		return plan, nil, nil
	}
	return plan, []clientUsageWindow{window}, nil
}

// parseXAIWeeklyClientUsage reads the credit-based (weekly) billing period.
func parseXAIWeeklyClientUsage(body []byte) (clientUsageWindow, bool) {
	config := gjson.GetBytes(body, "config")
	used, ok := clientUsageNumber(clientUsageFirst(config, "creditUsagePercent", "credit_usage_percent"))
	if !ok {
		return clientUsageWindow{}, false
	}
	period := clientUsageFirst(config, "currentPeriod", "current_period")
	start := clientUsageTime(period.Get("start").String())
	end := clientUsageTime(period.Get("end").String())
	seconds := clientUsageWeek
	if start != nil && end != nil && end.After(*start) {
		seconds = int64(end.Sub(*start).Round(time.Hour) / time.Second)
	}
	return newClientUsageWindow(seconds, "", used, end), true
}

// parseXAIMonthlyClientUsage reads the legacy monthly spend limit in cents.
func parseXAIMonthlyClientUsage(body []byte) (string, clientUsageWindow, bool) {
	config := gjson.GetBytes(body, "config")
	limit, okLimit := clientUsageNumber(clientUsageFirst(config, "monthlyLimit", "monthly_limit"))
	plan := ""
	switch limit {
	case clientUsageXAISuperGrokCents:
		plan = "SuperGrok"
	case clientUsageXAIHeavyCents:
		plan = "SuperGrok Heavy"
	}
	used, okUsed := clientUsageNumber(config.Get("used"))
	if !okLimit || !okUsed || limit <= 0 {
		return plan, clientUsageWindow{}, false
	}
	end := clientUsageTime(clientUsageFirst(config, "billingPeriodEnd", "billing_period_end").String())
	return plan, newClientUsageWindow(30*clientUsageDay, "", used/limit*100, end), true
}

// clientUsageNumber accepts JSON numbers, numeric strings, and {"val": n} objects.
func clientUsageNumber(value gjson.Result) (float64, bool) {
	switch value.Type {
	case gjson.Number:
		return value.Float(), true
	case gjson.String:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value.String()), "%")), 64)
		return parsed, errParse == nil
	case gjson.JSON:
		if value.IsObject() {
			return clientUsageNumber(value.Get("val"))
		}
	}
	return 0, false
}

// clientUsageFraction reads a 0..1 fraction; percentage strings ("75%") are scaled.
func clientUsageFraction(value gjson.Result) (float64, bool) {
	number, ok := clientUsageNumber(value)
	if !ok {
		return 0, false
	}
	if value.Type == gjson.String && strings.HasSuffix(strings.TrimSpace(value.String()), "%") {
		number /= 100
	}
	if number < 0 || number > 1 {
		return 0, false
	}
	return number, true
}

func clientUsageFirst(value gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if result := value.Get(key); result.Exists() {
			return result
		}
	}
	return gjson.Result{}
}

func clientUsageTime(value string) *time.Time {
	parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if errParse != nil {
		return nil
	}
	utc := parsed.UTC()
	return &utc
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
