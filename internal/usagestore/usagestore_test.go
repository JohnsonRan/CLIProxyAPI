package usagestore

import (
	"context"
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

	st, err := s.Stats(ctx, day, day.Add(48*time.Hour), "", 0)
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
	st, err = s.Stats(ctx, day.Add(-8*time.Hour), day.Add(40*time.Hour), "day", 8*60)
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
	page, err := s.Events(ctx, EventFilter{Failed: &failed, Model: "gpt-x"})
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
