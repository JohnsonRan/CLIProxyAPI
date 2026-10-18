package management

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Response kinds tag each upstream response so the summarizers can find the
// body they need regardless of the order the probes ran in.
const (
	clientUsageKindUsage        = "usage"
	clientUsageKindSubscription = "subscription"
	clientUsageKindResetCredits = "reset_credits"
	clientUsageKindProfile      = "profile"
	clientUsageKindResetGrants  = "reset_grants"
	clientUsageKindTier         = "tier"
	clientUsageKindBillingWeek  = "billing_weekly"
	clientUsageKindBillingMonth = "billing_monthly"
	clientUsageKindUser         = "user"
	clientUsageKindSettings     = "settings"
)

const (
	clientUsageFiveHourSeconds = 5 * 60 * 60
	clientUsageWeekSeconds     = 7 * 24 * 60 * 60
	clientUsageMinMonthSeconds = 28 * 24 * 60 * 60
	clientUsageMaxMonthSeconds = 31 * 24 * 60 * 60
)

// clientUsageWindow is one rate-limit window. UsedPercent is 0-100 and null
// when the upstream does not report it.
type clientUsageWindow struct {
	ID            string     `json:"id"`
	Label         string     `json:"label,omitempty"`
	UsedPercent   *float64   `json:"used_percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	PeriodSeconds int64      `json:"period_seconds,omitempty"`
}

// clientUsageBalance is an amount-based allowance: credits, dollars, or a
// count such as remaining rate-limit resets. Unit is "usd", "credits" or
// "count", or an upstream currency code.
type clientUsageBalance struct {
	ID        string     `json:"id"`
	Label     string     `json:"label,omitempty"`
	Remaining *float64   `json:"remaining,omitempty"`
	Used      *float64   `json:"used,omitempty"`
	Limit     *float64   `json:"limit,omitempty"`
	Unit      string     `json:"unit,omitempty"`
	Unlimited bool       `json:"unlimited,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

// clientUsageSummary is the provider-neutral quota view of one credential.
type clientUsageSummary struct {
	Plan          string               `json:"plan,omitempty"`
	PlanExpiresAt *time.Time           `json:"plan_expires_at,omitempty"`
	Windows       []clientUsageWindow  `json:"windows"`
	Balances      []clientUsageBalance `json:"balances,omitempty"`
}

// summarizeClientUsage derives the quota summary from a credential's upstream
// responses. Failed or missing responses contribute nothing.
func summarizeClientUsage(provider string, responses []clientUsageUpstream, observedAt time.Time) clientUsageSummary {
	bodies := make(map[string]gjson.Result, len(responses))
	for _, response := range responses {
		if response.kind == "" || !clientUsageUpstreamOK(response) || len(response.Body) == 0 {
			continue
		}
		if _, seen := bodies[response.kind]; !seen {
			bodies[response.kind] = gjson.ParseBytes(response.Body)
		}
	}
	var summary clientUsageSummary
	switch provider {
	case "codex":
		summary = summarizeCodexUsage(bodies, observedAt)
	case "claude":
		summary = summarizeClaudeUsage(bodies)
	case "antigravity":
		summary = summarizeAntigravityUsage(bodies)
	case "xai":
		summary = summarizeXAIUsage(bodies)
	case clientUsageCommandCodeProvider:
		summary = summarizeCommandCodeUsage(bodies)
	}
	if summary.Windows == nil {
		summary.Windows = []clientUsageWindow{}
	}
	return summary
}

func summarizeCodexUsage(bodies map[string]gjson.Result, observedAt time.Time) clientUsageSummary {
	var summary clientUsageSummary
	usage, ok := bodies[clientUsageKindUsage]
	if ok {
		summary.Plan = clientUsageFirstString(usage, "plan_type", "planType")
		summary.Windows = append(summary.Windows, codexUsageWindows(clientUsageFirst(usage, "rate_limit", "rateLimit"), "", "", observedAt)...)
		summary.Windows = append(summary.Windows, codexUsageWindows(clientUsageFirst(usage, "code_review_rate_limit", "codeReviewRateLimit"), "code_review_", "", observedAt)...)
		for index, item := range clientUsageFirst(usage, "additional_rate_limits", "additionalRateLimits").Array() {
			name := clientUsageFirstString(item, "limit_name", "limitName", "metered_feature", "meteredFeature")
			if name == "" {
				name = "additional-" + strconv.Itoa(index+1)
			}
			prefix := clientUsageSlug(name) + "_"
			summary.Windows = append(summary.Windows, codexUsageWindows(clientUsageFirst(item, "rate_limit", "rateLimit"), prefix, name, observedAt)...)
		}
		if credits := usage.Get("credits"); credits.IsObject() {
			balance := clientUsageBalance{ID: "credits", Unit: "credits", Unlimited: credits.Get("unlimited").Bool()}
			balance.Remaining = clientUsageNumber(credits.Get("balance"))
			if balance.Remaining != nil || balance.Unlimited {
				summary.Balances = append(summary.Balances, balance)
			}
		}
	}
	if resets, ok := bodies[clientUsageKindResetCredits]; ok {
		count := clientUsageNumber(clientUsageFirst(resets, "available_count", "availableCount"))
		if count == nil && resets.Get("credits").IsArray() {
			count = clientUsageFloat(float64(len(resets.Get("credits").Array())))
		}
		if count != nil {
			summary.Balances = append(summary.Balances, clientUsageBalance{ID: "rate_limit_resets", Remaining: count, Unit: "count"})
		}
	}
	if subscription, ok := bodies[clientUsageKindSubscription]; ok {
		summary.PlanExpiresAt = clientUsageTime(clientUsageFirst(subscription, "active_until", "activeUntil"))
	}
	return summary
}

// codexUsageWindows classifies a Codex rate limit's primary and secondary
// windows by their length, falling back to their order for payloads without
// a length, as the panel does.
func codexUsageWindows(limit gjson.Result, idPrefix, label string, observedAt time.Time) []clientUsageWindow {
	if !limit.IsObject() {
		return nil
	}
	primary := clientUsageFirst(limit, "primary_window", "primaryWindow")
	secondary := clientUsageFirst(limit, "secondary_window", "secondaryWindow")
	var fiveHour, longer gjson.Result
	for _, window := range []gjson.Result{primary, secondary} {
		if !window.IsObject() {
			continue
		}
		seconds := codexWindowSeconds(window)
		switch {
		case seconds == clientUsageFiveHourSeconds && !fiveHour.Exists():
			fiveHour = window
		case (seconds == clientUsageWeekSeconds || codexMonthlySeconds(seconds)) && !longer.Exists():
			longer = window
		}
	}
	if !fiveHour.Exists() && primary.IsObject() && primary.Raw != longer.Raw {
		fiveHour = primary
	}
	if !longer.Exists() && secondary.IsObject() && secondary.Raw != fiveHour.Raw {
		longer = secondary
	}
	limitReached := clientUsageFirst(limit, "limit_reached", "limitReached").Bool()
	if allowed := limit.Get("allowed"); allowed.Exists() && !allowed.Bool() {
		limitReached = true
	}
	var out []clientUsageWindow
	if fiveHour.Exists() {
		out = append(out, codexUsageWindow(fiveHour, idPrefix+"five_hour", label, limitReached, observedAt))
	}
	if longer.Exists() {
		id := "weekly"
		if codexMonthlySeconds(codexWindowSeconds(longer)) {
			id = "monthly"
		}
		out = append(out, codexUsageWindow(longer, idPrefix+id, label, limitReached, observedAt))
	}
	return out
}

func codexUsageWindow(window gjson.Result, id, label string, limitReached bool, observedAt time.Time) clientUsageWindow {
	out := clientUsageWindow{ID: id, Label: label, PeriodSeconds: codexWindowSeconds(window)}
	out.UsedPercent = clientUsageNumber(clientUsageFirst(window, "used_percent", "usedPercent"))
	out.ResetAt = clientUsageTime(clientUsageFirst(window, "reset_at", "resetAt"))
	if out.ResetAt == nil {
		if after := clientUsageNumber(clientUsageFirst(window, "reset_after_seconds", "resetAfterSeconds")); after != nil && *after >= 0 {
			resetAt := observedAt.Add(time.Duration(*after * float64(time.Second))).UTC()
			out.ResetAt = &resetAt
		}
	}
	if out.UsedPercent == nil && limitReached && out.ResetAt != nil {
		out.UsedPercent = clientUsageFloat(100)
	}
	return out
}

func codexWindowSeconds(window gjson.Result) int64 {
	if seconds := clientUsageNumber(clientUsageFirst(window, "limit_window_seconds", "limitWindowSeconds")); seconds != nil {
		return int64(*seconds)
	}
	return 0
}

func codexMonthlySeconds(seconds int64) bool {
	return seconds >= clientUsageMinMonthSeconds && seconds <= clientUsageMaxMonthSeconds
}

// claudeUsageWindowKeys are the plan windows of the Claude OAuth usage
// response, in the panel's order. Only five_hour is a 5-hour window.
var claudeUsageWindowKeys = []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork"}

func summarizeClaudeUsage(bodies map[string]gjson.Result) clientUsageSummary {
	var summary clientUsageSummary
	if usage, ok := bodies[clientUsageKindUsage]; ok {
		known := map[string]struct{}{"extra_usage": {}, "limits": {}}
		for _, key := range claudeUsageWindowKeys {
			known[key] = struct{}{}
			window := usage.Get(key)
			if !window.IsObject() || !window.Get("utilization").Exists() {
				continue
			}
			period := int64(clientUsageWeekSeconds)
			if key == "five_hour" {
				period = clientUsageFiveHourSeconds
			}
			summary.Windows = append(summary.Windows, clientUsageWindow{
				ID:            key,
				UsedPercent:   clientUsageNumber(window.Get("utilization")),
				ResetAt:       clientUsageTime(window.Get("resets_at")),
				PeriodSeconds: period,
			})
		}
		fableFromLimits := false
		for _, limit := range usage.Get("limits").Array() {
			model := strings.ToLower(strings.TrimSpace(limit.Get("scope.model.display_name").String()))
			if strings.EqualFold(limit.Get("kind").String(), "weekly_scoped") && (model == "fable" || model == "fable 5") && limit.Get("percent").Exists() {
				summary.Windows = append(summary.Windows, clientUsageWindow{
					ID:            "seven_day_fable",
					UsedPercent:   clientUsageNumber(limit.Get("percent")),
					ResetAt:       clientUsageTime(limit.Get("resets_at")),
					PeriodSeconds: clientUsageWeekSeconds,
				})
				fableFromLimits = true
				break
			}
		}
		// Dollar-denominated pools (for example iguana_necktie on credit plans)
		// are balances; a plain iguana_necktie window is the Fable weekly limit.
		usage.ForEach(func(key, value gjson.Result) bool {
			name := key.String()
			if _, skip := known[name]; skip || !value.IsObject() {
				return true
			}
			if value.Get("limit_dollars").Exists() && value.Get("used_dollars").Exists() && value.Get("remaining_dollars").Exists() {
				summary.Balances = append(summary.Balances, clientUsageBalance{
					ID:        name,
					Remaining: clientUsageNumber(value.Get("remaining_dollars")),
					Used:      clientUsageNumber(value.Get("used_dollars")),
					Limit:     clientUsageNumber(value.Get("limit_dollars")),
					Unit:      "usd",
					ResetAt:   clientUsageTime(value.Get("resets_at")),
				})
			} else if name == "iguana_necktie" && !fableFromLimits && value.Get("utilization").Exists() {
				summary.Windows = append(summary.Windows, clientUsageWindow{
					ID:            "seven_day_fable",
					UsedPercent:   clientUsageNumber(value.Get("utilization")),
					ResetAt:       clientUsageTime(value.Get("resets_at")),
					PeriodSeconds: clientUsageWeekSeconds,
				})
			}
			return true
		})
		if extra := usage.Get("extra_usage"); extra.IsObject() && extra.Get("is_enabled").Bool() {
			// Extra-usage amounts are minor currency units.
			balance := clientUsageBalance{ID: "extra_usage", Unit: strings.ToLower(clientUsageFirstString(extra, "currency"))}
			if balance.Unit == "" {
				balance.Unit = "usd"
			}
			balance.Used = clientUsageCents(extra.Get("used_credits"))
			balance.Limit = clientUsageCents(extra.Get("monthly_limit"))
			if balance.Used != nil && balance.Limit != nil {
				balance.Remaining = clientUsageFloat(math.Max(0, *balance.Limit-*balance.Used))
			}
			summary.Balances = append(summary.Balances, balance)
		}
	}
	if profile, ok := bodies[clientUsageKindProfile]; ok {
		summary.Plan = claudePlan(profile)
	}
	if grants, ok := bodies[clientUsageKindResetGrants]; ok {
		block := grants.Get("cedar_ember")
		if block.IsObject() && block.Get("eligible").Bool() {
			var left, total float64
			for _, grant := range block.Get("grants").Array() {
				left += grant.Get("resets_left").Float()
				total += grant.Get("resets_total").Float()
			}
			summary.Balances = append(summary.Balances, clientUsageBalance{
				ID:        "rate_limit_resets",
				Remaining: clientUsageFloat(left),
				Limit:     clientUsageFloat(total),
				Unit:      "count",
				ResetAt:   clientUsageTime(block.Get("weekly_resets_at")),
			})
		}
	}
	return summary
}

// claudePlan mirrors the panel: an active Team organization wins, then the
// account's Max and Pro flags.
func claudePlan(profile gjson.Result) string {
	if strings.EqualFold(profile.Get("organization.organization_type").String(), "claude_team") &&
		strings.EqualFold(profile.Get("organization.subscription_status").String(), "active") {
		return "team"
	}
	maxFlag, proFlag := profile.Get("account.has_claude_max"), profile.Get("account.has_claude_pro")
	switch {
	case maxFlag.Bool():
		return "max"
	case proFlag.Bool():
		return "pro"
	case maxFlag.Exists() && proFlag.Exists():
		return "free"
	}
	return ""
}

var antigravityPlans = map[string]string{
	"free-tier":          "free",
	"g1-pro-tier":        "pro",
	"g1-ultra-tier":      "ultra",
	"g1-ultra-lite-tier": "ultra-lite",
}

func summarizeAntigravityUsage(bodies map[string]gjson.Result) clientUsageSummary {
	var summary clientUsageSummary
	if usage, ok := bodies[clientUsageKindUsage]; ok {
		for groupIndex, group := range usage.Get("groups").Array() {
			groupLabel := clientUsageFirstString(group, "displayName", "display_name")
			if groupLabel == "" {
				groupLabel = "Quota Group " + strconv.Itoa(groupIndex+1)
			}
			for bucketIndex, bucket := range group.Get("buckets").Array() {
				remaining := clientUsageNumber(clientUsageFirst(bucket, "remainingFraction", "remaining_fraction"))
				if remaining == nil {
					continue
				}
				window := clientUsageFirstString(bucket, "window")
				id := clientUsageFirstString(bucket, "bucketId", "bucket_id")
				if id == "" {
					id = clientUsageSlug(groupLabel) + "_" + clientUsageSlug(window) + "_" + strconv.Itoa(bucketIndex+1)
				}
				label := clientUsageFirstString(bucket, "displayName", "display_name", "description")
				if label == "" {
					label = window
				}
				used := math.Min(100, math.Max(0, (1-*remaining)*100))
				summary.Windows = append(summary.Windows, clientUsageWindow{
					ID:            id,
					Label:         strings.TrimSpace(groupLabel + " · " + label),
					UsedPercent:   &used,
					ResetAt:       clientUsageTime(clientUsageFirst(bucket, "resetTime", "reset_time")),
					PeriodSeconds: antigravityWindowSeconds(window),
				})
			}
		}
	}
	if tier, ok := bodies[clientUsageKindTier]; ok {
		current := clientUsageFirst(tier, "paidTier", "paid_tier")
		if current.Get("id").String() == "" {
			current = clientUsageFirst(tier, "currentTier", "current_tier")
		}
		if plan, known := antigravityPlans[current.Get("id").String()]; known {
			summary.Plan = plan
		} else {
			summary.Plan = clientUsageFirstString(current, "name", "id")
		}
	}
	return summary
}

func antigravityWindowSeconds(window string) int64 {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h", "five-hour", "five_hour":
		return clientUsageFiveHourSeconds
	case "weekly", "week":
		return clientUsageWeekSeconds
	}
	return 0
}

