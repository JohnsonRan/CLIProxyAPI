package usagestore

import (
	"context"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseQuotaWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	codex := parseQuotaWindows("codex", map[string]string{
		"X-Codex-Primary-Used-Percent":           "12",
		"X-Codex-Primary-Window-Minutes":         "300",
		"X-Codex-Primary-Reset-After-Seconds":    "600",
		"X-Codex-Secondary-Used-Percent":         "40.5",
		"X-Codex-Secondary-Window-Minutes":       "10080",
		"X-Codex-Secondary-Reset-At":             "1800100000",
		"X-Codex-Bengalfox-Primary-Used-Percent": "99",
	}, now)
	if len(codex) != 2 ||
		codex[0] != (quotaWindow{ID: "primary", Period: 18000, ResetAt: now.Unix() + 600, Used: 12}) ||
		codex[1] != (quotaWindow{ID: "secondary", Period: 604800, ResetAt: 1800100000, Used: 40.5}) {
		t.Fatalf("codex windows = %+v", codex)
	}
	claude := parseQuotaWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset":          "1800010000",
		"Anthropic-Ratelimit-Unified-7d-Utilization":    "bad",
		"Anthropic-Ratelimit-Unified-7d-Reset":          "1800200000",
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "1.02",
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       "1800200000",
	}, now)
	if len(claude) != 2 || claude[0].ID != "5h" || claude[0].Period != 18000 || claude[0].ResetAt != 1800010000 ||
		claude[1].ID != "7d_oi" || claude[1].Period != 604800 || claude[1].ResetAt != 1800200000 {
		t.Fatalf("claude windows = %+v", claude)
	}
	approx(t, "claude used", claude[0].Used, 23)
	approx(t, "claude fable used", claude[1].Used, 102)
	rfc := parseQuotaWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
		"Anthropic-Ratelimit-Unified-7d-Reset":       time.Unix(1800200000, 0).UTC().Format(time.RFC3339),
	}, now)
	if len(rfc) != 1 || rfc[0].ResetAt != 1800200000 || rfc[0].Used != 50 {
		t.Fatalf("RFC3339 reset = %+v", rfc)
	}
	if got := parseQuotaWindows("devin", map[string]string{"X-Codex-Primary-Used-Percent": "1"}, now); got != nil {
		t.Fatalf("unsupported provider parsed: %+v", got)
	}
}

func TestQuotaValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	// gpt-x: $1/M input, $10/M output, so each event costs $2.
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, ago := range []time.Duration{3 * time.Hour, 90 * time.Minute, 30 * time.Minute, 2 * time.Minute} {
		s.Enqueue(Event{Timestamp: now.Add(-ago).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	}
	s.Enqueue(Event{Timestamp: now.Add(-time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "other", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	primaryReset := now.Add(3 * time.Hour).Unix() // 5h window started 2h ago
	secondaryReset := now.Add(6 * 24 * time.Hour).Unix()
	snapshot := func(observed time.Time, primary, secondary int, resetAfterJitter int64) []*coreauth.Auth {
		quota := coreauth.QuotaState{
			ObservedAt: observed,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        strconv.Itoa(primary),
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-After-Seconds": strconv.FormatInt(primaryReset+resetAfterJitter-observed.Unix(), 10),
				"X-Codex-Secondary-Used-Percent":      strconv.Itoa(secondary),
				"X-Codex-Secondary-Window-Minutes":    "10080",
				"X-Codex-Secondary-Reset-At":          strconv.FormatInt(secondaryReset, 10),
			},
		}
		// An API key relaying upstream quota headers must not be sampled.
		apiKey := &coreauth.Auth{ID: "k1", Index: "k1", Provider: "codex", Quota: quota.Clone(),
			Attributes: map[string]string{coreauth.AttributeAPIKey: "sk-test"}}
		return []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: quota}, apiKey}
	}
	last := map[string]quotaLast{}
	for _, step := range []struct {
		observed           time.Time
		primary, secondary int
		jitter             int64
	}{
		{now.Add(-60 * time.Minute), 10, 2, 0},
		{now.Add(-50 * time.Minute), 10, 2, 1}, // unchanged: not stored
		{now.Add(-10 * time.Minute), 20, 3, -1},
		{now.Add(-5 * time.Minute), 15, 3, 0}, // stale out-of-order snapshot: must not lower the window
	} {
		auths := snapshot(step.observed, step.primary, step.secondary, step.jitter)
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	var stored int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&stored); err != nil || stored != 5 {
		t.Fatalf("stored samples = %d (%v), want 5", stored, err)
	}

	value, err := s.QuotaValue(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 {
		t.Fatalf("windows = %+v", value.Windows)
	}
	secondary, primary := value.Windows[0], value.Windows[1]
	if primary.Window != "primary" || primary.StartAt != primaryReset-1-18000 || primary.Samples != 3 ||
		primary.UsedPercent != 20 || primary.ObservedAt != now.Add(-10*time.Minute).UnixMilli() {
		t.Fatalf("primary = %+v", primary)
	}
	// Events at -90m and -30m fall inside [start, 20% first seen]; -3h is before the window, -2m after.
	if primary.Usage.Requests != 2 || primary.FullCost == nil || primary.FullTokens == nil {
		t.Fatalf("primary usage = %+v", primary)
	}
	approx(t, "primary cost", primary.Usage.Cost, 4)
	approx(t, "primary full cost", *primary.FullCost, 20)
	approx(t, "primary full tokens", *primary.FullTokens, 11_000_000)
	if secondary.Window != "secondary" || secondary.Usage.Requests != 3 {
		t.Fatalf("secondary = %+v", secondary)
	}
	approx(t, "secondary full cost", *secondary.FullCost, 200)

	filtered, err := s.QuotaValue(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), []string{"other"})
	if err != nil || len(filtered.Windows) != 0 {
		t.Fatalf("auth filter: %+v %v", filtered, err)
	}
}

func TestGroupQuotaSamplesSplitsOnReset(t *testing.T) {
	got := groupQuotaSamples([]quotaSample{
		{authIndex: "a", window: "5h", period: 18000, resetAt: 100_000, used: 50, observed: 1},
		{authIndex: "a", window: "5h", period: 18000, resetAt: 100_000 + 1000, used: 60, observed: 2},
		{authIndex: "a", window: "5h", period: 18000, resetAt: 120_000, used: 0, observed: 3},
		{authIndex: "a", window: "7d", period: 604800, resetAt: 120_000, used: 1, observed: 3},
	})
	if len(got) != 3 || got[0].UsedPercent != 60 || got[0].ObservedAt != 2 || got[1].UsedPercent != 0 || got[2].Window != "7d" {
		t.Fatalf("groups = %+v", got)
	}
}

func TestGroupQuotaSamplesEarlyReset(t *testing.T) {
	const week, r = 604800, 1_000_000
	sample := func(reset int64, used float64, observed int64) quotaSample {
		return quotaSample{authIndex: "a", window: "7d", period: week, resetAt: reset, used: used, observed: observed}
	}
	// Reset 2 days into the window: the new reset is 2 days after the old one, within period/2.
	got := groupQuotaSamples([]quotaSample{
		sample(r, 30, 1),
		sample(r, 40, 2),
		sample(r+2*86400, 0, 3),
		sample(r, 41, 4), // late stale snapshot of the old window
		sample(r+2*86400, 10, 5),
	})
	if len(got) != 2 || got[0].ResetAt != r || got[0].UsedPercent != 41 || got[0].Samples != 3 ||
		got[1].ResetAt != r+2*86400 || got[1].UsedPercent != 10 || got[1].Samples != 2 {
		t.Fatalf("groups = %+v", got)
	}

	// A stale snapshot that lowers the percentage only slightly, or a reset that moved only a few
	// minutes, stays in the same instance.
	for name, samples := range map[string][]quotaSample{
		"small drop":  {sample(r, 40, 1), sample(r+20*60, 35, 2)},
		"small shift": {sample(r, 10, 1), sample(r+5*60, 0, 2)},
	} {
		if got := groupQuotaSamples(samples); len(got) != 1 {
			t.Fatalf("%s: groups = %+v", name, got)
		}
	}
}

func TestQuotaValueEarlyReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	// Each event costs $2.
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, ago := range []time.Duration{50 * time.Hour, 30 * time.Hour, 20 * time.Hour, 2 * time.Hour} {
		s.Enqueue(Event{Timestamp: now.Add(-ago).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// The weekly window started 3 days ago and was reset early 1 day ago.
	oldReset := now.Add(4 * 24 * time.Hour).Unix()
	earlyReset := now.Add(-24 * time.Hour)
	newReset := earlyReset.Add(7 * 24 * time.Hour).Unix()
	last := map[string]quotaLast{}
	for _, step := range []struct {
		ago   time.Duration
		reset int64
		used  int
	}{
		{48 * time.Hour, oldReset, 20},
		{26 * time.Hour, oldReset, 40},
		{23 * time.Hour, newReset, 2},
		{time.Hour, newReset, 10},
	} {
		auths := []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: coreauth.QuotaState{
			ObservedAt: now.Add(-step.ago),
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   strconv.Itoa(step.used),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(step.reset, 10),
			},
		}}}
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	var storedNew int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE reset_at = ?`, newReset).Scan(&storedNew); err != nil || storedNew != 2 {
		t.Fatalf("samples stored with the new reset = %d (%v), want 2", storedNew, err)
	}

	value, err := s.QuotaValue(context.Background(), now.Add(-7*24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 {
		t.Fatalf("windows = %+v", value.Windows)
	}
	newer, older := value.Windows[0], value.Windows[1]
	// The old instance ends at the early reset; the new one counts usage from there, including
	// events before the old nominal reset.
	if older.ResetAt != earlyReset.Unix() || older.UsedPercent != 40 || older.Usage.Requests != 2 {
		t.Fatalf("older = %+v", older)
	}
	if newer.ResetAt != newReset || newer.StartAt != earlyReset.Unix() || newer.UsedPercent != 10 || newer.Usage.Requests != 2 {
		t.Fatalf("newer = %+v", newer)
	}
	approx(t, "older cost", older.Usage.Cost, 4)
	approx(t, "newer cost", newer.Usage.Cost, 4)
}

func TestQuotaEarlyResetBoundaries(t *testing.T) {
	const r = 1_000_000
	for _, tc := range []struct {
		name      string
		prevReset int64
		prevUsed  float64
		reset     int64
		used      float64
		want      bool
	}{
		// Shift boundary: must be > 900s (15 min)
		{"shift exactly 900s", r, 20, r + 900, 10, false},
		{"shift 901s", r, 20, r + 901, 10, true},
		// Drop boundary: prevUsed - used must be >= 5
		{"drop 4.9 points", r, 10, r + 1000, 5.1, false},
		{"drop 4.9 points to below half", r, 8, r + 1000, 3.1, false},
		{"drop exactly 5 points", r, 10, r + 1000, 5.0, true},
		// Ratio boundary: used must be <= prevUsed / 2
		{"used half", r, 20, r + 1000, 10.0, true},
		{"used just above half", r, 20, r + 1000, 10.1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaEarlyReset(tc.prevReset, tc.prevUsed, tc.reset, tc.used); got != tc.want {
				t.Errorf("quotaEarlyReset(%d, %v, %d, %v) = %v, want %v", tc.prevReset, tc.prevUsed, tc.reset, tc.used, got, tc.want)
			}
		})
	}
}

func TestGroupQuotaSamplesStaleLongStreamDoesNotSplit(t *testing.T) {
	const r = 1_000_000
	// 5h window (18000s). Stream A started early in the window and completed at start+1260s.
	// Its relative reset shifted by 1200s and reported 1% used against earlier 20%.
	// A fresh stream at start+1300s reports reset r and 22%.
	// The stale completion must NOT split the window into two.
	got := groupQuotaSamples([]quotaSample{
		{authIndex: "a", window: "5h", period: 18000, resetAt: r, used: 20, observed: 1},
		{authIndex: "a", window: "5h", period: 18000, resetAt: r + 1200, used: 1, observed: 2},
		{authIndex: "a", window: "5h", period: 18000, resetAt: r, used: 22, observed: 3},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 window instance, got %d: %+v", len(got), got)
	}
	if got[0].ResetAt != r || got[0].UsedPercent != 22 || got[0].Samples != 3 {
		t.Fatalf("instance = %+v", got[0])
	}

	// The next natural window must not confirm the stale snapshot as an early reset afterwards.
	got = groupQuotaSamples([]quotaSample{
		{authIndex: "a", window: "5h", period: 18000, resetAt: r, used: 20, observed: 1},
		{authIndex: "a", window: "5h", period: 18000, resetAt: r + 1200, used: 1, observed: 2},
		{authIndex: "a", window: "5h", period: 18000, resetAt: r, used: 22, observed: 3},
		{authIndex: "a", window: "5h", period: 18000, resetAt: r + 18000, used: 2, observed: 4},
	})
	if len(got) != 2 || got[0].ResetAt != r || got[0].Samples != 3 || got[1].ResetAt != r+18000 {
		t.Fatalf("after natural rollover: %+v", got)
	}
}

func TestQuotaValueStaleLongStreamEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, ago := range []time.Duration{80 * time.Minute, 40 * time.Minute, 5 * time.Minute} {
		s.Enqueue(Event{Timestamp: now.Add(-ago).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	primaryReset := now.Add(3 * time.Hour).Unix() // 5h window started 2h ago
	last := map[string]quotaLast{}
	for _, step := range []struct {
		ago        time.Duration
		used       int
		resetAfter int64
	}{
		// Normal stream: reset is primaryReset
		{60 * time.Minute, 20, primaryReset - now.Add(-60*time.Minute).Unix()},
		// Stale long stream completes: its headers were read 20 min before completion, so the inferred reset shifts 1200s forward
		{30 * time.Minute, 1, (primaryReset + 1200) - now.Add(-30*time.Minute).Unix()},
		// Fresh stream: reset is primaryReset
		{10 * time.Minute, 25, primaryReset - now.Add(-10*time.Minute).Unix()},
	} {
		obs := now.Add(-step.ago)
		auths := []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: coreauth.QuotaState{
			ObservedAt: obs,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        strconv.Itoa(step.used),
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-After-Seconds": strconv.FormatInt(step.resetAfter, 10),
			},
		}}}
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	value, err := s.QuotaValue(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d: %+v", len(value.Windows), value.Windows)
	}
	w := value.Windows[0]
	if w.Window != "primary" || w.ResetAt != primaryReset || w.UsedPercent != 25 {
		t.Fatalf("window = %+v", w)
	}
	// Events at -80m and -40m fall inside [start, 25% first seen]; -5m is after.
	if w.Usage.Requests != 2 {
		t.Fatalf("requests = %d, want 2", w.Usage.Requests)
	}
}

func TestQuotaValueSamplerStaleAfterEarlyReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	s.Enqueue(Event{Timestamp: now.Add(-48 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Enqueue(Event{Timestamp: now.Add(-10 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Sequence: (R,40), (R',0), (R',25), (R,41), (R',30), (R',35)
	oldReset := now.Add(4 * 24 * time.Hour).Unix()
	earlyReset := now.Add(-36 * time.Hour)
	newReset := earlyReset.Add(7 * 24 * time.Hour).Unix()
	last := map[string]quotaLast{}
	for _, step := range []struct {
		ago   time.Duration
		reset int64
		used  int
	}{
		{48 * time.Hour, oldReset, 40},
		{36 * time.Hour, newReset, 0},
		{30 * time.Hour, newReset, 25},
		{25 * time.Hour, oldReset, 41}, // late stale snapshot of old window
		{20 * time.Hour, newReset, 30}, // new window progress: must NOT be rewritten to oldReset
		{5 * time.Hour, newReset, 35},
	} {
		auths := []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: coreauth.QuotaState{
			ObservedAt: now.Add(-step.ago),
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   strconv.Itoa(step.used),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(step.reset, 10),
			},
		}}}
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	// All 4 new-window samples must be stored with newReset, none rewritten to oldReset.
	var storedNew int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE reset_at = ?`, newReset).Scan(&storedNew); err != nil || storedNew != 4 {
		t.Fatalf("samples stored with new reset = %d (%v), want 4", storedNew, err)
	}

	value, err := s.QuotaValue(context.Background(), now.Add(-7*24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 {
		t.Fatalf("windows = %+v", value.Windows)
	}
	newer, older := value.Windows[0], value.Windows[1]
	if older.ResetAt != earlyReset.Unix() || older.UsedPercent != 41 {
		t.Fatalf("older = %+v, want ResetAt=%d UsedPercent=41", older, earlyReset.Unix())
	}
	if newer.ResetAt != newReset || newer.UsedPercent != 35 {
		t.Fatalf("newer = %+v, want ResetAt=%d UsedPercent=35", newer, newReset)
	}
}

func TestQuotaValueEarlyResetSecondHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	// Event 1: in Window 1 (4 days ago)
	s.Enqueue(Event{Timestamp: now.Add(-4 * 24 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// Event 2: after early reset (24h ago), before old nominal reset (which was now + 12h)!
	s.Enqueue(Event{Timestamp: now.Add(-24 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// Event 3: also after early reset (3h ago, before peak observed at 2h ago and old nominal reset)
	s.Enqueue(Event{Timestamp: now.Add(-3 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Window 1 started 6.5 days ago, nominal reset is now + 12h.
	// Reset happens 5 days in (now - 36h), so it is in the SECOND half (elapsed 5d > period/2 = 3.5d).
	// New reset is now - 36h + 7d = now + 5.5d.
	// Shift is 5 days > period/2, so grouping already places them into separate instances.
	// ResetAt trimming must trim Window 1 to now - 36h so Window 2 counts Event 2!
	oldReset := now.Add(12 * time.Hour).Unix()
	earlyReset := now.Add(-36 * time.Hour)
	newReset := earlyReset.Add(7 * 24 * time.Hour).Unix()
	last := map[string]quotaLast{}
	for _, step := range []struct {
		ago   time.Duration
		reset int64
		used  int
	}{
		{5 * 24 * time.Hour, oldReset, 20},
		{40 * time.Hour, oldReset, 50},
		{30 * time.Hour, newReset, 5},
		{2 * time.Hour, newReset, 20},
	} {
		auths := []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: coreauth.QuotaState{
			ObservedAt: now.Add(-step.ago),
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   strconv.Itoa(step.used),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(step.reset, 10),
			},
		}}}
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	value, err := s.QuotaValue(context.Background(), now.Add(-7*24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 {
		t.Fatalf("windows = %+v", value.Windows)
	}
	newer, older := value.Windows[0], value.Windows[1]
	if older.ResetAt != earlyReset.Unix() || older.Usage.Requests != 1 {
		t.Fatalf("older = %+v, want ResetAt=%d Requests=1", older, earlyReset.Unix())
	}
	// Newer window must count Event 2 (between earlyReset and oldReset) AND Event 3!
	if newer.ResetAt != newReset || newer.StartAt != earlyReset.Unix() || newer.Usage.Requests != 2 {
		t.Fatalf("newer = %+v, want ResetAt=%d StartAt=%d Requests=2", newer, newReset, earlyReset.Unix())
	}
}

func TestQuotaValueEarlyResetExhaustedWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	// Event 1: while primary was 100% (now - 2h). Secondary should exclude it.
	s.Enqueue(Event{Timestamp: now.Add(-2 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// Event 2: after primary was early-reset (now - 30m), but BEFORE old nominal reset (now + 1h)!
	// Secondary must count this event because primary is no longer exhausted.
	s.Enqueue(Event{Timestamp: now.Add(-30 * time.Minute).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	primaryOldReset := now.Add(time.Hour).Unix()
	primaryEarlyReset := now.Add(-45 * time.Minute)
	primaryNewReset := primaryEarlyReset.Add(5 * time.Hour).Unix()
	secondaryReset := now.Add(5 * 24 * time.Hour).Unix()

	last := map[string]quotaLast{}
	for _, step := range []struct {
		ago                time.Duration
		pReset             int64
		primary, secondary int
	}{
		// Primary hits 100% (exhausted) 3h ago
		{3 * time.Hour, primaryOldReset, 100, 10},
		// Primary is early-reset 45m ago to 0%, now at 5% 40m ago
		{40 * time.Minute, primaryNewReset, 5, 12},
		// Fresh sample of new primary window 20m ago
		{20 * time.Minute, primaryNewReset, 10, 15},
	} {
		auths := []*coreauth.Auth{{ID: "a1", Index: "a1", Provider: "codex", Quota: coreauth.QuotaState{
			ObservedAt: now.Add(-step.ago),
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":     strconv.Itoa(step.primary),
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         strconv.FormatInt(step.pReset, 10),
				"X-Codex-Secondary-Used-Percent":   strconv.Itoa(step.secondary),
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       strconv.FormatInt(secondaryReset, 10),
			},
		}}}
		SetAuthLister(func() []*coreauth.Auth { return auths })
		s.sampleQuota(last)
	}
	SetAuthLister(nil)

	value, err := s.QuotaValue(context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	var secondaryWin *QuotaWindowValue
	for i := range value.Windows {
		if value.Windows[i].Window == "secondary" {
			secondaryWin = &value.Windows[i]
			break
		}
	}
	if secondaryWin == nil {
		t.Fatalf("secondary window not found in %+v", value.Windows)
	}
	// Secondary must exclude Event 1 (during exhausted span) and count Event 2 (after early reset)
	if secondaryWin.Usage.Requests != 1 {
		t.Fatalf("secondary requests = %d, want 1 (event after early reset)", secondaryWin.Usage.Requests)
	}
}

func TestSplitLegacyEarlyResets(t *testing.T) {
	const week, r = 604800, 2_000_000
	start := int64(r - week)
	at := func(days float64) int64 { return (start + int64(days*86400)) * 1000 }
	sample := func(used, days float64) quotaSample {
		return quotaSample{authIndex: "a", window: "7d", period: week, resetAt: r, used: used, observed: at(days)}
	}
	group := func(samples []quotaSample) []QuotaWindowValue {
		splitLegacyEarlyResets(samples)
		return groupQuotaSamples(samples)
	}

	// A legacy early reset two days in: the new samples kept the old reset.
	got := group([]quotaSample{sample(30, 1), sample(40, 2), sample(0, 2.5), sample(10, 3)})
	if len(got) != 2 || got[0].ResetAt != r || got[0].UsedPercent != 40 ||
		got[1].StartAt != start+2*86400 || got[1].ResetAt != start+2*86400+week || got[1].UsedPercent != 10 {
		t.Fatalf("single reset: %+v", got)
	}

	// Several resets under one stored reset (repeated promotional resets).
	got = group([]quotaSample{sample(40, 1), sample(0, 2), sample(20, 3), sample(2, 4), sample(5, 5)})
	if len(got) != 3 || got[0].UsedPercent != 40 || got[1].UsedPercent != 20 || got[2].UsedPercent != 5 ||
		got[1].StartAt != start+86400 || got[2].StartAt != start+3*86400 {
		t.Fatalf("repeated resets: %+v", got)
	}

	// A stale snapshot under an absolute reset, and an unconfirmed trailing drop, stay merged.
	for name, samples := range map[string][]quotaSample{
		"stale snapshot": {sample(40, 1), sample(15, 2), sample(42, 3)},
		"trailing drop":  {sample(40, 1), sample(0, 2)},
	} {
		if got := group(samples); len(got) != 1 || got[0].ResetAt != r {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

func TestQuotaValueLegacyEarlyReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	// Each event costs $2.
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, ago := range []time.Duration{50 * time.Hour, 30 * time.Hour, 20 * time.Hour, 2 * time.Hour} {
		s.Enqueue(Event{Timestamp: now.Add(-ago).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Rows as the old sampler stored them: the weekly window started 3 days ago and was reset early
	// about a day ago, but the new window's samples kept the old reset.
	oldReset := now.Add(4 * 24 * time.Hour).Unix()
	for _, row := range []struct {
		ago  time.Duration
		used float64
	}{{48 * time.Hour, 20}, {26 * time.Hour, 40}, {23 * time.Hour, 2}, {time.Hour, 10}} {
		if _, err = s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
			VALUES (?,?,?,?,?,?,?)`, "a1", "codex", "secondary", 604800, oldReset, row.used, now.Add(-row.ago).UnixMilli()); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	value, err := s.QuotaValue(context.Background(), now.Add(-7*24*time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 {
		t.Fatalf("windows = %+v", value.Windows)
	}
	newer, older := value.Windows[0], value.Windows[1]
	// The reset is estimated at the last old-window sample, so the old window ends there and the
	// new one counts the later events.
	estimated := now.Add(-26 * time.Hour).Unix()
	if older.ResetAt != estimated || older.UsedPercent != 40 || older.Usage.Requests != 2 {
		t.Fatalf("older = %+v", older)
	}
	if newer.StartAt != estimated || newer.ResetAt != estimated+604800 || newer.UsedPercent != 10 || newer.Usage.Requests != 2 {
		t.Fatalf("newer = %+v", newer)
	}
	approx(t, "older cost", older.Usage.Cost, 4)
	approx(t, "newer cost", newer.Usage.Cost, 4)

	// Reading does not rewrite the stored rows.
	var rewritten int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE reset_at != ?`, oldReset).Scan(&rewritten); err != nil || rewritten != 0 {
		t.Fatalf("rows with another reset = %d (%v), want 0", rewritten, err)
	}
}

func TestGroupQuotaSamplesOutOfOrderRollover(t *testing.T) {
	// (a) A,B,A,B grouping yields 2 instances with correct maxima
	got := groupQuotaSamples([]quotaSample{
		{authIndex: "a", window: "5h", period: 18000, resetAt: 100_000, used: 70, observed: 1},
		{authIndex: "a", window: "5h", period: 18000, resetAt: 118_000, used: 10, observed: 2},
		{authIndex: "a", window: "5h", period: 18000, resetAt: 100_000, used: 80, observed: 3},
		{authIndex: "a", window: "5h", period: 18000, resetAt: 118_000, used: 20, observed: 4},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 instances, got %d: %+v", len(got), got)
	}
	if got[0].ResetAt != 100_000 || got[0].UsedPercent != 80 || got[0].ObservedAt != 3 || got[0].Samples != 2 {
		t.Fatalf("instance 0 = %+v", got[0])
	}
	if got[1].ResetAt != 118_000 || got[1].UsedPercent != 20 || got[1].ObservedAt != 4 || got[1].Samples != 2 {
		t.Fatalf("instance 1 = %+v", got[1])
	}
}

func TestQuotaValueWindowBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	T0 := int64(1_800_000_000)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	// e1: 60s before Window 1's StartAt (StartAt is T0 - 18000)
	s.Enqueue(Event{Timestamp: (T0 - 18000 - 60) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// e2: 60s before Window 2's StartAt (StartAt is T0), which is also right before Window 1's ResetAt
	s.Enqueue(Event{Timestamp: (T0 - 60) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// e3: 10s after Window 1's ResetAt (during Window 2)
	s.Enqueue(Event{Timestamp: (T0 + 10) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// e4: 100s after Window 2's StartAt
	s.Enqueue(Event{Timestamp: (T0 + 100) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Seed samples directly: Window 1 has late sample at T0+60s; Window 2 has sample at T0+200s.
	for _, sm := range []struct {
		resetAt  int64
		used     float64
		observed int64
	}{
		{T0, 50, (T0 + 60) * 1000},
		{T0 + 18000, 25, (T0 + 200) * 1000},
	} {
		if _, errExec := s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, sm.resetAt, sm.used, sm.observed); errExec != nil {
			t.Fatalf("insert sample: %v", errExec)
		}
	}

	val, err := s.QuotaValue(context.Background(), time.Unix(T0-20000, 0), time.Unix(T0+20000, 0), []string{"a1"})
	if err != nil {
		t.Fatalf("QuotaValue: %v", err)
	}
	if len(val.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d: %+v", len(val.Windows), val.Windows)
	}

	// Sorted newest reset first: val.Windows[0] is Window 2 (ResetAt = T0+18000), val.Windows[1] is Window 1 (ResetAt = T0).
	w2, w1 := val.Windows[0], val.Windows[1]

	// (b) Event 1 (60s before Window 1's StartAt) is included in Window 1 because it has no preceding instance.
	// (d) Event 3 (10s after Window 1's ResetAt) is NOT included in Window 1 even though Window 1's ObservedAt is T0+60s.
	// Therefore Window 1 has exactly 2 requests: e1 and e2.
	if w1.Usage.Requests != 2 {
		t.Fatalf("(b)/(d) window 1 requests = %d, want 2 (e1 and e2)", w1.Usage.Requests)
	}

	// (c) Event 2 (just before Window 2's StartAt) is NOT counted in Window 2 because Window 1's ResetAt equals Window 2's StartAt.
	// Therefore Window 2 has exactly 2 requests: e3 and e4.
	if w2.Usage.Requests != 2 {
		t.Fatalf("(c) window 2 requests = %d, want 2 (e3 and e4)", w2.Usage.Requests)
	}
}

func TestQuotaValueJitterAndStartPad(t *testing.T) {
	// (a) relative-reset samples with jitter up to 20 min group into one instance,
	// StartAt from the minimum reset, and a request 10 minutes before that StartAt is counted.
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000) // minimum reset
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	startAt := T0 - 18000
	// Request 10 minutes (600s) before startAt:
	s.Enqueue(Event{Timestamp: (startAt - 600) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// Request inside the window:
	s.Enqueue(Event{Timestamp: (startAt + 1000) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Sample with 20 minutes (1200s) jitter:
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0+1200, 30.0, (T0-5000)*1000)
	// Sample with minimum reset:
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0, 50.0, (T0-2000)*1000)

	val, err := s.QuotaValue(context.Background(), time.Unix(T0-30000, 0), time.Unix(T0+10000, 0), []string{"a1"})
	if err != nil {
		t.Fatalf("QuotaValue: %v", err)
	}
	if len(val.Windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(val.Windows))
	}
	w := val.Windows[0]
	if w.ResetAt != T0 {
		t.Fatalf("ResetAt = %d, want minimum %d", w.ResetAt, T0)
	}
	if w.StartAt != startAt {
		t.Fatalf("StartAt = %d, want %d", w.StartAt, startAt)
	}
	if w.Usage.Requests != 2 {
		t.Fatalf("requests = %d, want 2 (including request 10m before StartAt)", w.Usage.Requests)
	}
}

func TestQuotaValuePredecessorClampRangeIndependence(t *testing.T) {
	// (b) predecessor clamp still applies when the query range excludes the predecessor's samples
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	// Event right before T0 (in Window 1's period):
	s.Enqueue(Event{Timestamp: (T0 - 60) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	// Event in Window 2:
	s.Enqueue(Event{Timestamp: (T0 + 100) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Predecessor sample (Window 1) observed at T0 - 5000:
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0, 80.0, (T0-5000)*1000)
	// Window 2 sample observed at T0 + 500:
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0+18000, 20.0, (T0+500)*1000)

	// Broad range (includes Window 1's sample):
	valBroad, err := s.QuotaValue(context.Background(), time.Unix(T0-10000, 0), time.Unix(T0+20000, 0), []string{"a1"})
	if err != nil || len(valBroad.Windows) != 2 {
		t.Fatalf("broad range: %v, len=%d", err, len(valBroad.Windows))
	}
	// valBroad.Windows[0] is Window 2 (newest first).
	w2Broad := valBroad.Windows[0]

	// Narrow range (starts at T0, excludes Window 1's sample at T0 - 5000):
	valNarrow, err := s.QuotaValue(context.Background(), time.Unix(T0, 0), time.Unix(T0+20000, 0), []string{"a1"})
	if err != nil || len(valNarrow.Windows) != 1 {
		t.Fatalf("narrow range: %v, len=%d", err, len(valNarrow.Windows))
	}
	w2Narrow := valNarrow.Windows[0]

	if w2Narrow.Usage.Requests != 1 || w2Narrow.Usage.Requests != w2Broad.Usage.Requests {
		t.Fatalf("narrow requests = %d, broad requests = %d; want 1 for both", w2Narrow.Usage.Requests, w2Broad.Usage.Requests)
	}
}

func TestQuotaValueRolloverInstantExclusive(t *testing.T) {
	// (c) an event exactly at the rollover instant is counted in exactly one window
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	// Event exactly at T0 * 1000:
	s.Enqueue(Event{Timestamp: T0 * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	// Window 1: ResetAt = T0, observed at T0 + 60s (late sample):
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0, 100.0, (T0+60)*1000)
	// Window 2: ResetAt = T0 + 18000, observed at T0 + 200s:
	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0+18000, 10.0, (T0+200)*1000)

	val, err := s.QuotaValue(context.Background(), time.Unix(T0-20000, 0), time.Unix(T0+20000, 0), []string{"a1"})
	if err != nil || len(val.Windows) != 2 {
		t.Fatalf("QuotaValue: %v, len=%d", err, len(val.Windows))
	}
	w2, w1 := val.Windows[0], val.Windows[1]
	if w1.Usage.Requests != 0 {
		t.Fatalf("Window 1 requests = %d, want 0 (exclusive upper bound at reset)", w1.Usage.Requests)
	}
	if w2.Usage.Requests != 1 {
		t.Fatalf("Window 2 requests = %d, want 1 (inclusive lower bound at pred reset)", w2.Usage.Requests)
	}
}

func TestGroupQuotaSamplesInterleavedCredentials(t *testing.T) {
	// (d) grouping with 2 credentials interleaved
	samples := []quotaSample{
		{authIndex: "c1", window: "5h", period: 18000, resetAt: 100_000, used: 20, observed: 10},
		{authIndex: "c2", window: "5h", period: 18000, resetAt: 100_000, used: 30, observed: 11},
		{authIndex: "c1", window: "5h", period: 18000, resetAt: 100_200, used: 40, observed: 20},
		{authIndex: "c2", window: "5h", period: 18000, resetAt: 100_100, used: 50, observed: 21},
		{authIndex: "c1", window: "7d", period: 604800, resetAt: 700_000, used: 10, observed: 25},
		{authIndex: "c2", window: "7d", period: 604800, resetAt: 700_000, used: 15, observed: 26},
	}
	got := groupQuotaSamples(samples)
	if len(got) != 4 {
		t.Fatalf("expected 4 window instances, got %d: %+v", len(got), got)
	}
	find := func(auth, win string) *QuotaWindowValue {
		for i := range got {
			if got[i].AuthIndex == auth && got[i].Window == win {
				return &got[i]
			}
		}
		return nil
	}
	c1_5h := find("c1", "5h")
	c2_5h := find("c2", "5h")
	if c1_5h == nil || c1_5h.UsedPercent != 40 || c1_5h.ResetAt != 100_000 || c1_5h.Samples != 2 {
		t.Fatalf("c1 5h = %+v", c1_5h)
	}
	if c2_5h == nil || c2_5h.UsedPercent != 50 || c2_5h.ResetAt != 100_000 || c2_5h.Samples != 2 {
		t.Fatalf("c2 5h = %+v", c2_5h)
	}
}

func TestQuotaValueExternalUsageDetection(t *testing.T) {
	// (e) external detection: samples 10,20,30,60(+30 with almost no tokens),70
	// with steady tokens per point except the jump interval -> external_percent == 30, external_checked == true.
	// full_cost and full_tokens use own_percent (70 used, 30 external -> divide by 40).
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000)
	tokSteady := Tokens{InputUncached: 100_000, Output: 0, Total: 100_000} // 10k tokens / %
	tokJump := Tokens{InputUncached: 100, Output: 0, Total: 100}           // tiny tokens during jump

	// Sample timestamps (5 intervals):
	// t0 = T0 + 1000, used = 10 (interval 0: 100k tokens, delta = 10)
	// t1 = T0 + 2000, used = 20 (interval 1: 100k tokens, delta = 10)
	// t2 = T0 + 3000, used = 30 (interval 2: 100k tokens, delta = 10)
	// t3 = T0 + 4000, used = 60 (interval 3: 100 tokens, delta = 30) -> outside jump!
	// t4 = T0 + 5000, used = 70 (interval 4: 100k tokens, delta = 10)
	s.Enqueue(Event{Timestamp: (T0 + 500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tokSteady})
	s.Enqueue(Event{Timestamp: (T0 + 1500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tokSteady})
	s.Enqueue(Event{Timestamp: (T0 + 2500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tokSteady})
	s.Enqueue(Event{Timestamp: (T0 + 3500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tokJump})
	s.Enqueue(Event{Timestamp: (T0 + 4500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "a1", Tokens: tokSteady})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	sampleTimes := []int64{T0 + 1000, T0 + 2000, T0 + 3000, T0 + 4000, T0 + 5000}
	sampleUsed := []float64{10, 20, 30, 60, 70}
	for i := range sampleTimes {
		s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "a1", "codex", "primary", 18000, T0+18000, sampleUsed[i], sampleTimes[i]*1000)
	}

	val, err := s.QuotaValue(context.Background(), time.Unix(T0, 0), time.Unix(T0+20000, 0), []string{"a1"})
	if err != nil || len(val.Windows) != 1 {
		t.Fatalf("QuotaValue: %v, len=%d", err, len(val.Windows))
	}
	w := val.Windows[0]
	if !w.ExternalChecked {
		t.Fatalf("external_checked = false, want true")
	}
	if math.Abs(w.ExternalPercent-30) > 0.05 {
		t.Fatalf("external_percent = %v, want ~30", w.ExternalPercent)
	}
	if math.Abs(w.OwnPercent-40) > 0.05 {
		t.Fatalf("own_percent = %v, want ~40", w.OwnPercent)
	}
	// Total tokens = 400,100. Extrapolated using own_percent: full_tokens = 400,100 * 100 / own_percent
	wantTokens := 400_100.0 * 100.0 / w.OwnPercent
	if w.FullTokens == nil || math.Abs(*w.FullTokens-wantTokens) > 1e-3 {
		t.Fatalf("full_tokens = %v, want %v", w.FullTokens, wantTokens)
	}

	// Also test: a window with only 2 increasing samples -> external_checked == false
	s2, err := Open(filepath.Join(t.TempDir(), "usage2.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s2.Close()
	s2.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a2", "codex", "primary", 18000, T0+18000, 10.0, (T0+1000)*1000)
	s2.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, "a2", "codex", "primary", 18000, T0+18000, 20.0, (T0+2000)*1000)

	val2, err := s2.QuotaValue(context.Background(), time.Unix(T0, 0), time.Unix(T0+20000, 0), []string{"a2"})
	if err != nil || len(val2.Windows) != 1 {
		t.Fatalf("QuotaValue: %v", err)
	}
	if val2.Windows[0].ExternalChecked {
		t.Fatalf("window with 2 samples: external_checked = true, want false")
	}
	if val2.Windows[0].ExternalPercent != 0 {
		t.Fatalf("external_percent = %v, want 0", val2.Windows[0].ExternalPercent)
	}
	if val2.Windows[0].OwnPercent != 20 {
		t.Fatalf("own_percent = %v, want 20", val2.Windows[0].OwnPercent)
	}
}

func TestSampleQuotaStagingFailureRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now()
	auths := []*coreauth.Auth{{
		ID:       "fail-test",
		Index:    "fail-test",
		Provider: "codex",
		Quota: coreauth.QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "25",
				"X-Codex-Primary-Window-Minutes": "300",
				"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Unix()+18000, 10),
			},
		},
	}}
	SetAuthLister(func() []*coreauth.Auth { return auths })
	defer SetAuthLister(nil)

	// Create a trigger that aborts insertion to simulate a DB write failure
	if _, err := s.db.Exec(`CREATE TRIGGER abort_quota_samples BEFORE INSERT ON quota_samples BEGIN SELECT RAISE(ABORT, "forced error"); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	last := make(map[string]quotaLast)
	s.sampleQuota(last)

	// The map must NOT have been updated with the failed sample
	if len(last) != 0 {
		t.Fatalf("last map updated despite DB failure: %+v", last)
	}

	// Drop trigger and retry - it must now succeed and update last
	if _, err := s.db.Exec(`DROP TRIGGER abort_quota_samples;`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	s.sampleQuota(last)
	if len(last) != 1 {
		t.Fatalf("last map not updated after retry: %+v", last)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE auth_index = "fail-test"`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("sample not recorded on retry: count=%d, err=%v", count, err)
	}
}

func TestParseQuotaWindowsOverflowInf(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// Claude header with 1e308 utilization or huge reset
	claudeInf := parseQuotaWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "1e308",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1800010000",
	}, now)
	if len(claudeInf) != 0 {
		t.Fatalf("1e308 utilization must be rejected, got: %+v", claudeInf)
	}

	claudeResetInf := parseQuotaWindows("claude", map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1e308",
	}, now)
	if len(claudeResetInf) != 0 {
		t.Fatalf("1e308 reset must be rejected, got: %+v", claudeResetInf)
	}

	codexUsedInf := parseQuotaWindows("codex", map[string]string{
		"X-Codex-Primary-Used-Percent":   "1001",
		"X-Codex-Primary-Window-Minutes": "300",
		"X-Codex-Primary-Reset-At":       "1800010000",
	}, now)
	if len(codexUsedInf) != 0 {
		t.Fatalf(">1000 used_percent must be rejected, got: %+v", codexUsedInf)
	}
}
