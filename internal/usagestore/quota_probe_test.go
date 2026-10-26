package usagestore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseAntigravityQuota(t *testing.T) {
	body := []byte(`{"response":{"groups":[
		{"displayName":"Gemini Models","buckets":[
			{"window":"weekly","remainingFraction":0.96,"resetTime":"2027-01-08T00:00:00.123Z"},
			{"window":"5h","remaining":{"remainingFraction":0.87},"resetTime":"2027-01-02T00:00:00Z"},
			{"window":"5h","remainingFraction":0.5,"resetTime":"2027-01-02T00:00:00Z"}]},
		{"displayName":"Claude and GPT models","buckets":[
			{"window":"weekly","remainingFraction":1,"resetTime":"2027-01-08T00:00:00Z"},
			{"window":"5h","resetTime":"2027-01-02T00:00:00Z"},
			{"window":"monthly","remainingFraction":0.1,"resetTime":"2027-02-01T00:00:00Z"}]},
		{"displayName":"Image Models","buckets":[{"window":"5h","remainingFraction":0.1,"resetTime":"2027-01-02T00:00:00Z"}]}]}}`)
	got := parseAntigravityQuota(body)
	reset5h := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC).Unix()
	// Untouched, fractionless, unknown-window and unknown-group buckets are skipped; the first
	// bucket of a window wins.
	if len(got) != 2 ||
		got[0] != (quotaWindow{ID: "gemini-weekly", Period: 7 * 86400, ResetAt: time.Date(2027, 1, 8, 0, 0, 0, 0, time.UTC).Unix(), Used: 4}) ||
		got[1] != (quotaWindow{ID: "gemini-5h", Period: 5 * 3600, ResetAt: reset5h, Used: 13}) {
		t.Fatalf("windows = %+v", got)
	}
	bare := parseAntigravityQuota([]byte(`{"groups":[{"displayName":"Claude and GPT models","buckets":[{"window":"5h","remainingFraction":0.75,"resetTime":"2027-01-02T00:00:00Z"}]}]}`))
	if len(bare) != 1 || bare[0].ID != "claude-gpt-5h" || bare[0].Used != 25 {
		t.Fatalf("bare body windows = %+v", bare)
	}
	if quotaPool("antigravity", "claude-gpt-5h") != "claude-gpt" || quotaPool("codex", "primary") != "" {
		t.Fatal("quotaPool")
	}
}

func TestAntigravityQuotaValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	pricer := testPricer(t)
	pricer.Configure(config.UsagePricingConfig{Overrides: []config.UsagePriceOverride{{Model: "gemini-z", Input: 1, Output: 10}}})
	s, err := Open(path, 30, pricer)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	// Each event costs $2: two Gemini events and one GPT event inside the 5h windows.
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, e := range []struct {
		ago   time.Duration
		model string
	}{{90 * time.Minute, "gemini-z"}, {60 * time.Minute, "gpt-x"}, {30 * time.Minute, "gemini-z"}} {
		s.Enqueue(Event{Timestamp: now.Add(-e.ago).UnixMilli(), Provider: "antigravity", Model: e.model, AuthIndex: "ag1", Tokens: tok})
	}
	s.Enqueue(Event{Timestamp: now.Add(-time.Hour).UnixMilli(), Provider: "codex", Model: "gpt-x", AuthIndex: "cx1", Tokens: tok})
	s.Close()
	if _, ok := s.quotaActive.Load("ag1"); !ok {
		t.Fatal("antigravity request must mark its credential for probing")
	}
	if _, ok := s.quotaActive.Load("cx1"); ok {
		t.Fatal("codex request must not mark its credential for probing")
	}
	s, err = Open(path, 30, pricer)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	reset := time.Unix(now.Add(3*time.Hour).Unix(), 0).UTC().Format(time.RFC3339)
	probes := 0
	SetQuotaProbe(func(_ context.Context, auth *coreauth.Auth) ([]byte, error) {
		probes++
		return []byte(fmt.Sprintf(`{"groups":[
			{"displayName":"Gemini Models","buckets":[{"window":"5h","remainingFraction":0.8,"resetTime":%q}]},
			{"displayName":"Claude and GPT models","buckets":[{"window":"5h","remainingFraction":0.9,"resetTime":%q}]}]}`, reset, reset)), nil
	})
	auths := []*coreauth.Auth{{ID: "ag1", Index: "ag1", Provider: "antigravity"}, {ID: "ag2", Index: "ag2", Provider: "antigravity"}}
	SetAuthLister(func() []*coreauth.Auth { return auths })
	defer SetAuthLister(nil)
	defer SetQuotaProbe(nil)

	last := map[string]quotaLast{}
	s.quotaActive.Store("ag1", struct{}{})
	s.probeQuota(last)
	s.probeQuota(last) // no new requests: no probe
	if probes != 1 {
		t.Fatalf("probes = %d, want 1 (idle credentials are not probed)", probes)
	}

	value, err := s.QuotaValue(context.Background(), now.Add(-24*time.Hour), now.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	if len(value.Windows) != 2 || len(value.Models) != 2 {
		t.Fatalf("value = %+v", value)
	}
	byPool := map[string]QuotaWindowValue{}
	for _, w := range value.Windows {
		byPool[w.Pool] = w
	}
	gemini, thirdParty := byPool["gemini"], byPool["claude-gpt"]
	if gemini.Window != "gemini-5h" || gemini.Usage.Requests != 2 || gemini.FullCost == nil {
		t.Fatalf("gemini window = %+v", gemini)
	}
	approx(t, "gemini full cost", *gemini.FullCost, 20)
	if thirdParty.Window != "claude-gpt-5h" || thirdParty.Usage.Requests != 1 || thirdParty.FullCost == nil {
		t.Fatalf("claude-gpt window = %+v", thirdParty)
	}
	approx(t, "claude-gpt full cost", *thirdParty.FullCost, 20)
	if value.Models[0].Pool != "claude-gpt" || value.Models[1].Pool != "gemini" {
		t.Fatalf("models must be fitted per pool: %+v", value.Models)
	}
}

func TestClaudeFableQuotaValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	pricer := testPricer(t)
	pricer.Configure(config.UsagePricingConfig{Overrides: []config.UsagePriceOverride{
		{Model: "claude-fable-5-1", Input: 1, Output: 10}, // $2 per event
		{Model: "claude-opus-5", Input: 0.5, Output: 5},   // $1 per event
		{Model: "claude-sonnet-5", Input: 0.5, Output: 5},
	}})
	s, err := Open(path, 30, pricer)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	T0 := int64(1_800_000_000)
	tok := Tokens{InputUncached: 1_000_000, Output: 100_000, Total: 1_100_000}
	for _, e := range []struct {
		auth   string
		at     int64
		model  string
		served string
	}{
		{"c1", 100, "claude-fable-5-1", ""},
		{"c1", 200, "claude-sonnet-5", ""},
		{"c1", 300, "claude-fable-5-1", "claude-opus-5"}, // Opus fallback: shared windows, Opus price
		{"c1", 500, "claude-fable-5-1", ""},              // Fable window exhausted: usage credits
		{"c1", 600, "claude-sonnet-5", ""},
		{"c2", 100, "claude-fable-5-1", ""}, // plan without Fable: always credits
		{"c2", 200, "claude-sonnet-5", ""},
	} {
		s.Enqueue(Event{Timestamp: (T0 + e.at) * 1000, Provider: "claude", Model: e.model, AuthIndex: e.auth, Tokens: tok,
			ResponseModel: e.served, Substituted: e.served != ""})
	}
	s.Close()
	s, err = Open(path, 30, pricer)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	reset := T0 + 604800
	for _, sm := range []struct {
		auth, window string
		used         float64
		at           int64
	}{
		{"c1", "7d_oi", 50, 150}, {"c1", "7d_oi", 100, 400}, {"c1", "7d", 10, 250}, {"c1", "7d", 20, 700},
		{"c2", "7d_oi", 102, 300}, {"c2", "7d", 5, 400},
	} {
		if _, errExec := s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
			VALUES (?, 'claude', ?, 604800, ?, ?, ?)`, sm.auth, sm.window, reset, sm.used, (T0+sm.at)*1000); errExec != nil {
			t.Fatalf("insert sample: %v", errExec)
		}
	}

	value, err := s.QuotaValue(context.Background(), time.Unix(T0-1000, 0), time.Unix(T0+10000, 0), nil)
	if err != nil {
		t.Fatalf("quota value: %v", err)
	}
	got := map[string]QuotaWindowValue{}
	for _, w := range value.Windows {
		got[w.AuthIndex+"/"+w.Window] = w
	}
	if w := got["c1/7d_oi"]; w.Pool != "fable" || w.Usage.Requests != 1 {
		t.Fatalf("c1 fable window = %+v, want only the Fable-served request before exhaustion", w)
	} else {
		approx(t, "c1 fable cost", w.Usage.Cost, 2)
	}
	if w := got["c1/7d"]; w.Pool != "" || w.Usage.Requests != 4 {
		t.Fatalf("c1 7d window = %+v, want every request but the credit-paid Fable one", w)
	} else {
		approx(t, "c1 7d cost", w.Usage.Cost, 2+1+1+1)
	}
	if w := got["c2/7d"]; w.Usage.Requests != 1 {
		t.Fatalf("c2 7d window = %+v, want the Sonnet request only", w)
	}
}
