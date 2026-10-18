package management

import (
	"encoding/json"
	"testing"
	"time"
)

func clientUsageTestResponses(bodies map[string]string) []clientUsageUpstream {
	out := make([]clientUsageUpstream, 0, len(bodies))
	for kind, body := range bodies {
		out = append(out, clientUsageUpstream{kind: kind, StatusCode: 200, Body: json.RawMessage(body)})
	}
	return out
}

func findClientUsageWindow(t *testing.T, summary clientUsageSummary, id string) clientUsageWindow {
	t.Helper()
	for _, window := range summary.Windows {
		if window.ID == id {
			return window
		}
	}
	t.Fatalf("window %q missing from %+v", id, summary.Windows)
	return clientUsageWindow{}
}

func findClientUsageBalance(t *testing.T, summary clientUsageSummary, id string) clientUsageBalance {
	t.Helper()
	for _, balance := range summary.Balances {
		if balance.ID == id {
			return balance
		}
	}
	t.Fatalf("balance %q missing from %+v", id, summary.Balances)
	return clientUsageBalance{}
}

func assertClientUsageWindow(t *testing.T, window clientUsageWindow, used float64, reset string, period int64) {
	t.Helper()
	if window.UsedPercent == nil || *window.UsedPercent != used {
		t.Fatalf("window %s used = %v, want %v", window.ID, window.UsedPercent, used)
	}
	if reset == "" {
		if window.ResetAt != nil {
			t.Fatalf("window %s reset = %v, want none", window.ID, window.ResetAt)
		}
	} else if window.ResetAt == nil || window.ResetAt.Format(time.RFC3339) != reset {
		t.Fatalf("window %s reset = %v, want %s", window.ID, window.ResetAt, reset)
	}
	if window.PeriodSeconds != period {
		t.Fatalf("window %s period = %d, want %d", window.ID, window.PeriodSeconds, period)
	}
}

func TestSummarizeCodexUsage(t *testing.T) {
	observed := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	summary := summarizeClientUsage("codex", clientUsageTestResponses(map[string]string{
		clientUsageKindUsage: `{"plan_type":"team","credits":{"balance":"12.5","unlimited":false},
			"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1760832000},
			              "secondary_window":{"used_percent":60,"limit_window_seconds":18000,"reset_after_seconds":3600}},
			"code_review_rate_limit":{"limit_reached":true,"primary_window":{"limit_window_seconds":18000,"reset_at":1760800000}},
			"additional_rate_limits":[{"limit_name":"GPT-5 Pro","rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":2592000}}}]}`,
		clientUsageKindResetCredits: `{"available_count":2,"credits":[{},{},{}]}`,
		clientUsageKindSubscription: `{"active_until":"2026-11-01T00:00:00Z"}`,
	}), observed)

	if summary.Plan != "team" || summary.PlanExpiresAt == nil || summary.PlanExpiresAt.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
		t.Fatalf("plan = %q expires %v", summary.Plan, summary.PlanExpiresAt)
	}
	// Windows are classified by length, not by primary/secondary order.
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "five_hour"), 60, "2026-10-18T01:00:00Z", 18000)
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "weekly"), 25, "2025-10-19T00:00:00Z", 604800)
	// A reached limit without a percentage reads as full.
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "code_review_five_hour"), 100, "2025-10-18T15:06:40Z", 18000)
	additional := findClientUsageWindow(t, summary, "gpt_5_pro_monthly")
	if additional.Label != "GPT-5 Pro" {
		t.Fatalf("additional label = %q", additional.Label)
	}
	if credits := findClientUsageBalance(t, summary, "credits"); *credits.Remaining != 12.5 || credits.Unit != "credits" {
		t.Fatalf("credits = %+v", credits)
	}
	if resets := findClientUsageBalance(t, summary, "rate_limit_resets"); *resets.Remaining != 2 {
		t.Fatalf("resets = %+v", resets)
	}
}

func TestSummarizeClaudeUsage(t *testing.T) {
	summary := summarizeClientUsage("claude", clientUsageTestResponses(map[string]string{
		clientUsageKindUsage: `{"five_hour":{"utilization":42,"resets_at":"2026-10-18T05:00:00+00:00"},
			"seven_day":{"utilization":10,"resets_at":"2026-10-20T00:00:00Z"},
			"seven_day_opus":null,
			"iguana_necktie":{"utilization":30,"resets_at":"2026-10-21T00:00:00Z"},
			"cloud_credits":{"limit_dollars":50,"used_dollars":20,"remaining_dollars":30,"utilization":40,"resets_at":null},
			"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1250,"currency":"USD"}}`,
		clientUsageKindProfile:     `{"account":{"has_claude_max":true,"has_claude_pro":false}}`,
		clientUsageKindResetGrants: `{"cedar_ember":{"eligible":true,"grants":[{"resets_left":1,"resets_total":2},{"resets_left":2,"resets_total":2}]}}`,
	}), time.Now())

	if summary.Plan != "max" {
		t.Fatalf("plan = %q", summary.Plan)
	}
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "five_hour"), 42, "2026-10-18T05:00:00Z", 18000)
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "seven_day"), 10, "2026-10-20T00:00:00Z", 604800)
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "seven_day_fable"), 30, "2026-10-21T00:00:00Z", 604800)
	if pool := findClientUsageBalance(t, summary, "cloud_credits"); *pool.Remaining != 30 || *pool.Limit != 50 || pool.Unit != "usd" {
		t.Fatalf("dollar pool = %+v", pool)
	}
	// Extra usage is reported in minor units upstream.
	if extra := findClientUsageBalance(t, summary, "extra_usage"); *extra.Used != 12.5 || *extra.Limit != 50 || *extra.Remaining != 37.5 || extra.Unit != "usd" {
		t.Fatalf("extra usage = %+v", extra)
	}
	if resets := findClientUsageBalance(t, summary, "rate_limit_resets"); *resets.Remaining != 3 || *resets.Limit != 4 {
		t.Fatalf("reset grants = %+v", resets)
	}
}

