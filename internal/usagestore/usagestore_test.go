package usagestore

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const fixtureModelsDev = `{
  "reseller": {"models": {"gpt-x": {"cost": {"input": 9, "output": 9}}}},
  "openai": {"models": {"gpt-x": {"cost": {"input": 1, "output": 10, "cache_read": 0.1},
    "experimental": {"modes": {"fast": {"cost": {"input": 2, "output": 20, "cache_read": 0.2},
      "provider": {"body": {"service_tier": "priority"}}}}}}}},
  "anthropic": {"models": {"claude-y-20250101": {"cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}}}},
  "free": {"models": {"no-cost": {}}}
}`

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func testPricer(t *testing.T) *Pricer {
	t.Helper()
	prices, err := parseModelsDev([]byte(fixtureModelsDev))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := NewPricer("")
	p.modelsDev = prices
	return p
}

func TestPricerLookup(t *testing.T) {
	p := testPricer(t)

	price, source, ok := p.Lookup("gpt-x", "")
	if !ok || source != "models.dev" || price.Input != 1 {
		t.Fatalf("vendor listing must win over reseller: %+v %q %v", price, source, ok)
	}
	if price, _, _ = p.Lookup("GPT-X", "priority"); price.Input != 2 {
		t.Fatalf("priority tier price = %+v", price)
	}
	if price, _, _ = p.Lookup("openai/gpt-x(high)", "flex"); price.Input != 1 {
		t.Fatalf("prefix/suffix lookup or unknown tier fallback failed: %+v", price)
	}
	if price, _, ok = p.Lookup("claude-y-20250101", ""); !ok || price.CacheWrite != 3.75 {
		t.Fatalf("claude price = %+v %v", price, ok)
	}
	if price, _, ok = p.Lookup("gpt-x", ""); price.CacheWrite != 1 {
		t.Fatalf("missing cache_write must default to input price: %+v", price)
	}
	if _, _, ok = p.Lookup("no-cost", ""); ok {
		t.Fatal("model without cost must be unpriced")
	}

	zero := 0.0
	p.Configure(config.UsagePricingConfig{Overrides: []config.UsagePriceOverride{{Model: "gpt-x", Input: 5, Output: 6, CacheRead: &zero}}})
	price, source, _ = p.Lookup("gpt-x", "priority")
	if source != "override" || price.Input != 5 || price.CacheRead != 0 || price.CacheWrite != 5 {
		t.Fatalf("override = %+v %q", price, source)
	}
	p.Configure(config.UsagePricingConfig{DisableModelsDev: true})
	if _, _, ok = p.Lookup("gpt-x", ""); ok {
		t.Fatal("disabled models.dev must not price")
	}
}

func TestPriceCost(t *testing.T) {
	p := Price{Input: 1, Output: 10, CacheRead: 0.1, CacheWrite: 2, Reasoning: 20}
	tok := Tokens{InputUncached: 1e6, CacheRead: 1e6, CacheWrite: 1e6, Output: 3e6, Reasoning: 1e6, Unclassified: 1e6}
	// input 1 + unclassified 1 + cache read 0.1 + cache write 2 + output 2M*10 + reasoning 1M*20
	approx(t, "cost", p.Cost(tok), 1+1+0.1+2+20+20)
	p.Reasoning = 0
	approx(t, "cost without reasoning price", p.Cost(tok), 1+1+0.1+2+30)
}

