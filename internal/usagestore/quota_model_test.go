package usagestore

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestSolveGaussian(t *testing.T) {
	// (f) solveGaussian on a known 3x3 system:
	//  2x +  y -  z = 8
	// -3x -  y + 2z = -11
	// -2x +  y + 2z = -3
	// Exact solution: x = 2, y = 3, z = -1
	A := [][]float64{
		{2, 1, -1},
		{-3, -1, 2},
		{-2, 1, 2},
	}
	b := []float64{8, -11, -3}
	x, ok := solveGaussian(A, b)
	if !ok {
		t.Fatalf("solveGaussian failed")
	}
	want := []float64{2, 3, -1}
	for i := range want {
		if math.Abs(x[i]-want[i]) > 1e-6 {
			t.Fatalf("x[%d] = %v, want %v", i, x[i], want[i])
		}
	}
}

func TestFitLayer1Exact(t *testing.T) {
	// (a) synthetic segments with Δu = 2·cost exactly → k=2, full_cost=50, ci ~0
	segs := []quotaSegment{
		{windowIdx: 0, cost: 10, deltaU: 20},
		{windowIdx: 0, cost: 5, deltaU: 10},
		{windowIdx: 1, cost: 8, deltaU: 16},
		{windowIdx: 1, cost: 12, deltaU: 24},
	}
	weights := []float64{1, 1, 1, 1}
	res := fitLayer1(segs, weights)
	if res.status != "fitted" {
		t.Fatalf("status = %q, want fitted", res.status)
	}
	if math.Abs(res.k-2.0) > 1e-6 {
		t.Fatalf("k = %v, want 2.0", res.k)
	}
	if res.fullCost == nil || math.Abs(*res.fullCost-50.0) > 1e-6 {
		t.Fatalf("fullCost = %v, want 50.0", res.fullCost)
	}
	if res.ciPercent == nil || math.Abs(*res.ciPercent) > 1e-4 {
		t.Fatalf("ciPercent = %v, want ~0", res.ciPercent)
	}
}

func TestFitLayer1ExternalJump(t *testing.T) {
	// (b) adding one segment with Δu=10, cost≈0 marks it external and k stays 2
	segs := []quotaSegment{
		{windowIdx: 0, cost: 10, deltaU: 20},
		{windowIdx: 0, cost: 5, deltaU: 10},
		{windowIdx: 1, cost: 8, deltaU: 16},
		{windowIdx: 1, cost: 12, deltaU: 24},
		{windowIdx: 1, cost: 0.0001, deltaU: 10}, // jump
	}
	weights := []float64{1, 1, 1, 1, 1}
	res := fitLayer1(segs, weights)
	if res.status != "fitted" {
		t.Fatalf("status = %q, want fitted", res.status)
	}
	if math.Abs(res.k-2.0) > 1e-3 {
		t.Fatalf("k = %v, want ~2.0", res.k)
	}
	if res.externalSegments != 1 {
		t.Fatalf("externalSegments = %d, want 1", res.externalSegments)
	}
}

func TestFitLayer2MultiModel(t *testing.T) {
	// (c) two models where model B consumes 3× its price share: multipliers ≈ (1, 3) within 10% and model_values reflect it
	k := 1.0
	var segs []quotaSegment
	var weights []float64
	for i := 0; i < 10; i++ {
		// Model A: cost 10, deltaU 10 (1x)
		segs = append(segs, quotaSegment{
			windowIdx: i / 5,
			cost:      10,
			deltaU:    10,
			kindCosts: map[segmentModelKind]float64{{model: "modelA", kind: "output"}: 10},
		})
		weights = append(weights, 1.0)
		// Model B: cost 10, deltaU 30 (3x)
		segs = append(segs, quotaSegment{
			windowIdx: i / 5,
			cost:      10,
			deltaU:    30,
			kindCosts: map[segmentModelKind]float64{{model: "modelB", kind: "output"}: 10},
		})
		weights = append(weights, 1.0)
	}

	feats, mvals := fitLayer2(segs, weights, k, make(map[int]bool))
	if len(feats) < 2 {
		t.Fatalf("len(feats) = %d, want >= 2", len(feats))
	}
	multMap := make(map[string]float64)
	for _, f := range feats {
		multMap[f.Model] = f.Multiplier
	}
	mA, mB := multMap["modelA"], multMap["modelB"]
	if math.Abs(mA-1.0) > 0.15 {
		t.Fatalf("multiplier modelA = %v, want ~1.0", mA)
	}
	if math.Abs(mB-3.0) > 0.3 {
		t.Fatalf("multiplier modelB = %v, want ~3.0", mB)
	}

	mvMap := make(map[string]float64)
	for _, mv := range mvals {
		mvMap[mv.Model] = mv.FullCost
	}
	fcA, fcB := mvMap["modelA"], mvMap["modelB"]
	if math.Abs(fcA-100.0) > 15.0 {
		t.Fatalf("fullCost modelA = %v, want ~100.0", fcA)
	}
	if math.Abs(fcB-33.3) > 5.0 {
		t.Fatalf("fullCost modelB = %v, want ~33.3", fcB)
	}
}