func TestSummarizeAntigravityUsage(t *testing.T) {
	summary := summarizeClientUsage("antigravity", clientUsageTestResponses(map[string]string{
		clientUsageKindUsage: `{"groups":[{"displayName":"Gemini","buckets":[
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.75,"resetTime":"2026-10-18T03:00:00Z"},
			{"bucketId":"gemini-week","window":"weekly","remainingFraction":"0.2"},
			{"bucketId":"broken"}]}]}`,
		clientUsageKindTier: `{"currentTier":{"id":"free-tier"},"paidTier":{"id":"g1-ultra-tier","name":"Ultra"}}`,
	}), time.Now())

	if summary.Plan != "ultra" || len(summary.Windows) != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	fiveHour := findClientUsageWindow(t, summary, "gemini-5h")
	assertClientUsageWindow(t, fiveHour, 25, "2026-10-18T03:00:00Z", 18000)
	if fiveHour.Label != "Gemini · 5h" {
		t.Fatalf("label = %q", fiveHour.Label)
	}
	week := findClientUsageWindow(t, summary, "gemini-week")
	if week.UsedPercent == nil || *week.UsedPercent < 79.99 || *week.UsedPercent > 80.01 || week.PeriodSeconds != 604800 {
		t.Fatalf("weekly = %+v", week)
	}
}

func TestSummarizeXAIUsage(t *testing.T) {
	summary := summarizeClientUsage("xai", clientUsageTestResponses(map[string]string{
		clientUsageKindBillingWeek:  `{"config":{"creditUsagePercent":35,"currentPeriod":{"type":"weekly","start":"2026-10-13T00:00:00Z","end":"2026-10-20T00:00:00Z"}}}`,
		clientUsageKindBillingMonth: `{"config":{"monthlyLimit":{"val":3000},"used":{"val":4500},"onDemandCap":{"val":2000},"prepaidBalance":"250","billingPeriodEnd":"2026-11-01T00:00:00Z"}}`,
		clientUsageKindSettings:     `{"subscription_tier_display":"SuperGrok Heavy"}`,
		clientUsageKindUser:         `{"subscriptionTier":"heavy"}`,
	}), time.Now())

	if summary.Plan != "SuperGrok Heavy" {
		t.Fatalf("plan = %q", summary.Plan)
	}
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "weekly"), 35, "2026-10-20T00:00:00Z", 604800)
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "monthly"), 100, "2026-11-01T00:00:00Z", 0)
	if onDemand := findClientUsageBalance(t, summary, "on_demand"); *onDemand.Used != 15 || *onDemand.Limit != 20 || *onDemand.Remaining != 5 {
		t.Fatalf("on demand = %+v", onDemand)
	}
	if prepaid := findClientUsageBalance(t, summary, "prepaid"); *prepaid.Remaining != 2.5 {
		t.Fatalf("prepaid = %+v", prepaid)
	}
}

func TestSummarizeCommandCodeUsage(t *testing.T) {
	summary := summarizeClientUsage(clientUsageCommandCodeProvider, clientUsageTestResponses(map[string]string{
		clientUsageKindUsage:        `{"credits":{"monthlyCredits":60,"purchasedCredits":5,"freeCredits":-1},"windowLimits":{"fiveHour":{"cap":4,"used":1,"resetAt":"2026-10-18T02:00:00Z"},"weekly":{"cap":0,"used":0}}}`,
		clientUsageKindSubscription: `{"data":{"planId":"individual-pro-v1-monthly","status":"active","currentPeriodEnd":"2026-11-01T00:00:00Z"}}`,
	}), time.Now())

	if summary.Plan != "Pro" {
		t.Fatalf("plan = %q", summary.Plan)
	}
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "five_hour"), 25, "2026-10-18T02:00:00Z", 18000)
	// A zero cap reads as exhausted, like the panel.
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "weekly"), 100, "", 604800)
	assertClientUsageWindow(t, findClientUsageWindow(t, summary, "monthly"), 25, "2026-11-01T00:00:00Z", 0)
	if credits := findClientUsageBalance(t, summary, "credits"); *credits.Remaining != 65 {
		t.Fatalf("credits = %+v", credits)
	}
}

// Failed and optional-only responses contribute nothing, and the summary
// always serializes windows as a list.
func TestSummarizeClientUsageIgnoresFailedResponses(t *testing.T) {
	summary := summarizeClientUsage("claude", []clientUsageUpstream{
		{kind: clientUsageKindUsage, StatusCode: 429, Body: json.RawMessage(`{"five_hour":{"utilization":99}}`)},
	}, time.Now())
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"windows":[]}` {
		t.Fatalf("summary = %s", encoded)
	}
	if clientUsageAnyOK([]clientUsageUpstream{{optional: true, StatusCode: 200}}) {
		t.Fatal("an optional response alone counted as a successful probe")
	}
}
