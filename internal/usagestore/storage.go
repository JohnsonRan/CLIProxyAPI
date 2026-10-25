package usagestore

import (
	"context"
	"errors"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

// ErrOptimizeRunning is returned when an optimize is requested while one is still running.
var ErrOptimizeRunning = errors.New("usage store: optimize already running")

// growthWindow is how far back the daily event rate is averaged.
const growthWindow = 7 * 24 * time.Hour

// StorageStatus describes the database files and how fast they fill.
type StorageStatus struct {
	Path string `json:"path"`
	// FileBytes is the main database file; WALBytes the write-ahead log next to it.
	FileBytes int64 `json:"file_bytes"`
	WALBytes  int64 `json:"wal_bytes"`
	// FreeBytes are pages freed by deletes; they are reused, and only VACUUM returns them to the disk.
	FreeBytes   int64 `json:"free_bytes"`
	Events      int64 `json:"events"`
	OldestEvent int64 `json:"oldest_event"` // unix ms, 0 without events
	// EventsPerDay averages the last 7 days (or the time since the oldest event when shorter).
	EventsPerDay float64 `json:"events_per_day"`
	// GrowthBytesPerDay estimates new data per day: used bytes per stored event times EventsPerDay.
	// Hourly rollups and lookups are counted in the per-event size, so it is an upper estimate.
	GrowthBytesPerDay int64          `json:"growth_bytes_per_day"`
	RetentionDays     int64          `json:"retention_days"` // <0 keeps forever
	Optimize          OptimizeStatus `json:"optimize"`
}

// OptimizeStatus reports the running or last optimize.
type OptimizeStatus struct {
	Running       bool   `json:"running"`
	KeepDays      int    `json:"keep_days"`
	StartedAt     int64  `json:"started_at"`  // unix ms, 0 never
	FinishedAt    int64  `json:"finished_at"` // unix ms, 0 while running or never
	BeforeBytes   int64  `json:"before_bytes"`
	AfterBytes    int64  `json:"after_bytes"`
	DeletedEvents int64  `json:"deleted_events"`
	Error         string `json:"error,omitempty"`
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) diskBytes() int64 { return fileSize(s.path) + fileSize(s.path+"-wal") }

// Storage reports database size, free pages and the estimated daily growth.
func (s *Store) Storage(ctx context.Context) (StorageStatus, error) {
	st := StorageStatus{
		Path:          s.path,
		FileBytes:     fileSize(s.path),
		WALBytes:      fileSize(s.path + "-wal"),
		RetentionDays: s.retention.Load(),
	}
	s.optMu.Lock()
	st.Optimize = s.optState
	s.optMu.Unlock()

	var pageSize, freePages int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return st, err
	}
	st.FreeBytes = pageSize * freePages

	var oldest *int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(ts) FROM events`).Scan(&st.Events, &oldest); err != nil {
		return st, err
	}
	if oldest == nil || st.Events == 0 {
		return st, nil
	}
	st.OldestEvent = *oldest
	now := time.Now().UnixMilli()
	from := max(now-growthWindow.Milliseconds(), st.OldestEvent)
	// At least an hour, so a fresh database does not extrapolate a few seconds to a day.
	span := max(now-from, time.Hour.Milliseconds())
	var recent int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE ts >= ?`, from).Scan(&recent); err != nil {
		return st, err
	}
	st.EventsPerDay = float64(recent) * float64(24*time.Hour.Milliseconds()) / float64(span)
	used := max(st.FileBytes-st.FreeBytes, 0)
	st.GrowthBytesPerDay = int64(float64(used) / float64(st.Events) * st.EventsPerDay)
	return st, nil
}

// StartOptimize queues an optimize on the writer: delete events older than keepDays (0 keeps the
// retention setting), then VACUUM to return free pages to the disk. It returns at once; Storage
// reports progress. VACUUM rewrites the file, so it briefly needs up to its size again in free disk.
func (s *Store) StartOptimize(keepDays int) error {
	s.optMu.Lock()
	defer s.optMu.Unlock()
	if s.optState.Running {
		return ErrOptimizeRunning
	}
	s.optState = OptimizeStatus{Running: true, KeepDays: keepDays, StartedAt: time.Now().UnixMilli()}
	select {
	case s.maint <- keepDays:
		return nil
	default:
		// Unreachable while Running guards the one-slot channel; keep the state consistent anyway.
		s.optState.Running = false
		return ErrOptimizeRunning
	}
}

func (s *Store) optimize(keepDays int) {
	res := OptimizeStatus{KeepDays: keepDays, BeforeBytes: s.diskBytes()}
	s.optMu.Lock()
	res.StartedAt = s.optState.StartedAt
	s.optMu.Unlock()

	err := func() error {
		if keepDays > 0 {
			n, err := s.purgeBefore(time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).UnixMilli())
			res.DeletedEvents = n
			if err != nil {
				return err
			}
		} else {
			s.purge()
		}
		for _, stmt := range []string{`PRAGMA wal_checkpoint(TRUNCATE)`, `VACUUM`, `PRAGMA optimize`, `PRAGMA wal_checkpoint(TRUNCATE)`} {
			if _, err := s.db.Exec(stmt); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		res.Error = err.Error()
		log.Errorf("usage store: optimize: %v", err)
	}
	res.AfterBytes = s.diskBytes()
	res.FinishedAt = time.Now().UnixMilli()
	s.optMu.Lock()
	s.optState = res
	s.optMu.Unlock()
	log.Infof("usage store: optimized %s: %d -> %d bytes, %d events deleted", s.path, res.BeforeBytes, res.AfterBytes, res.DeletedEvents)
}
