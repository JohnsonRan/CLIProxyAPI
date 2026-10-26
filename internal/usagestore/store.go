// Package usagestore persists usage records to a local SQLite database and
// serves aggregated statistics and cost estimates to the management API.
package usagestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	user_agent TEXT NOT NULL DEFAULT '',
	response_model TEXT NOT NULL DEFAULT '',
	substituted INTEGER NOT NULL DEFAULT 0,
	transport TEXT NOT NULL DEFAULT '',
	risk TEXT NOT NULL DEFAULT '',
	risk_prompt TEXT NOT NULL DEFAULT '',
	risk_reason TEXT NOT NULL DEFAULT ''
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
	substituted INTEGER NOT NULL DEFAULT 0,
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
	gen_output INTEGER NOT NULL DEFAULT 0,
	gen_ms INTEGER NOT NULL DEFAULT 0,
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
	unclassified, total, client_ip, user_agent, response_model, substituted, transport, risk, risk_prompt, risk_reason)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

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
	requests, failed, substituted, input_uncached, cache_read, cache_write, output, reasoning, unclassified, total,
	latency_ms, ttft_ms, ttft_count, gen_output, gen_ms)
	VALUES (?,?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(hour, provider, model, api_key_id, auth_index, service_tier) DO UPDATE SET
	api_key = excluded.api_key,
	requests = requests + 1,
	failed = failed + excluded.failed,
	substituted = substituted + excluded.substituted,
	input_uncached = input_uncached + excluded.input_uncached,
	cache_read = cache_read + excluded.cache_read,
	cache_write = cache_write + excluded.cache_write,
	output = output + excluded.output,
	reasoning = reasoning + excluded.reasoning,
	unclassified = unclassified + excluded.unclassified,
	total = total + excluded.total,
	latency_ms = latency_ms + excluded.latency_ms,
	ttft_ms = ttft_ms + excluded.ttft_ms,
	ttft_count = ttft_count + excluded.ttft_count,
	gen_output = gen_output + excluded.gen_output,
	gen_ms = gen_ms + excluded.gen_ms`

// minGenerationMs drops decode windows too short to time: upstreams that buffer the answer
// deliver it in one burst right after the first token, which would read as thousands of t/s.
const minGenerationMs = 100

// generationCond selects events with a measurable decode phase; it mirrors Event.generation.
var generationCond = fmt.Sprintf("stream = 1 AND failed = 0 AND ttft_ms > 0 AND output > 0 AND latency_ms - ttft_ms >= %d", minGenerationMs)

// genOutputSQL and genMsSQL are the output tokens and decode milliseconds of an events row (0 when
// it has no measurable decode phase).
var (
	genOutputSQL = "(CASE WHEN " + generationCond + " THEN output ELSE 0 END)"
	genMsSQL     = "(CASE WHEN " + generationCond + " THEN latency_ms - ttft_ms ELSE 0 END)"
)

// generation returns the output tokens and decode time (latency after the first token) of a
// successful streamed request, or zeros when it has none: non-streamed responses arrive whole,
// so their first byte does not mark the start of generation.
func (e *Event) generation() (output, ms int64) {
	ms = e.LatencyMs - e.TTFTMs
	if !e.Stream || e.Failed || e.TTFTMs <= 0 || e.Tokens.Output <= 0 || ms < minGenerationMs {
		return 0, 0
	}
	return e.Tokens.Output, ms
}

// tokensPerSecond returns output tokens per second of decode time, 0 when ms is 0.
func tokensPerSecond(output, ms int64) float64 {
	if ms <= 0 {
		return 0
	}
	return float64(output) * 1000 / float64(ms)
}

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
	ResponseModel   string  `json:"response_model"`
	Substituted     bool    `json:"substituted"`
	// OutputTPS is output tokens per second after the first token; 0 when not measurable.
	OutputTPS float64 `json:"output_tps"`
	// Transport is the downstream transport: "ws", "sse" or "http" ("" for rows recorded before it existed).
	Transport string `json:"transport"`
	// Risk names the upstream safety action on this request ("" when none): cyber_policy, bio_policy,
	// invalid_prompt, refusal_<category>, trusted_access_for_cyber, model_reroute, safety_buffering,
	// prompt_blocked_<reason> or output_blocked_<reason>.
	Risk string `json:"risk"`
	// RiskPrompt is the flagged turn as plain text, kept only for risk events.
	RiskPrompt string `json:"risk_prompt,omitempty"`
	// RiskReason is the explanation the upstream gave (rejection message, Claude stop_details), if any.
	RiskReason string `json:"risk_reason,omitempty"`
	// ClientGeo holds IP geolocation and ISP information. Omitted when client_ip is empty or unresolvable.
	ClientGeo *ClientGeo `json:"client_geo,omitempty"`
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
	geo       *GeoIP
	// ctx ends with the store; background lookups use it so Close stops them.
	ctx      context.Context
	cancel   context.CancelFunc
	lookups  sync.WaitGroup
	resolver sync.Once
	// quotaActive holds auth indexes that served requests since their last active quota probe.
	quotaActive sync.Map
	// maint carries optimize requests to the writer goroutine (keep days, 0 = retention only).
	maint    chan int
	optMu    sync.Mutex
	optState OptimizeStatus
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
	if _, errGeo := db.Exec(ipGeoSchema); errGeo != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: ip_geo schema: %w", errGeo)
	}
	if errMigrate := migrateSchema(db); errMigrate != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: migrate: %w", errMigrate)
	}
	if _, errIndex := db.Exec(ipGeoIndex); errIndex != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: ip_geo index: %w", errIndex)
	}
	if _, errQuota := db.Exec(quotaSchema); errQuota != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage store: quota schema: %w", errQuota)
	}
	if errBackfill := backfillIPGeo(db); errBackfill != nil {
		log.Warnf("usage store: register client ips: %v", errBackfill)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		ctx:    ctx,
		cancel: cancel,
		path:   path,
		db:     db,
		ch:     make(chan Event, writeBuffer),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		maint:  make(chan int, 1),
		pricer: pricer,
		geo:    NewGeoIP(db),
	}
	var credentialCount int
	if errCount := db.QueryRow(`SELECT COUNT(*) FROM credentials`).Scan(&credentialCount); errCount == nil && credentialCount == 0 {
		if _, errBackfill := db.Exec(backfillCredentialsSQL); errBackfill != nil {
			log.Warnf("usage store: backfill credentials: %v", errBackfill)
		}
	}
	s.SetRetentionDays(retentionDays)
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

func migrateSchema(db *sql.DB) error {
	hasColumn := func(table, column string) (bool, error) {
		rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt any
			if errScan := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); errScan != nil {
				return false, errScan
			}
			if strings.EqualFold(name, column) {
				return true, nil
			}
		}
		return false, rows.Err()
	}

	// Columns added after the first release; ALTER TABLE keeps existing rows with the default.
	for _, col := range []struct{ table, name, ddl string }{
		{"events", "response_model", "TEXT NOT NULL DEFAULT ''"},
		{"events", "substituted", "INTEGER NOT NULL DEFAULT 0"},
		{"events", "transport", "TEXT NOT NULL DEFAULT ''"},
		{"events", "risk", "TEXT NOT NULL DEFAULT ''"},
		{"events", "risk_prompt", "TEXT NOT NULL DEFAULT ''"},
		{"events", "risk_reason", "TEXT NOT NULL DEFAULT ''"},
		{"hourly", "substituted", "INTEGER NOT NULL DEFAULT 0"},
		{"hourly", "gen_output", "INTEGER NOT NULL DEFAULT 0"},
		{"hourly", "gen_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"ip_geo", "seen_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := hasColumn(col.table, col.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, errExec := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.table, col.name, col.ddl)); errExec != nil {
			return fmt.Errorf("migrate %s.%s: %w", col.table, col.name, errExec)
		}
	}
	return nil
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

// Close stops background lookups, then flushes buffered events and closes the database.
func (s *Store) Close() {
	s.cancel()
	s.lookups.Wait()
	close(s.stop)
	<-s.done
}

// GeoIPStatus reports client IP lookup progress.
func (s *Store) GeoIPStatus(ctx context.Context) GeoIPStatus {
	if s == nil || s.geo == nil {
		return GeoIPStatus{}
	}
	return s.geo.Status(ctx)
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
	// The first purge runs here, not in Open, so a long backlog after downtime does not delay startup.
	s.purge()
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
		case keepDays := <-s.maint:
			// Runs here so new events wait in the channel instead of failing on the vacuum's lock.
			flush()
			s.optimize(keepDays)
			lastPurge = time.Now()
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
	newPublicIP, err := registerClientIPs(tx, batch)
	if err != nil {
		return fmt.Errorf("register client ips: %w", err)
	}
	latest := map[string]*Event{}
	for i := range batch {
		e := &batch[i]
		t := e.Tokens
		subInt := 0
		if e.Substituted {
			subInt = 1
		}
		if _, errExec := insEvent.Exec(e.Timestamp, e.RequestID, e.Provider, e.ExecutorType, e.Model, e.Alias,
			e.Endpoint, e.AuthType, e.AuthIndex, e.Source, e.APIKeyID, e.APIKey, e.AccessProvider, e.ReasoningEffort,
			e.ServiceTier, e.Stream, e.Failed, e.StatusCode, e.Error, e.LatencyMs, e.TTFTMs, t.InputUncached,
			t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified, t.Total, e.ClientIP, e.UserAgent,
			e.ResponseModel, subInt, e.Transport, e.Risk, e.RiskPrompt, e.RiskReason); errExec != nil {
			return errExec
		}
		s.markQuotaActive(e.Provider, e.AuthIndex)
		ttftCount := 0
		if e.TTFTMs > 0 {
			ttftCount = 1
		}
		hour := e.Timestamp / 1000 / 3600 * 3600
		genOutput, genMs := e.generation()
		if _, errExec := upHourly.Exec(hour, e.Provider, e.Model, e.APIKeyID, e.AuthIndex, e.ServiceTier, e.APIKey,
			e.Failed, subInt, t.InputUncached, t.CacheRead, t.CacheWrite, t.Output, t.Reasoning, t.Unclassified, t.Total,
			e.LatencyMs, e.TTFTMs, ttftCount, genOutput, genMs); errExec != nil {
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
	if errCommit := tx.Commit(); errCommit != nil {
		return errCommit
	}
	if newPublicIP && s.geo != nil {
		s.geo.Kick()
	}
	return nil
}

func (s *Store) purge() {
	days := s.retention.Load()
	if days < 0 {
		return
	}
	if _, err := s.purgeBefore(time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()); err != nil {
		log.Errorf("usage store: purge: %v", err)
	}
}

// purgeBefore deletes events (and quota samples) older than cutoff ms and returns the events deleted.
func (s *Store) purgeBefore(cutoff int64) (int64, error) {
	var deleted int64
	for {
		res, err := s.db.Exec(`DELETE FROM events WHERE id IN (SELECT id FROM events WHERE ts < ? LIMIT ?)`, cutoff, purgeChunk)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
		if n < purgeChunk {
			// Quota samples are only useful next to the events they are divided by.
			if _, errQuota := s.db.Exec(`DELETE FROM quota_samples WHERE observed_at < ?`, cutoff); errQuota != nil {
				log.Errorf("usage store: purge quota samples: %v", errQuota)
			}
			if s.geo != nil {
				s.geo.prune(context.Background())
			}
			return deleted, nil
		}
	}
}