func summarizeXAIUsage(bodies map[string]gjson.Result) clientUsageSummary {
	var summary clientUsageSummary
	if weekly, ok := bodies[clientUsageKindBillingWeek]; ok {
		config := weekly.Get("config")
		if percent := clientUsageNumber(clientUsageFirst(config, "creditUsagePercent", "credit_usage_percent")); percent != nil {
			period := clientUsageFirst(config, "currentPeriod", "current_period")
			window := clientUsageWindow{ID: "weekly", UsedPercent: percent}
			window.ResetAt, window.PeriodSeconds = clientUsagePeriod(period.Get("start"), period.Get("end"))
			if window.PeriodSeconds == 0 {
				window.PeriodSeconds = clientUsageWeekSeconds
			}
			summary.Windows = append(summary.Windows, window)
		}
	}
	if monthly, ok := bodies[clientUsageKindBillingMonth]; ok {
		config := monthly.Get("config")
		limit := xaiCents(clientUsageFirst(config, "monthlyLimit", "monthly_limit"))
		used := xaiCents(config.Get("used"))
		if limit != nil && used != nil && *limit > 0 {
			window := clientUsageWindow{ID: "monthly", UsedPercent: clientUsageFloat(math.Min(*used, *limit) / *limit * 100)}
			window.ResetAt, window.PeriodSeconds = clientUsagePeriod(
				clientUsageFirst(config, "billingPeriodStart", "billing_period_start"),
				clientUsageFirst(config, "billingPeriodEnd", "billing_period_end"))
			summary.Windows = append(summary.Windows, window)
		}
		onDemandCap := xaiCents(clientUsageFirst(config, "onDemandCap", "on_demand_cap"))
		onDemandUsed := xaiCents(clientUsageFirst(config, "onDemandUsed", "on_demand_used"))
		if onDemandUsed == nil && used != nil && limit != nil {
			onDemandUsed = clientUsageFloat(math.Max(0, *used-*limit))
		}
		if onDemandCap != nil && *onDemandCap > 0 {
			balance := clientUsageBalance{ID: "on_demand", Unit: "usd", Limit: clientUsageFloat(*onDemandCap / 100)}
			if onDemandUsed != nil {
				balance.Used = clientUsageFloat(*onDemandUsed / 100)
				balance.Remaining = clientUsageFloat(math.Max(0, *onDemandCap-*onDemandUsed) / 100)
			}
			summary.Balances = append(summary.Balances, balance)
		}
		if prepaid := xaiCents(clientUsageFirst(config, "prepaidBalance", "prepaid_balance")); prepaid != nil {
			summary.Balances = append(summary.Balances, clientUsageBalance{ID: "prepaid", Unit: "usd", Remaining: clientUsageFloat(*prepaid / 100)})
		}
	}
	if settings, ok := bodies[clientUsageKindSettings]; ok {
		summary.Plan = clientUsageFirstString(settings, "subscription_tier_display", "subscriptionTierDisplay")
	}
	if user, ok := bodies[clientUsageKindUser]; ok && summary.Plan == "" {
		summary.Plan = clientUsageFirstString(user, "subscriptionTier", "subscription_tier")
	}
	return summary
}

