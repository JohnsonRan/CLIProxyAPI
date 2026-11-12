package usagestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageAndOptimize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour).UnixMilli()
	padding := strings.Repeat("x", 400)
	for i := 0; i < 1000; i++ {
		s.Enqueue(Event{Timestamp: old, Provider: "codex", Model: "m", Error: padding})
		s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "codex", Model: "m", Error: padding})
	}
	s.Close()
	s, err = Open(path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	st, err := s.Storage(context.Background())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if st.Events != 2000 || st.OldestEvent != old || st.FileBytes <= 0 || st.RetentionDays != 30 {
		t.Fatalf("storage: %+v", st)
	}
	// 1000 events in the last 7 days.
	approx(t, "events per day", st.EventsPerDay, 1000.0/7)
	if st.GrowthBytesPerDay <= 0 || st.GrowthBytesPerDay > st.FileBytes {
		t.Fatalf("growth: %+v", st)
	}

	if err = s.StartOptimize(10); err != nil {
		t.Fatalf("start optimize: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, err = s.Storage(context.Background()); err != nil {
			t.Fatalf("storage: %v", err)
		}
		if !st.Optimize.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("optimize did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	o := st.Optimize
	if o.Error != "" || o.KeepDays != 10 || o.DeletedEvents != 1000 || o.StartedAt == 0 || o.FinishedAt < o.StartedAt {
		t.Fatalf("optimize: %+v", o)
	}
	if o.AfterBytes >= o.BeforeBytes || st.Events != 1000 || st.FreeBytes != 0 {
		t.Fatalf("vacuum must shrink the file and leave no free pages: %+v", st)
	}

	// Recording keeps working after the vacuum.
	s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "codex", Model: "m"})
	deadline = time.Now().Add(5 * time.Second)
	for st.Events != 1001 {
		if time.Now().After(deadline) {
			t.Fatalf("event after optimize not written: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
		if st, err = s.Storage(context.Background()); err != nil {
			t.Fatalf("storage: %v", err)
		}
	}

	s.optMu.Lock()
	s.optState.Running = true
	s.optMu.Unlock()
	if err = s.StartOptimize(0); !errors.Is(err, ErrOptimizeRunning) {
		t.Fatalf("second optimize must be refused: %v", err)
	}
}

func TestOptimizeDefaultAndNegativeRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path, 15, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour).UnixMilli()
	for i := 0; i < 50; i++ {
		s.Enqueue(Event{Timestamp: old, Provider: "codex", Model: "m"})
		s.Enqueue(Event{Timestamp: now.UnixMilli(), Provider: "codex", Model: "m"})
	}

	waitForEvents := func(want int64) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			st, err := s.Storage(context.Background())
			if err == nil && st.Events == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d events (have %d)", want, st.Events)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitForEvents(100)

	// keepDays == 0 uses s.retention (15 days), so the 50 events at 20 days ago are deleted.
	if err = s.StartOptimize(0); err != nil {
		t.Fatalf("start optimize: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var st StorageStatus
	for {
		if st, err = s.Storage(context.Background()); err != nil {
			t.Fatalf("storage: %v", err)
		}
		if !st.Optimize.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("optimize did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Optimize.DeletedEvents != 50 || st.Events != 50 || st.Optimize.Error != "" {
		t.Fatalf("default retention optimize: %+v (events: %d)", st.Optimize, st.Events)
	}

	// Negative retention keeps forever: nothing is deleted when keepDays == 0.
	s.SetRetentionDays(-1)
	for i := 0; i < 50; i++ {
		s.Enqueue(Event{Timestamp: old, Provider: "codex", Model: "m"})
	}
	waitForEvents(100)

	if err = s.StartOptimize(0); err != nil {
		t.Fatalf("start optimize with negative retention: %v", err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		if st, err = s.Storage(context.Background()); err != nil {
			t.Fatalf("storage: %v", err)
		}
		if !st.Optimize.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("optimize did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Optimize.DeletedEvents != 0 || st.Events != 100 || st.Optimize.Error != "" {
		t.Fatalf("negative retention optimize: %+v (events: %d)", st.Optimize, st.Events)
	}
}