func TestFitLayer2SingleModel(t *testing.T) {
	// (d) single-model data → multipliers ≈ 1 (ridge keeps it near prior when unidentifiable)
	k := 2.0
	var segs []quotaSegment
	var weights []float64
	for i := 0; i < 6; i++ {
		segs = append(segs, quotaSegment{
			windowIdx: i / 3,
			cost:      10,
			deltaU:    20,
			kindCosts: map[segmentModelKind]float64{{model: "gpt-x", kind: "output"}: 10},
		})
		weights = append(weights, 1.0)
	}
	feats, _ := fitLayer2(segs, weights, k, make(map[int]bool))
	if len(feats) != 1 {
		t.Fatalf("len(feats) = %d, want 1", len(feats))
	}
	if math.Abs(feats[0].Multiplier-1.0) > 0.05 {
		t.Fatalf("multiplier = %v, want ~1.0", feats[0].Multiplier)
	}
}

func TestFitLayer1Insufficient(t *testing.T) {
	// (e) fewer than 3 segments → insufficient
	segs := []quotaSegment{
		{windowIdx: 0, cost: 10, deltaU: 20},
		{windowIdx: 0, cost: 5, deltaU: 10},
	}
	weights := []float64{1, 1}
	res := fitLayer1(segs, weights)
	if res.status != "insufficient" {
		t.Fatalf("status = %q, want insufficient", res.status)
	}
}