// xaiCents reads an xAI amount in cents, either bare or as {"val": n}.
func xaiCents(value gjson.Result) *float64 {
	if value.IsObject() {
		return clientUsageNumber(value.Get("val"))
	}
	return clientUsageNumber(value)
}

// commandCodePlans is the official CLI's plan table: plan ID prefix to name
// and monthly credits in dollars.
var commandCodePlans = map[string]struct {
	name    string
	monthly float64
}{
	"individual-go":       {"Go", 10},
	"individual-go-v1":    {"Go", 10},
	"individual-goat":     {"GOAT", 70},
	"individual-pro":      {"Pro", 30},
	"individual-pro-v1":   {"Pro", 80},
	"individual-provider": {"Provider", 15},
	"individual-max":      {"Max", 150},
	"individual-ultra":    {"Ultra", 300},
	"teams-pro":           {"Teams Pro", 40},
}

func summarizeCommandCodeUsage(bodies map[string]gjson.Result) clientUsageSummary {
	var summary clientUsageSummary
	subscription := bodies[clientUsageKindSubscription].Get("data")
	planID := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(subscription.Get("planId").String())), "_", "-")
	var planKey string
	for key := range commandCodePlans {
		if strings.HasPrefix(planID, key) && len(key) > len(planKey) {
			planKey = key
		}
	}
	if planKey != "" {
		summary.Plan = commandCodePlans[planKey].name
	}
	credits, ok := bodies[clientUsageKindUsage]
	if !ok {
		return summary
	}
	for _, window := range []struct {
		key, id string
		period  int64
	}{{"fiveHour", "five_hour", clientUsageFiveHourSeconds}, {"weekly", "weekly", clientUsageWeekSeconds}} {
		raw := credits.Get("windowLimits." + window.key)
		capValue, used := clientUsageNumber(raw.Get("cap")), clientUsageNumber(raw.Get("used"))
		if capValue == nil || used == nil {
			continue
		}
		percent := 100.0
		if *capValue > 0 {
			percent = math.Min(100, *used / *capValue * 100)
		}
		summary.Windows = append(summary.Windows, clientUsageWindow{
			ID:            window.id,
			UsedPercent:   &percent,
			ResetAt:       clientUsageTime(raw.Get("resetAt")),
			PeriodSeconds: window.period,
		})
	}
	monthlyLeft := clientUsageNumber(credits.Get("credits.monthlyCredits"))
	// The API reports only the remaining monthly credits, so the used share
	// needs a known plan with an active subscription.
	if planKey != "" && monthlyLeft != nil && subscription.Get("status").String() == "active" {
		total := math.Max(commandCodePlans[planKey].monthly, math.Max(0, *monthlyLeft))
		percent := 0.0
		if total > 0 {
			percent = (total - math.Max(0, *monthlyLeft)) / total * 100
		}
		summary.Windows = append(summary.Windows, clientUsageWindow{
			ID:          "monthly",
			UsedPercent: &percent,
			ResetAt:     clientUsageTime(subscription.Get("currentPeriodEnd")),
		})
	}
	var left float64
	found := false
	for _, key := range []string{"monthlyCredits", "purchasedCredits", "freeCredits"} {
		if value := clientUsageNumber(credits.Get("credits." + key)); value != nil {
			left += math.Max(0, *value)
			found = true
		}
	}
	if found {
		summary.Balances = append(summary.Balances, clientUsageBalance{ID: "credits", Remaining: &left, Unit: "usd"})
	}
	return summary
}

