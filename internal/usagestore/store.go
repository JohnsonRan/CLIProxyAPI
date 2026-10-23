// Package usagestore persists usage records to a local SQLite database and
// serves aggregated statistics and cost estimates to the management API.
package usagestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	writeBuffer   = 8192
	writeBatch    = 512
	flushInterval = time.Second
	purgeInterval = time.Hour
	purgeChunk    = 5000
)

const schema = `
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
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
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
CREATE TABLE IF NOT EXISTS credentials (
	auth_index TEXT PRIMARY KEY,
	provider TEXT NOT NULL DEFAULT '',
	auth_type TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL DEFAULT 0
);
`

const insertEventSQL = `INSERT INTO events (ts, request_id, provider, executor_type, model, alias, endpoint, auth_type,
	auth_index, source, api_key_id, api_key, access_provider, reasoning_effort, service_tier, stream, failed,
	status_code, error, latency_ms, ttft_ms, input_uncached, cache_read, cache_write, output, reasoning,
	unclassified, total, client_ip, user_agent)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// Credentials keep the latest display fields per auth index so labels survive event pruning.
const upsertCredentialSQL = `INSERT INTO credentials (auth_index, provider, auth_type, source, last_seen)
	VALUES (?,?,?,?,?)
	ON CONFLICT(auth_index) DO UPDATE SET provider = excluded.provider, auth_type = excluded.auth_type,
	source = excluded.source, last_seen = excluded.last_seen WHERE excluded.last_seen >= credentials.last_seen`

// backfillCredentialsSQL seeds the credentials table for databases created before it existed.
const backfillCredentialsSQL = `INSERT OR IGNORE INTO credentials (auth_index, provider, auth_type, source, last_seen)
	SELECT e.auth_index, e.provider, e.auth_type, e.source, e.ts FROM events e
	JOIN (SELECT MAX(id) AS id FROM events WHERE auth_index != '' GROUP BY auth_index) latest ON e.id = latest.id`

const upsertHourlySQL = `INSERT INTO hourly (hour, provider, model, api_key_id, auth_index, service_tier, api_key,
	requests, failed, input_uncached, cache_read, cache_write, output, reasoning, unclassified, total,
	latency_ms, ttft_ms, ttft_count)
	VALUES (?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(hour, provider, model, api_key_id, auth_index, service_tier) DO UPDATE SET
	api_key = excluded.api_key,
	requests = requests + 1,
	failed = failed + excluded.failed,
	input_uncached = input_uncached + excluded.input_uncached,
	cache_read = cache_read + excluded.cache_read,
	cache_write = cache_write + excluded.cache_write,
	output = output + excluded.output,
	reasoning = reasoning + excluded.reasoning,
	unclassified = unclassified + excluded.unclassified,
	total = total + excluded.total,
	latency_ms = latency_ms + excluded.latency_ms,
	ttft_ms = ttft_ms + excluded.ttft_ms,
	ttft_count = ttft_count + excluded.ttft_count`

// Tokens holds mutually exclusive token buckets (canonical v2 accounting).
type Tokens struct {
	InputUncached int64 `json:"input_uncached"`
	CacheRead     int64 `json:"cache_read"`
	CacheWrite    int64 `json:"cache_write"`
	// Output includes Reasoning.
	Output       int64 `json:"output"`
	Reasoning    int64 `json:"reasoning"`
	Unclassified int64 `json:"unclassified"`
	Total        int64 `json:"total"`
}

func (t *Tokens) add(o Tokens) {
	t.InputUncached += o.InputUncached
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.Unclassified += o.Unclassified
	t.Total += o.Total
}

// Event is one stored usage record.
type Event struct {
	ID              int64   `json:"id"`
	Timestamp       int64   `json:"timestamp"` // unix milliseconds
	RequestID       string  `json:"request_id"`
	Provider        string  `json:"provider"`
	ExecutorType    string  `json:"executor_type"`
	Model           string  `json:"model"`
	Alias           string  `json:"alias"`
	Endpoint        string  `json:"endpoint"`
	AuthType        string  `json:"auth_type"`
	AuthIndex       string  `json:"auth_index"`
	Source          string  `json:"source"`
	APIKeyID        string  `json:"api_key_id"`
	APIKey          string  `json:"api_key"` // masked
	AccessProvider  string  `json:"access_provider"`
	ReasoningEffort string  `json:"reasoning_effort"`
	ServiceTier     string  `json:"service_tier"`
	Stream          bool    `json:"stream"`
	Failed          bool    `json:"failed"`
	StatusCode      int     `json:"status_code"`
	Error           string  `json:"error"`
	LatencyMs       int64   `json:"latency_ms"`
	TTFTMs          int64   `json:"ttft_ms"`
	Tokens          Tokens  `json:"tokens"`
	ClientIP        string  `json:"client_ip"`
	UserAgent       string  `json:"user_agent"`
	Cost            float64 `json:"cost"`
	Priced          bool    `json:"priced"`
}

// Store owns the database and its single writer goroutine.
type Store struct {
	path      string
	db        *sql.DB
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	retention atomic.Int64 // days; <0 keeps forever
	dropped   atomic.Int64
	pricer    *Pricer
}

// Open creates or opens the database at path and starts the writer.
func Open(path string, retentionDays int, pricer *Pricer) (*Store, error) {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o755); errMkdir != nil {
		return nil, fmt.Errorf("usage store: create dir: %w", errMkdir)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("usage store: open: %w", err)
	}
	db.SetMaxOpenConns(4)
	if _, errSchema := db.Exec(schema); errSchema != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: schema: %w", errSchema)
	}
	s := &Store{
		path:   path,
		db:     db,
		ch:     make(chan Event, writeBuffer),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		pricer: pricer,
	}
	var credentialCount int
	if errCount := db.QueryRow(`SELECT COUNT(*) FROM credentials`).Scan(&credentialCount); errCount == nil && credentialCount == 0 {
		if _, errBackfill := db.Exec(backfillCredentialsSQL); errBackfill != nil {
			log.Warnf("usage store: backfill credentials: %v", errBackfill)
		}
	}
	s.SetRetentionDays(retentionDays)
	s.purge()
	go s.run()
	return s, nil
}

// SetRetentionDays updates event retention; 0 means the default of 30 days.
func (s *Store) SetRetentionDays(days int) {
	if days == 0 {
		days = 30
	}
	s.retention.Store(int64(days))
}

// Enqueue hands an event to the writer without blocking; it drops the event when the buffer is full.
func (s *Store) Enqueue(e Event) {
	select {
	case s.ch <- e:
	default:
		if s.dropped.Add(1)%1000 == 1 {
			log.Warnf("usage store: write buffer full, dropped %d events so far", s.dropped.Load())
		}
	}
}

// Close flushes buffered events and closes the database.
func (s *Store) Close() {
	close(s.stop)
	<-s.done
}

func (s *Store) run() {
	defer close(s.done)
	defer func() {
		if errClose := s.db.Close(); errClose != nil {
			log.Errorf("usage store: close: %v", errClose)
		}
	}()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	lastPurge := time.Now()
	batch := make([]Event, 0, writeBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if errWrite := s.write(batch); errWrite != nil {
			log.Errorf("usage store: write %d events: %v", len(batch), errWrite)
		}
		batch = batch[:0]
	}
	for {
		select {
		case e := <-s.ch:
			batch = append(batch, e)
			if len(batch) >= writeBatch {
				flush()
			}
		case <-ticker.C:
			flush()
			if time.Since(lastPurge) >= purgeInterval {
				s.purge()
				lastPurge = time.Now()
			}
		case <-s.stop:
			for {
				select {
				case e := <-s.ch:
					batch = append(batch, e)
					if len(batch) >= writeBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Store) write(batch []Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	insEvent, err := tx.Prepare(insertEventSQL)
	if err != nil {
		return err
	}
	defer func() { _ = insEvent.Close() }()
	upHourly, err := tx.Prepare(upsertHourlySQL)
	if err != nil {
		return err
	}
	defer func() { _ = upHourly.Close() }()
	latest := map[string]*Event{}
	for i := range batch {
		e := &batch[i]
		t := e.Tokens
		if _, errExec := insEvent.Exec(e.Timestamp, e.RequestID, e.Provider, e.ExecutorType, e.Model, e.Alias,
			e.Endpoint, e.AuthType, e.AuthIndex, e.Source, e.APIKeyID, e.APIKey, e.AccessProvider, e.ReasoningEffort,
			e.ServiceTier, e.Stream, e.Failed, e.StatusCode, e.Error, e.LatencyMs, e.TTFTMs, t.InputUncached,
			t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified, t.Total, e.ClientIP, e.UserAgent); errExec != nil {
			return errExec
		}
		ttftCount := 0
		if e.TTFTMs > 0 {
			ttftCount = 1
		}
		hour := e.Timestamp / 1000 / 3600 * 3600
		if _, errExec := upHourly.Exec(hour, e.Provider, e.Model, e.APIKeyID, e.AuthIndex, e.ServiceTier, e.APIKey,
			e.Failed, t.InputUncached, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified, t.Total,
			e.LatencyMs, e.TTFTMs, ttftCount); errExec != nil {
			return errExec
		}
		if e.AuthIndex != "" {
			if prev := latest[e.AuthIndex]; prev == nil || e.Timestamp >= prev.Timestamp {
				latest[e.AuthIndex] = e
			}
		}
	}
	for authIndex, e := range latest {
		if _, errExec := tx.Exec(upsertCredentialSQL, authIndex, e.Provider, e.AuthType, e.Source, e.Timestamp); errExec != nil {
			return errExec
		}
	}
	return tx.Commit()
}

func (s *Store) purge() {
	days := s.retention.Load()
	if days < 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	for {
		res, err := s.db.Exec(`DELETE FROM events WHERE id IN (SELECT id FROM events WHERE ts < ? LIMIT ?)`, cutoff, purgeChunk)
		if err != nil {
			log.Errorf("usage store: purge: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n < purgeChunk {
			return
		}
	}
}
