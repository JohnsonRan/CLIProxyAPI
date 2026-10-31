package usagestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

const fixtureContextTiers = `{
  "google": {"models": {"gem-long": {"cost": {"input": 1, "output": 10, "cache_read": 0.1,
    "tiers": [{"input": 2, "output": 20, "cache_read": 0.2, "tier": {"type": "context", "size": 200000}}]}}}},
  "legacy": {"models": {"old-long": {"cost": {"input": 1, "output": 1,
    "context_over_200k": {"input": 3, "output": 3}}}}}
}`

func TestContextTierParsingAndPricing(t *testing.T) {
	prices, err := parseModelsDev([]byte(fixtureContextTiers))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	gem := prices["gem-long"]
	if len(gem.ContextTiers) != 1 || gem.ContextTiers[0].Above != 200000 || gem.ContextTiers[0].Input != 2 {
		t.Fatalf("gem tiers: %+v", gem.ContextTiers)
	}
	if gem.ContextTiers[0].CacheWrite != 2 {
		t.Fatalf("missing tier cache_write must default to the tier input price: %+v", gem.ContextTiers[0])
	}
	if old := prices["old-long"]; len(old.ContextTiers) != 1 || old.ContextTiers[0].Input != 3 {
		t.Fatalf("context_over_200k: %+v", old.ContextTiers)
	}
	if p := gem.ForInput(200000); p.Input != 1 {
		t.Fatalf("exactly at the threshold stays on the base price: %+v", p)
	}
	if p := gem.ForInput(200001); p.Input != 2 || p.ContextTiers != nil {
		t.Fatalf("above the threshold uses the tier: %+v", p)
	}
	if got := contextBandSQL(nil); got != "0" {
		t.Fatalf("no thresholds: %q", got)
	}
}

func TestExactStatsFromEvents(t *testing.T) {
	prices, err := parseModelsDev([]byte(fixtureContextTiers))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := NewPricer("")
	p.modelsDev = prices
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	at := func(d time.Duration) int64 { return base.Add(d).UnixMilli() }
	small := Tokens{InputUncached: 100_000, Output: 100_000, Total: 200_000}
	long := Tokens{InputUncached: 300_000, Output: 100_000, Total: 400_000}
	s.Enqueue(Event{Timestamp: at(10 * time.Minute), Model: "gem-long", Tokens: small})
	s.Enqueue(Event{Timestamp: at(40 * time.Minute), Model: "gem-long", Tokens: long, Failed: true})
	s.Enqueue(Event{Timestamp: at(50 * time.Minute), Model: "gem-long", Tokens: small, Substituted: true})
	s.Close()
	s, err = Open(s.path, 30, p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// A 30-minute window inside one hour: rollups would count all three requests.
	st, err := s.Stats(ctx, base.Add(30*time.Minute), base.Add(time.Hour), "", 0, StatsFilter{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !st.Exact || st.Partial || st.Totals.Requests != 2 || st.Totals.Failed != 1 || st.Totals.Substituted != 1 {
		t.Fatalf("exact window: %+v", st.Totals)
	}
	// Long request: 300k input at $2 + 100k output at $20; small request: 100k at $1 + 100k at $10.
	approx(t, "tiered cost", st.Totals.Cost, 0.3*2+0.1*20+0.1*1+0.1*10)

	failed := true
	st, err = s.Stats(ctx, base, base.Add(time.Hour), "", 0, StatsFilter{Failed: &failed})
	if err != nil || st.Totals.Requests != 1 || st.Totals.Failed != 1 {
		t.Fatalf("failed filter: %v %+v", err, st.Totals)
	}
	st, err = s.Stats(ctx, base, base.Add(time.Hour), "", 0, StatsFilter{Substituted: &failed})
	if err != nil || st.Totals.Requests != 1 || st.Totals.Substituted != 1 {
		t.Fatalf("substituted filter: %v %+v", err, st.Totals)
	}

	// Past the retention window: rollups, whole hours, base prices.
	s.SetRetentionDays(1)
	old := time.Now().Add(-72 * time.Hour)
	if !s.coversRange(time.Now().Add(-time.Hour)) || s.coversRange(old) {
		t.Fatal("coversRange must follow the retention window")
	}
	st, err = s.Stats(ctx, old, base.Add(time.Hour), "", 0, StatsFilter{})
	if err != nil || st.Exact || st.Totals.Requests != 3 {
		t.Fatalf("rollup path: %v exact=%v %+v", err, st.Exact, st.Totals)
	}
	approx(t, "rollup base cost", st.Totals.Cost, 0.5*1+0.3*10)
	st, err = s.Stats(ctx, old, base.Add(time.Hour), "", 0, StatsFilter{Failed: &failed})
	if err != nil || !st.Exact || !st.Partial || st.Totals.Requests != 1 {
		t.Fatalf("result filter forces events and flags partial: %v %+v", err, st)
	}
}