// clientUsageFirst returns the first existing field among keys.
func clientUsageFirst(value gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if field := value.Get(key); field.Exists() && field.Type != gjson.Null {
			return field
		}
	}
	return gjson.Result{}
}

func clientUsageFirstString(value gjson.Result, keys ...string) string {
	for _, key := range keys {
		if field := value.Get(key); field.Type == gjson.String {
			if text := strings.TrimSpace(field.String()); text != "" {
				return text
			}
		}
	}
	return ""
}

// clientUsageNumber reads a finite number given as a JSON number or a numeric string.
func clientUsageNumber(value gjson.Result) *float64 {
	var number float64
	switch value.Type {
	case gjson.Number:
		number = value.Float()
	case gjson.String:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
		if errParse != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

func clientUsageCents(value gjson.Result) *float64 {
	if number := clientUsageNumber(value); number != nil {
		return clientUsageFloat(*number / 100)
	}
	return nil
}

func clientUsageFloat(value float64) *float64 { return &value }

// clientUsageTime reads an RFC 3339 string or a Unix time in seconds or
// milliseconds. Zero and negative values mean unknown.
func clientUsageTime(value gjson.Result) *time.Time {
	if value.Type == gjson.String {
		text := strings.TrimSpace(value.String())
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05"} {
			if parsed, errParse := time.Parse(layout, text); errParse == nil {
				parsed = parsed.UTC()
				return &parsed
			}
		}
	}
	number := clientUsageNumber(value)
	if number == nil || *number <= 0 {
		return nil
	}
	seconds := *number
	if seconds > 1e12 {
		seconds /= 1000
	}
	parsed := time.Unix(int64(seconds), 0).UTC()
	return &parsed
}

// clientUsagePeriod returns the reset instant and, when both ends are known,
// the period length.
func clientUsagePeriod(start, end gjson.Result) (*time.Time, int64) {
	resetAt := clientUsageTime(end)
	startAt := clientUsageTime(start)
	if resetAt != nil && startAt != nil && resetAt.After(*startAt) {
		return resetAt, int64(resetAt.Sub(*startAt) / time.Second)
	}
	return resetAt, 0
}

var clientUsageSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func clientUsageSlug(value string) string {
	return strings.Trim(clientUsageSlugPattern.ReplaceAllString(strings.ToLower(value), "_"), "_")
}
