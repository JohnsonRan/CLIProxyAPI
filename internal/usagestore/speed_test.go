package usagestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputSpeed(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, NewPricer(""))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	ts := base.Add(10 * time.Minute).UnixMilli()
	ev := func(stream, failed bool, latency, ttft, output int64) Event {
		return Event{Timestamp: ts, Model: "m", Stream: stream, Failed: failed, LatencyMs: latency, TTFTMs: ttft,
			Tokens: Tokens{Output: output, Total: output}}
	}
	// Measured: 100 tokens in 1 s (100 t/s) and 300 tokens in 1 s (300 t/s).
	s.Enqueue(ev(true, false, 1500, 500, 100))
	s.Enqueue(ev(true, false, 1200, 200, 300))
	// Excluded: non-streamed, failed, no first token, burst under minGenerationMs, no output.
	s.Enqueue(ev(false, false, 2000, 500, 100))
	s.Enqueue(ev(true, true, 2000, 500, 100))
	s.Enqueue(ev(true, false, 2000, 0, 100))
	s.Enqueue(ev(true, false, 550, 500, 100))
	s.Enqueue(ev(true, false, 2000, 500, 0))
	s.Close()
	s, err = Open(s.path, 30, s.pricer)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Token-weighted: 400 tokens over 2 s.
	st, err := s.Stats(ctx, base, base.Add(time.Hour), "", 0, StatsFilter{})
	if err != nil || !st.Exact {
		t.Fatalf("events stats: %v %+v", err, st)
	}
	approx(t, "events output_tps", st.Totals.OutputTPS, 200)
	approx(t, "model output_tps", st.Models[0].OutputTPS, 200)

	// Rollups must agree with the events path.
	s.SetRetentionDays(1)
	st, err = s.Stats(ctx, time.Now().Add(-72*time.Hour), base.Add(time.Hour), "", 0, StatsFilter{})
	if err != nil || st.Exact {
		t.Fatalf("rollup stats: %v %+v", err, st)
	}
	approx(t, "rollup output_tps", st.Totals.OutputTPS, 200)

	events, err := s.Events(ctx, EventFilter{})
	if err != nil || len(events) != 7 {
		t.Fatalf("events: %v %d", err, len(events))
	}
	measured := map[float64]int{}
	for _, e := range events {
		measured[e.OutputTPS]++
	}
	if measured[100] != 1 || measured[300] != 1 || measured[0] != 5 {
		t.Fatalf("per-event output_tps: %v", measured)
	}

	a, err := s.Analysis(ctx, base, base.Add(time.Hour), 0, StatsFilter{})
	if err != nil {
		t.Fatalf("analysis: %v", err)
	}
	if a.TPS.Count != 2 || a.TPS.P50 != 300 || a.TPS.Max != 300 || a.Models[0].TPS.Count != 2 {
		t.Fatalf("analysis tps: %+v models %+v", a.TPS, a.Models)
	}
}

func TestMigrateAddsGenerationColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// An hourly table from before gen_output/gen_ms existed.
	if _, err = db.Exec(`CREATE TABLE hourly (hour INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
		api_key_id TEXT NOT NULL, auth_index TEXT NOT NULL, service_tier TEXT NOT NULL, api_key TEXT NOT NULL DEFAULT '',
		requests INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0, input_uncached INTEGER NOT NULL DEFAULT 0,
		cache_read INTEGER NOT NULL DEFAULT 0, cache_write INTEGER NOT NULL DEFAULT 0, output INTEGER NOT NULL DEFAULT 0,
		reasoning INTEGER NOT NULL DEFAULT 0, unclassified INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0, ttft_ms INTEGER NOT NULL DEFAULT 0, ttft_count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (hour, provider, model, api_key_id, auth_index, service_tier))`); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	_ = db.Close()
	s, err := Open(path, 30, NewPricer(""))
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`SELECT gen_output, gen_ms FROM hourly`); err != nil {
		t.Fatalf("columns missing after migrate: %v", err)
	}
}