func TestEventFromRecord(t *testing.T) {
	rec := coreusage.Record{
		Provider:             "codex",
		ExecutorType:         "codex",
		Model:                "gpt-x",
		APIKey:               "sk-client-secret-1234",
		AuthIndex:            "a1",
		AuthType:             "apikey",
		Source:               "sk-upstream-secret-9876",
		EffectiveServiceTier: "Priority",
		RequestedAt:          time.UnixMilli(1_700_000_000_000),
		Latency:              1500 * time.Millisecond,
		Failed:               true,
		Fail:                 coreusage.Failure{StatusCode: 429, Body: "rate limited"},
		Detail:               coreusage.Detail{InputTokens: 100, CachedTokens: 40, OutputTokens: 30, ReasoningTokens: 10, TotalTokens: 130},
	}
	e := eventFromRecord(context.Background(), rec)
	if e.APIKey != "sk-c...1234" || len(e.APIKeyID) != 12 || e.APIKey == rec.APIKey {
		t.Fatalf("api key must be masked and hashed: %q %q", e.APIKey, e.APIKeyID)
	}
	if e.Source != "sk-u...9876" {
		t.Fatalf("upstream api key source must be masked: %q", e.Source)
	}
	if !e.Failed || e.StatusCode != 429 || e.Error != "rate limited" || e.ServiceTier != "priority" {
		t.Fatalf("failure/tier fields: %+v", e)
	}
	if e.Tokens.Total != 130 || e.Tokens.InputUncached+e.Tokens.CacheRead+e.Tokens.CacheWrite+e.Tokens.Output+e.Tokens.Unclassified != 130 {
		t.Fatalf("token buckets must partition the total: %+v", e.Tokens)
	}
	if e.Tokens.Output < e.Tokens.Reasoning {
		t.Fatalf("output must include reasoning: %+v", e.Tokens)
	}

	// Client API key used as fallback Source (e.g. plugin executors or headless OAuth) must be masked.
	fallbackRec := coreusage.Record{
		Model:    "gpt-x",
		APIKey:   "sk-client-secret-1234",
		AuthType: "",
		Source:   "sk-client-secret-1234",
	}
	eFallback := eventFromRecord(context.Background(), fallbackRec)
	if eFallback.Source != "sk-c...1234" {
		t.Fatalf("client key fallback source must be masked: %q", eFallback.Source)
	}

	// OAuth email/project source must remain unmasked.
	oauthRec := coreusage.Record{
		Model:    "gpt-x",
		APIKey:   "sk-client-secret-1234",
		AuthType: "oauth",
		Source:   "user@example.com",
	}
	eOAuth := eventFromRecord(context.Background(), oauthRec)
	if eOAuth.Source != "user@example.com" {
		t.Fatalf("oauth email source must remain unmasked: %q", eOAuth.Source)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	p := testPricer(t)
	s, err := Open(path, 30, p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-5 * 24 * time.Hour)
	tok := Tokens{InputUncached: 1e6, Output: 1e6, Total: 2e6}
	s.Enqueue(Event{Timestamp: day.Add(1 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", APIKeyID: "k1", APIKey: "sk-a", AuthIndex: "a1", Tokens: tok, LatencyMs: 100, TTFTMs: 50})
	s.Enqueue(Event{Timestamp: day.Add(1*time.Hour + time.Minute).UnixMilli(), Provider: "codex", Model: "gpt-x", APIKeyID: "k1", APIKey: "sk-a", AuthIndex: "a1", ServiceTier: "priority", Tokens: tok, LatencyMs: 300, Failed: true, StatusCode: 500})
	s.Enqueue(Event{Timestamp: day.Add(26 * time.Hour).UnixMilli(), Provider: "x", Model: "mystery", APIKeyID: "k2", AuthIndex: "a2", Tokens: tok})
	// Older than retention: kept in rollups, purged from events on open.
	s.Enqueue(Event{Timestamp: time.Now().Add(-40 * 24 * time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", Tokens: tok})
	s.Close()

	s, err = Open(path, 30, p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	st, err := s.Stats(ctx, day, day.Add(48*time.Hour), "", 0, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Bucket != "hour" || len(st.Series) != 48 {
		t.Fatalf("bucket=%s series=%d", st.Bucket, len(st.Series))
	}
	if st.Totals.Requests != 3 || st.Totals.Failed != 1 || st.Totals.UnpricedRequests != 1 {
		t.Fatalf("totals: %+v", st.Totals)
	}
	// default tier 1+10, priority tier 2+20, mystery unpriced.
	approx(t, "total cost", st.Totals.Cost, 11+22)
	approx(t, "avg latency", st.Totals.AvgLatencyMs, 400.0/3)
	approx(t, "avg ttft", st.Totals.AvgTTFTMs, 50)
	if st.Series[1].Requests != 2 || len(st.Models) != 2 || st.Models[0].Model != "gpt-x" || st.Models[0].PriceSource != "models.dev" {
		t.Fatalf("series/models: %+v %+v", st.Series[1], st.Models)
	}
	if len(st.APIKeys) != 2 || st.APIKeys[0].APIKey != "sk-a" || len(st.Credentials) != 2 {
		t.Fatalf("keys/creds: %+v %+v", st.APIKeys, st.Credentials)
	}

	// UTC+8 day buckets: 01:00Z is 09:00 local on day one, 02:00Z next day is 10:00 local on day two.
	st, err = s.Stats(ctx, day.Add(-8*time.Hour), day.Add(40*time.Hour), "day", 8*60, StatsFilter{})
	if err != nil {
		t.Fatalf("stats day: %v", err)
	}
	if len(st.Series) != 2 || st.Series[0].Requests != 2 || st.Series[1].Requests != 1 || st.Series[0].Time != day.Add(-8*time.Hour).Unix() {
		t.Fatalf("day series: %+v", st.Series)
	}

	events, err := s.Events(ctx, EventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected the old event purged, got %d events", len(events))
	}
	if events[0].Model != "mystery" || events[0].Priced || !events[1].Failed || !events[1].Priced {
		t.Fatalf("events order/pricing: %+v", events)
	}
	approx(t, "event cost", events[1].Cost, 22)
	failed := true
	page, err := s.Events(ctx, EventFilter{Failed: &failed, Model: []string{"gpt-x"}})
	if err != nil || len(page) != 1 || page[0].StatusCode != 500 {
		t.Fatalf("failed filter: %v %+v", err, page)
	}
	page, err = s.Events(ctx, EventFilter{BeforeID: events[1].ID})
	if err != nil || len(page) != 1 || page[0].ID != events[2].ID {
		t.Fatalf("pagination: %v %+v", err, page)
	}

	mp, err := s.ModelPrices(ctx)
	if err != nil || len(mp) != 2 || mp[0].Model != "gpt-x" || mp[0].Price == nil || mp[1].Price != nil {
		t.Fatalf("model prices: %v %+v", err, mp)
	}
}

func TestApplyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	cfg := &config.Config{}
	cfg.UsageStore = config.UsageStoreConfig{Enable: true, Path: path, Pricing: config.UsagePricingConfig{DisableModelsDev: true}}
	Apply(cfg)
	s := Current()
	if s == nil {
		t.Fatal("store not opened")
	}
	Apply(cfg)
	if Current() != s {
		t.Fatal("re-apply with the same path must keep the store")
	}
	plugin{}.HandleUsage(context.Background(), coreusage.Record{Model: "m", Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1}})
	cfg.UsageStore.Enable = false
	Apply(cfg)
	if Current() != nil {
		t.Fatal("disable must close the store")
	}
	plugin{}.HandleUsage(context.Background(), coreusage.Record{Model: "m"})

	cfg.UsageStore.Enable = true
	Apply(cfg)
	defer Shutdown()
	events, err := Current().Events(context.Background(), EventFilter{})
	if err != nil || len(events) != 1 || events[0].Model != "m" {
		t.Fatalf("event written before close must persist: %v %+v", err, events)
	}
}

func TestAnalysis(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A recent Sunday 23:30 UTC is Monday 07:30 in UTC+8.
	base := time.Now().UTC().Truncate(24 * time.Hour)
	for base.Weekday() != time.Sunday {
		base = base.Add(-24 * time.Hour)
	}
	ts := base.Add(23*time.Hour + 30*time.Minute)
	for i := 1; i <= 100; i++ {
		s.Enqueue(Event{Timestamp: ts.UnixMilli(), Provider: "codex", Model: "gpt-x", LatencyMs: int64(i * 100),
			TTFTMs: int64(i), Tokens: Tokens{InputUncached: 10, Total: 10}})
	}
	s.Enqueue(Event{Timestamp: ts.UnixMilli(), Provider: "codex", Model: "gpt-x", Failed: true, LatencyMs: 999_999})
	s.Close()
	s, err = Open(filepath.Join(filepath.Dir(s.path), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	a, err := s.Analysis(context.Background(), base, base.Add(48*time.Hour), 8*60, StatsFilter{})
	if err != nil {
		t.Fatalf("analysis: %v", err)
	}
	if a.Sampled || a.Latency.Count != 100 || a.Latency.P50 != 5100 || a.Latency.P95 != 9500 || a.Latency.Max != 10_000 {
		t.Fatalf("failed requests must be excluded and percentiles exact: %+v", a.Latency)
	}
	if a.TTFT.P99 != 99 {
		t.Fatalf("ttft: %+v", a.TTFT)
	}
	// Buckets: <=500 (5), <=1000 (5), <=2000 (10), <=5000 (30), <=10000 (50).
	want := []int64{5, 5, 10, 30, 50, 0, 0, 0}
	for i, n := range want {
		if a.Histogram[i] != n {
			t.Fatalf("histogram = %v, want %v", a.Histogram, want)
		}
	}
	if len(a.Models) != 1 || a.Models[0].Latency.Count != 100 {
		t.Fatalf("models: %+v", a.Models)
	}
	if len(a.Heatmap) != 1 || a.Heatmap[0].Weekday != 1 || a.Heatmap[0].Hour != 7 || a.Heatmap[0].Requests != 101 || a.Heatmap[0].Failed != 1 {
		t.Fatalf("heatmap must use the local weekday and hour: %+v", a.Heatmap)
	}
	approx(t, "heat cost", a.Heatmap[0].Cost, 100*10*1.0/1e6)

	aFiltered, err := s.Analysis(context.Background(), base, base.Add(48*time.Hour), 8*60, StatsFilter{Model: []string{"gpt-x"}})
	if err != nil || aFiltered.Latency.Count != 100 {
		t.Fatalf("filtered analysis: count=%d, err=%v", aFiltered.Latency.Count, err)
	}
	aNone, err := s.Analysis(context.Background(), base, base.Add(48*time.Hour), 8*60, StatsFilter{Model: []string{"nope"}})
	if err != nil || aNone.Latency.Count != 0 || len(aNone.Heatmap) != 0 {
		t.Fatalf("filtered none analysis: count=%d, heatmap=%d, err=%v", aNone.Latency.Count, len(aNone.Heatmap), err)
	}

	var exported int
	if err = s.ExportEvents(context.Background(), EventFilter{Model: []string{"gpt-x"}}, func(e *Event) error {
		exported++
		if !e.Priced {
			t.Fatalf("exported events must be priced: %+v", e)
		}
		return nil
	}); err != nil || exported != 101 {
		t.Fatalf("export: %v %d", err, exported)
	}
}

func TestStatsFilterAndCredentialLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	tok := Tokens{InputUncached: 10, Total: 10}
	s.Enqueue(Event{Timestamp: now.Add(-time.Hour).UnixMilli(), Provider: "openai-compatible-acme", Model: "gpt-x", AuthIndex: "a1", AuthType: "apikey", Source: "sk-o...old", APIKeyID: "k1", Tokens: tok})
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "openai-compatible-acme", Model: "gpt-x", AuthIndex: "a1", AuthType: "apikey", Source: "sk-n...new", APIKeyID: "k2", Tokens: tok})
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "codex", Model: "gpt-y", AuthIndex: "a2", AuthType: "oauth", Source: "user@example.com", APIKeyID: "k1", Tokens: tok})
	s.Close()
	// Drop the label table to prove reopening backfills it from events.
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err = s.db.Exec(`DELETE FROM credentials`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	st, err := s.Stats(ctx, now.Add(-2*time.Hour), now.Add(time.Minute), "", 0, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	labels := map[string]CredentialStat{}
	for _, c := range st.Credentials {
		labels[c.AuthIndex] = c
	}
	if labels["a1"].Source != "sk-n...new" || labels["a1"].AuthType != "apikey" || labels["a2"].Source != "user@example.com" {
		t.Fatalf("credential labels must come from the latest event: %+v", st.Credentials)
	}

	for _, tc := range []struct {
		filter StatsFilter
		want   int64
	}{
		{StatsFilter{Model: []string{"gpt-x"}}, 2},
		{StatsFilter{Provider: []string{"codex"}}, 1},
		{StatsFilter{APIKeyID: []string{"k1"}}, 2},
		{StatsFilter{AuthIndex: []string{"a1"}, APIKeyID: []string{"k2"}}, 1},
		{StatsFilter{Model: []string{"nope"}}, 0},
		{StatsFilter{Model: []string{"gpt-x", "gpt-y"}}, 3},
		{StatsFilter{Model: []string{"gpt-x", "gpt-y"}, Provider: []string{"codex"}}, 1},
		{StatsFilter{Model: []string{"gpt-x"}, Provider: []string{"codex"}}, 0},
	} {
		got, errStats := s.Stats(ctx, now.Add(-2*time.Hour), now.Add(time.Minute), "", 0, tc.filter)
		if errStats != nil || got.Totals.Requests != tc.want {
			t.Fatalf("filter %+v: %v requests=%d want %d", tc.filter, errStats, got.Totals.Requests, tc.want)
		}
	}
}

func TestEventFromRecord_Substituted(t *testing.T) {
	ctx := context.Background()

	// 1. served differs from model and upstreamModel -> true
	rec1 := coreusage.Record{Model: "grok-4.7", ResponseModel: "grok-4.7-build"}
	e1 := eventFromRecord(ctx, rec1)
	if !e1.Substituted || e1.ResponseModel != "grok-4.7-build" {
		t.Fatalf("rec1: expected substituted true, got %v (%s)", e1.Substituted, e1.ResponseModel)
	}

	// 2. served equals UpstreamModel but differs from Model -> false (claude/kimi mapping)
	rec2 := coreusage.Record{
		Model:         "claude-sonnet-4-5",
		UpstreamModel: "claude-3-5-sonnet-20241022",
		ResponseModel: "claude-3-5-sonnet-20241022",
	}
	e2 := eventFromRecord(ctx, rec2)
	if e2.Substituted {
		t.Fatalf("rec2: mapped upstream model must not be flagged substituted: %+v", e2)
	}

	// 3. dated snapshot: requested "claude-sonnet-4-5" served "claude-sonnet-4-5-20250929" -> false
	rec3 := coreusage.Record{
		Model:         "claude-sonnet-4-5",
		ResponseModel: "claude-sonnet-4-5-20250929",
	}
	e3 := eventFromRecord(ctx, rec3)
	if e3.Substituted {
		t.Fatalf("rec3: dated snapshot alias must not be flagged substituted: %+v", e3)
	}

	// 4. empty served -> false
	rec4 := coreusage.Record{
		Model:         "gpt-5",
		ResponseModel: "",
	}
	e4 := eventFromRecord(ctx, rec4)
	if e4.Substituted || e4.ResponseModel != "" {
		t.Fatalf("rec4: empty response model must not be substituted: %+v", e4)
	}
}

func TestMigrationAndSubstitutedStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old_usage.db")
	oldSchema := `
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY,
	ts INTEGER NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	executor_type TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	alias TEXT NOT NULL DEFAULT '',
	endpoint TEXT NOT NULL DEFAULT '',
	auth_type TEXT NOT NULL DEFAULT '',
	auth_index TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	api_key_id TEXT NOT NULL DEFAULT '',
	api_key TEXT NOT NULL DEFAULT '',
	access_provider TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	input_uncached INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	output INTEGER NOT NULL DEFAULT 0,
	reasoning INTEGER NOT NULL DEFAULT 0,
	unclassified INTEGER NOT NULL DEFAULT 0,
	total INTEGER NOT NULL DEFAULT 0,
	client_ip TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS hourly (
	hour INTEGER NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	api_key_id TEXT NOT NULL,
	auth_index TEXT NOT NULL,
	service_tier TEXT NOT NULL,
	api_key TEXT NOT NULL DEFAULT '',
	requests INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	input_uncached INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0,
	cache_write INTEGER NOT NULL DEFAULT 0,
	output INTEGER NOT NULL DEFAULT 0,
	reasoning INTEGER NOT NULL DEFAULT 0,
	unclassified INTEGER NOT NULL DEFAULT 0,
	total INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	ttft_count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, provider, model, api_key_id, auth_index, service_tier)
);
`
	// 1. Create DB with old schema
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := rawDB.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	rawDB.Close()

	// 2. Open with usagestore.Open -> triggers migrateSchema
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open migrate: %v", err)
	}
	now := time.Now()
	tok := Tokens{InputUncached: 10, Total: 10}
	s.Enqueue(Event{
		Timestamp:     now.UnixMilli(),
		Provider:      "openai",
		Model:         "gpt-5",
		ResponseModel: "gpt-5-turbo",
		Substituted:   true,
		Tokens:        tok,
	})
	s.Enqueue(Event{
		Timestamp:     now.Add(time.Second).UnixMilli(),
		Provider:      "openai",
		Model:         "gpt-5",
		ResponseModel: "gpt-5",
		Substituted:   false,
		Tokens:        tok,
	})
	s.Close()

	// 3. Re-open and verify queries work
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen migrated: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	st, err := s.Stats(ctx, now.Add(-time.Hour), now.Add(time.Hour), "", 0, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Totals.Requests != 2 || st.Totals.Substituted != 1 {
		t.Fatalf("stats: requests=%d, substituted=%d (want 2, 1)", st.Totals.Requests, st.Totals.Substituted)
	}

	trueVal := true
	subEvents, err := s.Events(ctx, EventFilter{Substituted: &trueVal})
	if err != nil {
		t.Fatalf("events substituted: %v", err)
	}
	if len(subEvents) != 1 || !subEvents[0].Substituted || subEvents[0].ResponseModel != "gpt-5-turbo" {
		t.Fatalf("unexpected substituted events: %+v", subEvents)
	}

	falseVal := false
	unsubEvents, err := s.Events(ctx, EventFilter{Substituted: &falseVal})
	if err != nil {
		t.Fatalf("events unsubstituted: %v", err)
	}
	if len(unsubEvents) != 1 || unsubEvents[0].Substituted || unsubEvents[0].ResponseModel != "gpt-5" {
		t.Fatalf("unexpected unsubstituted events: %+v", unsubEvents)
	}
}

func TestEventsAfterPagination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	tok := Tokens{InputUncached: 10, Total: 10}
	for i := 1; i <= 5; i++ {
		s.Enqueue(Event{
			Timestamp: now.Add(time.Duration(i) * time.Second).UnixMilli(),
			Model:     "gpt-x",
			Tokens:    tok,
		})
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	// Test after: query events after ID 2
	events, err := s.Events(ctx, EventFilter{AfterID: 2, Limit: 2})
	if err != nil {
		t.Fatalf("events after: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].ID != 3 || events[1].ID != 4 {
		t.Fatalf("expected IDs 3, 4 (ASC order), got %d, %d", events[0].ID, events[1].ID)
	}
}

func TestOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	tok := Tokens{InputUncached: 10, Total: 10}
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "p1", Model: "m1", APIKeyID: "k1", APIKey: "sk-1", AuthIndex: "a1", Tokens: tok})
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "p1", Model: "m1", APIKeyID: "k1", APIKey: "sk-1", AuthIndex: "a1", Tokens: tok})
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "p2", Model: "m2", APIKeyID: "k2", APIKey: "sk-2", AuthIndex: "a2", Tokens: tok})
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	opts, err := s.Options(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(opts.Models) != 2 || opts.Models[0].Model != "m1" || opts.Models[0].Requests != 2 {
		t.Fatalf("models: %+v", opts.Models)
	}
	if len(opts.Providers) != 2 || opts.Providers[0].Provider != "p1" || opts.Providers[0].Requests != 2 {
		t.Fatalf("providers: %+v", opts.Providers)
	}
	if len(opts.APIKeys) != 2 || opts.APIKeys[0].APIKeyID != "k1" || opts.APIKeys[0].Requests != 2 {
		t.Fatalf("api keys: %+v", opts.APIKeys)
	}
	if len(opts.Credentials) != 2 || opts.Credentials[0].AuthIndex != "a1" || opts.Credentials[0].Requests != 2 {
		t.Fatalf("credentials: %+v", opts.Credentials)
	}
}

func TestRealtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	p := testPricer(t)
	s, err := Open(path, 30, p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	// Event 30s ago (within 1m and 5m): model gpt-x (input 1, output 10)
	tok1 := Tokens{InputUncached: 100, Output: 100, Total: 200}
	s.Enqueue(Event{
		Timestamp:   now.Add(-30 * time.Second).UnixMilli(),
		Model:       "gpt-x",
		Provider:    "openai",
		Tokens:      tok1,
		LatencyMs:   200,
		Failed:      false,
		Substituted: false,
	})
	// Event 2m ago (within 5m, outside 1m): 1 failed request, 50 tokens
	tok2 := Tokens{InputUncached: 50, Total: 50}
	s.Enqueue(Event{
		Timestamp:   now.Add(-2 * time.Minute).UnixMilli(),
		Model:       "gpt-x",
		Provider:    "openai",
		Tokens:      tok2,
		LatencyMs:   400,
		Failed:      true,
		Substituted: true,
	})
	// Event 10m ago (outside 5m, inside 60m): model other
	tok3 := Tokens{InputUncached: 10, Total: 10}
	s.Enqueue(Event{
		Timestamp: now.Add(-10 * time.Minute).UnixMilli(),
		Model:     "other",
		Provider:  "p2",
		Tokens:    tok3,
		LatencyMs: 100,
	})
	s.Close()
	s, err = Open(path, 30, p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// 1. Unfiltered 60 minutes
	rt, err := s.Realtime(ctx, 60, StatsFilter{})
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}
	if len(rt.Series) != 60 {
		t.Fatalf("expected 60 series points, got %d", len(rt.Series))
	}
	// Check zero-filling: timestamps are multiples of 60
	for i, pt := range rt.Series {
		if pt.Time%60 != 0 {
			t.Fatalf("bucket %d time %d not aligned to 60s", i, pt.Time)
		}
	}
	if rt.LastID != 3 {
		t.Fatalf("expected last_id 3, got %d", rt.LastID)
	}

	// Check current metrics
	// 1m: 1 request, 200 tokens
	approx(t, "rpm_1m", rt.Current.RPM1m, 1.0)
	approx(t, "tpm_1m", rt.Current.TPM1m, 200.0)
	// 5m: 2 requests, 250 tokens -> rpm_5m = 2/5 = 0.4, tpm_5m = 250/5 = 50.0
	approx(t, "rpm_5m", rt.Current.RPM5m, 0.4)
	approx(t, "tpm_5m", rt.Current.TPM5m, 50.0)
	if rt.Current.Failed5m != 1 {
		t.Fatalf("expected 1 failed in 5m, got %d", rt.Current.Failed5m)
	}
	// avg_latency_5m: (200 + 400) / 2 = 300
	approx(t, "avg_latency_5m", rt.Current.AvgLatency5m, 300.0)

	// 2. Filtered by model gpt-x
	rtFiltered, err := s.Realtime(ctx, 60, StatsFilter{Model: []string{"gpt-x"}})
	if err != nil {
		t.Fatalf("realtime filtered: %v", err)
	}
	var totalFilteredReqs int64
	for _, pt := range rtFiltered.Series {
		totalFilteredReqs += pt.Requests
	}
	if totalFilteredReqs != 2 {
		t.Fatalf("expected 2 filtered requests, got %d", totalFilteredReqs)
	}

	// 3. Filtered by non-existent model
	rtNone, err := s.Realtime(ctx, 60, StatsFilter{Model: []string{"none"}})
	if err != nil {
		t.Fatalf("realtime none: %v", err)
	}
	approx(t, "rpm_1m none", rtNone.Current.RPM1m, 0.0)
	approx(t, "rpm_5m none", rtNone.Current.RPM5m, 0.0)
}