func TestQuotaValueEndToEndModel(t *testing.T) {
	// (g) an end-to-end QuotaValue test via the store returning a fitted model with sane fields
	path := filepath.Join(t.TempDir(), "usage_model.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000)
	tok := Tokens{InputUncached: 1_000_000, Output: 0, Total: 1_000_000} // $1.00 each

	// Window 1: T0 to T0+18000
	// 3 samples: 10%, 20%, 30%
	s.Enqueue(Event{Timestamp: (T0 + 500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})
	s.Enqueue(Event{Timestamp: (T0 + 1500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})
	s.Enqueue(Event{Timestamp: (T0 + 2500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})

	// Window 2: T0+18000 to T0+36000
	// 3 samples: 10%, 20%, 30%
	s.Enqueue(Event{Timestamp: (T0 + 18500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})
	s.Enqueue(Event{Timestamp: (T0 + 19500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})
	s.Enqueue(Event{Timestamp: (T0 + 20500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "m1", Tokens: tok})
	s.Close()

	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at) VALUES
		("m1", "codex", "primary", 18000, ?, 10, ?),
		("m1", "codex", "primary", 18000, ?, 20, ?),
		("m1", "codex", "primary", 18000, ?, 30, ?),
		("m1", "codex", "primary", 18000, ?, 10, ?),
		("m1", "codex", "primary", 18000, ?, 20, ?),
		("m1", "codex", "primary", 18000, ?, 30, ?)`,
		T0+18000, (T0+1000)*1000,
		T0+18000, (T0+2000)*1000,
		T0+18000, (T0+3000)*1000,
		T0+36000, (T0+19000)*1000,
		T0+36000, (T0+20000)*1000,
		T0+36000, (T0+21000)*1000,
	)

	val, err := s.QuotaValue(context.Background(), time.Unix(T0, 0), time.Unix(T0+40000, 0), []string{"m1"})
	if err != nil {
		t.Fatalf("QuotaValue: %v", err)
	}
	if len(val.Models) != 1 {
		t.Fatalf("len(Models) = %d, want 1", len(val.Models))
	}
	m := val.Models[0]
	if m.AuthIndex != "m1" || m.PeriodSeconds != 18000 || m.Status != "fitted" {
		t.Fatalf("model = %+v", m)
	}
	if m.FullCost == nil || *m.FullCost <= 0 {
		t.Fatalf("fullCost = %v, want > 0", m.FullCost)
	}
	approx(t, "model fullCost", *m.FullCost, 10.0)
	if len(m.Features) == 0 {
		t.Fatalf("features = %+v, want non-empty", m.Features)
	}
}

func TestQuotaValueUnpricedWindowNotDeducted(t *testing.T) {
	// (b) a fitted model exists and a later window has only unpriced local traffic
	// -> that window's external_percent == 0 and own_percent == used_percent
	path := filepath.Join(t.TempDir(), "usage_unpriced.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	T0 := int64(1_800_000_000)
	tokPriced := Tokens{InputUncached: 1_000_000, Total: 1_000_000} // gpt-x is priced $1.00
	tokUnpriced := Tokens{InputUncached: 1_000_000, Total: 1_000_000}

	// Window 1: priced traffic to fit the model
	s.Enqueue(Event{Timestamp: (T0 + 500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "u1", Tokens: tokPriced})
	s.Enqueue(Event{Timestamp: (T0 + 1500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "u1", Tokens: tokPriced})
	s.Enqueue(Event{Timestamp: (T0 + 2500) * 1000, Provider: "codex", Model: "gpt-x", AuthIndex: "u1", Tokens: tokPriced})

	// Window 2: unpriced traffic only
	s.Enqueue(Event{Timestamp: (T0 + 18500) * 1000, Provider: "codex", Model: "unpriced-model", AuthIndex: "u1", Tokens: tokUnpriced})
	s.Enqueue(Event{Timestamp: (T0 + 19500) * 1000, Provider: "codex", Model: "unpriced-model", AuthIndex: "u1", Tokens: tokUnpriced})
	s.Enqueue(Event{Timestamp: (T0 + 20500) * 1000, Provider: "codex", Model: "unpriced-model", AuthIndex: "u1", Tokens: tokUnpriced})
	s.Close()

	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	s.db.Exec(`INSERT INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at) VALUES
		("u1", "codex", "primary", 18000, ?, 10, ?),
		("u1", "codex", "primary", 18000, ?, 20, ?),
		("u1", "codex", "primary", 18000, ?, 30, ?),
		("u1", "codex", "primary", 18000, ?, 10, ?),
		("u1", "codex", "primary", 18000, ?, 20, ?),
		("u1", "codex", "primary", 18000, ?, 30, ?)`,
		T0+18000, (T0+1000)*1000,
		T0+18000, (T0+2000)*1000,
		T0+18000, (T0+3000)*1000,
		T0+36000, (T0+19000)*1000,
		T0+36000, (T0+20000)*1000,
		T0+36000, (T0+21000)*1000,
	)

	val, err := s.QuotaValue(context.Background(), time.Unix(T0, 0), time.Unix(T0+40000, 0), []string{"u1"})
	if err != nil {
		t.Fatalf("QuotaValue: %v", err)
	}
	if len(val.Windows) != 2 {
		t.Fatalf("len(Windows) = %d, want 2", len(val.Windows))
	}
	// Windows are sorted newest reset first, so val.Windows[0] is Window 2 (reset T0+36000)
	w2 := val.Windows[0]
	if w2.ResetAt != T0+36000 {
		t.Fatalf("w2.ResetAt = %d, want %d", w2.ResetAt, T0+36000)
	}
	if w2.UsedPercent != 30 {
		t.Fatalf("w2.UsedPercent = %v, want 30", w2.UsedPercent)
	}
	if w2.ExternalPercent != 0 {
		t.Fatalf("w2.ExternalPercent = %v, want 0 (unpriced traffic must not be deducted as external)", w2.ExternalPercent)
	}
	if w2.OwnPercent != 30 {
		t.Fatalf("w2.OwnPercent = %v, want 30", w2.OwnPercent)
	}
	if w2.Usage.UnpricedRequests != 3 {
		t.Fatalf("w2.Usage.UnpricedRequests = %d, want 3", w2.Usage.UnpricedRequests)
	}
}

func TestCombinedLayer1Layer2Regression(t *testing.T) {
	// 27 segments of model A ($10 → 10%) and 3 segments of model B ($10 → 30%) all local
	// -> model B is NOT external and gets multiplier ≈ 3 (within 25%),
	// and a separate idle-jump segment (Δu 10, cost ≈ 0) IS external.
	var segs []quotaSegment
	var weights []float64

	for i := 0; i < 27; i++ {
		segs = append(segs, quotaSegment{
			windowIdx: i / 9,
			cost:      10,
			deltaU:    10,
			kindCosts: map[segmentModelKind]float64{{model: "modelA", kind: "output"}: 10},
		})
		weights = append(weights, 1.0)
	}
	for i := 0; i < 3; i++ {
		segs = append(segs, quotaSegment{
			windowIdx: i,
			cost:      10,
			deltaU:    30,
			kindCosts: map[segmentModelKind]float64{{model: "modelB", kind: "output"}: 10},
		})
		weights = append(weights, 1.0)
	}
	// Idle-jump segment: Δu 10, cost ≈ 0
	jumpIdx := len(segs)
	segs = append(segs, quotaSegment{
		windowIdx: 0,
		cost:      0.0001,
		deltaU:    10,
		kindCosts: map[segmentModelKind]float64{{model: "modelA", kind: "output"}: 0.0001},
	})
	weights = append(weights, 1.0)

	res1 := fitLayer1(segs, weights)
	if res1.status != "fitted" {
		t.Fatalf("fitLayer1 status = %s, want fitted", res1.status)
	}
	if !res1.isExternal[jumpIdx] {
		t.Fatalf("idle jump segment (idx %d) must be external", jumpIdx)
	}
	if res1.isExternal[27] || res1.isExternal[28] || res1.isExternal[29] {
		t.Fatalf("model B segments must NOT be external")
	}

	feats, _ := fitLayer2(segs, weights, res1.k, res1.isExternal)
	multMap := make(map[string]float64)
	for _, f := range feats {
		multMap[f.Model] = f.Multiplier
	}
	mB := multMap["modelB"]
	// Within 25% of 3.0
	if math.Abs(mB-3.0) > 0.75 {
		t.Fatalf("multiplier modelB = %v, want ~3.0 (within 25%%)", mB)
	}
}

func TestPriceScaleInvariance(t *testing.T) {
	// Multiplying all prices by 100 leaves ci_percent unchanged (within 1e-9 relative).
	segs1 := []quotaSegment{
		{windowIdx: 0, cost: 10, deltaU: 10},
		{windowIdx: 0, cost: 10, deltaU: 10},
		{windowIdx: 1, cost: 10, deltaU: 15},
		{windowIdx: 1, cost: 10, deltaU: 15},
	}
	weights := []float64{1.0, 1.0, 1.0, 1.0}
	res1 := fitLayer1(segs1, weights)
	if res1.status != "fitted" || res1.ciPercent == nil {
		t.Fatalf("res1 = %+v, want fitted with ciPercent", res1)
	}

	segs2 := make([]quotaSegment, len(segs1))
	for i, s := range segs1 {
		segs2[i] = s
		segs2[i].cost = s.cost * 100.0
	}
	res2 := fitLayer1(segs2, weights)
	if res2.status != "fitted" || res2.ciPercent == nil {
		t.Fatalf("res2 = %+v, want fitted with ciPercent", res2)
	}

	relDiff := math.Abs(*res1.ciPercent-*res2.ciPercent) / *res1.ciPercent
	if relDiff > 1e-9 {
		t.Fatalf("ciPercent not scale invariant: res1=%v, res2=%v, relDiff=%v", *res1.ciPercent, *res2.ciPercent, relDiff)
	}
}
