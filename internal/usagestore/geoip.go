package usagestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

const (
	defaultGeoIPEndpoint = "https://api.ip.sb/geoip/"
	// ip.sb's free API allows each caller 100 requests per minute and at most 5 per second.
	geoIPPerSecond    = 5
	geoIPPerMinute    = 100
	geoIPWorkers      = 5
	geoIPBatchSize    = 25
	geoIPRateLimitGap = time.Minute
	geoIPRoundEvery   = time.Minute
	geoIPRetryAfter   = 24 * time.Hour
	geoIPUserAgent    = "CLIProxyAPI-usage-store"
	geoIPMaxBodyBytes = 64 << 10
)

var (
	// ErrGeoIPDisabled is returned when a lookup round is requested while lookups are disabled.
	ErrGeoIPDisabled    = errors.New("geoip is disabled")
	errGeoIPRateLimited = errors.New("geoip lookup: HTTP 429, paused for a minute")
	cgnatPrefix         = netip.MustParsePrefix("100.64.0.0/10")
)

// ClientGeo holds IP geolocation and network owner information for an event.
type ClientGeo struct {
	Scope       string `json:"scope"`
	CountryCode string `json:"country_code,omitempty"`
	Country     string `json:"country,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	ASN         int64  `json:"asn,omitempty"`
	ISP         string `json:"isp,omitempty"`
	Org         string `json:"org,omitempty"`
}

// ip_geo.ok values.
const (
	ipGeoPending  = -1 // public address registered by the writer, not looked up yet
	ipGeoNotFound = 0  // looked up without a usable answer; retried after geoIPRetryAfter
	ipGeoFound    = 1
	ipGeoLocal    = 2 // private, loopback or unparsable; never sent
)

// GeoIPStatus reports lookup state for the management API.
type GeoIPStatus struct {
	Enabled      bool       `json:"enabled"`
	Provider     string     `json:"provider"`
	Resolved     int64      `json:"resolved"`
	Pending      int64      `json:"pending"`
	LastLookupAt *time.Time `json:"last_lookup_at"`
	LastError    string     `json:"last_error"`
}

// ipGeoSchema caches one lookup per distinct client IP; events join it at read time.
const ipGeoSchema = `
CREATE TABLE IF NOT EXISTS ip_geo (
	ip TEXT PRIMARY KEY,
	country_code TEXT NOT NULL DEFAULT '',
	country TEXT NOT NULL DEFAULT '',
	region TEXT NOT NULL DEFAULT '',
	city TEXT NOT NULL DEFAULT '',
	asn INTEGER NOT NULL DEFAULT 0,
	isp TEXT NOT NULL DEFAULT '',
	org TEXT NOT NULL DEFAULT '',
	ok INTEGER NOT NULL DEFAULT 0,
	looked_up_at INTEGER NOT NULL DEFAULT 0,
	seen_at INTEGER NOT NULL DEFAULT 0
);`

// ipGeoIndex serves the pending/due selection; it needs seen_at, so it runs after migrateSchema.
const ipGeoIndex = `CREATE INDEX IF NOT EXISTS ip_geo_ok ON ip_geo(ok, seen_at);`

// GeoIP looks up registered public client IPs through an online API, paced to the free limit,
// and stores the answers in the usage database. The writer registers every client IP in ip_geo
// with the event, so pending work lives in the database and survives errors, restarts, long
// requests and event id reuse after retention.
type GeoIP struct {
	db       *sql.DB
	endpoint string
	nowFunc  func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
	kick     chan struct{}
	// started, when set, observes every lookup start (tests check the pacing with it).
	started func(ip string, at time.Time)

	mu          sync.Mutex
	disabled    bool
	lastAt      *time.Time
	lastError   string
	pausedUntil time.Time

	running sync.Mutex
	// Guarded by running: the per-second limiter and the start times of the last
	// geoIPPerMinute lookups, oldest first, for the rolling one-minute cap.
	perSecond *rate.Limiter
	starts    []time.Time
}

// NewGeoIP returns a resolver that caches results in db.
func NewGeoIP(db *sql.DB) *GeoIP {
	return &GeoIP{
		db:        db,
		endpoint:  defaultGeoIPEndpoint,
		nowFunc:   time.Now,
		sleep:     sleepContext,
		kick:      make(chan struct{}, 1),
		perSecond: rate.NewLimiter(rate.Every(time.Second/geoIPPerSecond), 1),
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Configure applies the usage GeoIP configuration.
func (g *GeoIP) Configure(cfg config.UsageGeoIPConfig) {
	g.mu.Lock()
	g.disabled = cfg.Disable
	g.mu.Unlock()
	if !cfg.Disable {
		g.Kick()
	}
}

// Kick asks the background loop to run a round soon.
func (g *GeoIP) Kick() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *GeoIP) isDisabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.disabled
}

// ipScope classifies an address; only "public" addresses are ever sent to the lookup API.
func ipScope(raw string) (netip.Addr, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, ""
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, ""
	}
	addr = addr.Unmap()
	switch {
	case addr.IsLoopback():
		return addr, "loopback"
	case addr.IsPrivate(), addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(), addr.IsUnspecified(),
		addr.IsMulticast(), addr.IsInterfaceLocalMulticast(), cgnatPrefix.Contains(addr):
		return addr, "private"
	}
	return addr, "public"
}

// clientGeo builds the event field from the client IP and the cached lookup, if any.
func clientGeo(clientIP string, row *ipGeoRow) *ClientGeo {
	_, scope := ipScope(clientIP)
	if scope == "" {
		return nil
	}
	geo := &ClientGeo{Scope: scope}
	if scope == "public" && row != nil && row.OK {
		geo.CountryCode, geo.Country, geo.Region, geo.City = row.CountryCode, row.Country, row.Region, row.City
		geo.ASN, geo.ISP, geo.Org = row.ASN, row.ISP, row.Org
	}
	return geo
}

type ipGeoRow struct {
	CountryCode, Country, Region, City, ISP, Org string
	ASN                                          int64
	OK                                           bool
}

// Status reports lookup progress without changing any lookup state.
func (g *GeoIP) Status(ctx context.Context) GeoIPStatus {
	st := GeoIPStatus{Provider: "ip.sb"}
	g.mu.Lock()
	st.Enabled = !g.disabled
	st.LastLookupAt, st.LastError = g.lastAt, g.lastError
	g.mu.Unlock()
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ip_geo WHERE ok = ?`, ipGeoFound).Scan(&st.Resolved); err != nil {
		log.Debugf("usage store: count resolved ips: %v", err)
	}
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ip_geo WHERE ok = ? OR (ok = ? AND looked_up_at < ?)`,
		ipGeoPending, ipGeoNotFound, g.retryBefore()).Scan(&st.Pending); err != nil {
		log.Debugf("usage store: count pending ips: %v", err)
	}
	return st
}

func (g *GeoIP) retryBefore() int64 {
	return g.nowFunc().Add(-geoIPRetryAfter).UnixMilli()
}

// candidates lists up to limit addresses to look up: never looked up first (newest first),
// then failed lookups that are due for a retry.
func (g *GeoIP) candidates(ctx context.Context, limit int) ([]string, error) {
	rows, err := g.db.QueryContext(ctx, `SELECT ip FROM ip_geo
		WHERE ok = ? OR (ok = ? AND looked_up_at < ?)
		ORDER BY ok, seen_at DESC LIMIT ?`, ipGeoPending, ipGeoNotFound, g.retryBefore(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ip string
		if errScan := rows.Scan(&ip); errScan != nil {
			return nil, errScan
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}

// ipGeoState is the ip_geo.ok value a newly seen client IP is registered with.
func ipGeoState(ip string) int {
	if _, scope := ipScope(ip); scope == "public" {
		return ipGeoPending
	}
	return ipGeoLocal
}

// registerClientIPs adds every new client IP of a batch to ip_geo inside the writer transaction:
// public ones as pending, everything else as local, so local addresses are never sent. It
// reports whether a new public address was registered.
func registerClientIPs(tx *sql.Tx, batch []Event) (bool, error) {
	seen := map[string]int64{}
	for i := range batch {
		ip := batch[i].ClientIP
		if ip == "" {
			continue
		}
		if ts, ok := seen[ip]; !ok || batch[i].Timestamp > ts {
			seen[ip] = batch[i].Timestamp
		}
	}
	if len(seen) == 0 {
		return false, nil
	}
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO ip_geo (ip, ok, seen_at) VALUES (?, ?, ?)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = ins.Close() }()
	newPublic := false
	for ip, ts := range seen {
		state := ipGeoState(ip)
		res, errExec := ins.Exec(ip, state, ts)
		if errExec != nil {
			return false, errExec
		}
		if n, _ := res.RowsAffected(); n > 0 && state == ipGeoPending {
			newPublic = true
		}
	}
	return newPublic, nil
}

// backfillIPGeo registers client IPs of events stored before the writer registered them.
func backfillIPGeo(db *sql.DB) error {
	rows, err := db.Query(`SELECT client_ip, MAX(ts) FROM events
		WHERE client_ip <> '' AND NOT EXISTS (SELECT 1 FROM ip_geo g WHERE g.ip = events.client_ip)
		GROUP BY client_ip`)
	if err != nil {
		return err
	}
	var batch []Event
	for rows.Next() {
		var e Event
		if errScan := rows.Scan(&e.ClientIP, &e.Timestamp); errScan != nil {
			_ = rows.Close()
			return errScan
		}
		batch = append(batch, e)
	}
	if errRows := rows.Err(); errRows != nil {
		_ = rows.Close()
		return errRows
	}
	_ = rows.Close()
	if len(batch) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = registerClientIPs(tx, batch); err != nil {
		return err
	}
	return tx.Commit()
}

type ipSBResponse struct {
	CountryCode string `json:"country_code"`
	Country     string `json:"country"`
	Region      string `json:"region"`
	City        string `json:"city"`
	ASN         int64  `json:"asn"`
	ISP         string `json:"isp"`
	Org         string `json:"organization"`
}

// lookup asks the API about one address. A 4xx answer is stored as not found; network errors,
// 5xx and 429 are returned so the address stays pending and is tried again later.
func (g *GeoIP) lookup(ctx context.Context, client *http.Client, addr netip.Addr) (*ipGeoRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.endpoint+url.PathEscape(addr.String()), nil)
	if err != nil {
		return nil, err
	}
	// The API rejects the default library user agents.
	req.Header.Set("User-Agent", geoIPUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("usage store: close geoip response: %v", errClose)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, geoIPMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read geoip response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errGeoIPRateLimited
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("geoip lookup: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return &ipGeoRow{}, nil
	}
	var r ipSBResponse
	if errDecode := json.Unmarshal(body, &r); errDecode != nil {
		return nil, fmt.Errorf("decode geoip response: %w", errDecode)
	}
	if strings.TrimSpace(r.CountryCode) == "" && r.ASN == 0 {
		// An answer without location or network is kept as not found so it is retried later.
		return &ipGeoRow{}, nil
	}
	return &ipGeoRow{
		CountryCode: strings.TrimSpace(r.CountryCode), Country: strings.TrimSpace(r.Country),
		Region: strings.TrimSpace(r.Region), City: strings.TrimSpace(r.City),
		ASN: r.ASN, ISP: strings.TrimSpace(r.ISP), Org: strings.TrimSpace(r.Org), OK: true,
	}, nil
}

func (g *GeoIP) save(ctx context.Context, ip string, row *ipGeoRow, ok int) error {
	_, err := g.db.ExecContext(ctx, `INSERT INTO ip_geo (ip, country_code, country, region, city, asn, isp, org, ok, looked_up_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET country_code = excluded.country_code, country = excluded.country,
			region = excluded.region, city = excluded.city, asn = excluded.asn, isp = excluded.isp,
			org = excluded.org, ok = excluded.ok, looked_up_at = excluded.looked_up_at`,
		ip, row.CountryCode, row.Country, row.Region, row.City, row.ASN, row.ISP, row.Org, ok,
		g.nowFunc().UnixMilli())
	return err
}

// waitTurn blocks until another lookup may start: not during a rate-limit pause, at most
// geoIPPerSecond per second, and at most geoIPPerMinute in any rolling minute. The caller holds
// g.running. It returns the start time.
func (g *GeoIP) waitTurn(ctx context.Context) (time.Time, error) {
	for {
		now := g.nowFunc()
		g.mu.Lock()
		wait := g.pausedUntil.Sub(now)
		g.mu.Unlock()
		if len(g.starts) >= geoIPPerMinute {
			if d := g.starts[0].Add(time.Minute).Sub(now); d > wait {
				wait = d
			}
		}
		if wait <= 0 {
			r := g.perSecond.ReserveN(now, 1)
			if wait = r.DelayFrom(now); wait <= 0 {
				if len(g.starts) >= geoIPPerMinute {
					g.starts = g.starts[1:]
				}
				g.starts = append(g.starts, now)
				return now, nil
			}
			// Re-check the pause after waiting instead of holding this token.
			r.CancelAt(now)
		}
		if err := g.sleep(ctx, wait); err != nil {
			return time.Time{}, err
		}
	}
}

// Resolve looks up pending addresses until none are left, up to geoIPWorkers at a time and
// paced by waitTurn. A 429 pauses new lookups for a minute and ends the call; network errors
// and 5xx end it too. ctx cancels in-flight lookups; unfinished addresses stay pending.
func (g *GeoIP) Resolve(ctx context.Context, client *http.Client) (int, error) {
	g.running.Lock()
	defer g.running.Unlock()
	if g.isDisabled() {
		return 0, ErrGeoIPDisabled
	}
	if client == nil {
		client = &http.Client{}
	}
	var (
		resMu    sync.Mutex
		done     int
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		resMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		resMu.Unlock()
	}
	failed := func() bool {
		resMu.Lock()
		defer resMu.Unlock()
		return firstErr != nil
	}
	slots := make(chan struct{}, geoIPWorkers)
	for !failed() && !g.isDisabled() {
		// Each batch finishes before the next is selected, so no address is picked twice.
		ips, err := g.candidates(ctx, geoIPBatchSize)
		if err != nil {
			fail(fmt.Errorf("list pending client ips: %w", err))
			break
		}
		if len(ips) == 0 {
			break
		}
		for _, ip := range ips {
			if failed() || g.isDisabled() {
				break
			}
			addr, scope := ipScope(ip)
			if scope != "public" {
				if errSave := g.save(ctx, ip, &ipGeoRow{}, ipGeoLocal); errSave != nil {
					fail(fmt.Errorf("mark local client ip: %w", errSave))
				}
				continue
			}
			// Take a worker slot before pacing, so the pace applies to the moment the request is
			// actually sent and a pause or failure that happened while waiting is seen.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				fail(ctx.Err())
			}
			if ctx.Err() != nil {
				break
			}
			if failed() {
				<-slots
				break
			}
			at, errWait := g.waitTurn(ctx)
			if errWait != nil {
				<-slots
				fail(errWait)
				break
			}
			if failed() || g.isDisabled() {
				<-slots
				break
			}
			if g.started != nil {
				g.started(ip, at)
			}
			wg.Add(1)
			go func(ip string, addr netip.Addr) {
				defer wg.Done()
				defer func() { <-slots }()
				row, errLookup := g.lookup(ctx, client, addr)
				if errLookup != nil {
					// A 429, a network error or a 5xx pauses new lookups for a minute. The pause
					// outlives this call, so a pass started by a new address waits it out too.
					// fail first: a dispatcher that sees the pause then also sees the failure.
					fail(errLookup)
					if ctx.Err() == nil {
						g.mu.Lock()
						g.pausedUntil = g.nowFunc().Add(geoIPRateLimitGap)
						g.mu.Unlock()
					}
					return
				}
				ok := ipGeoNotFound
				if row.OK {
					ok = ipGeoFound
				}
				if errSave := g.save(ctx, ip, row, ok); errSave != nil {
					fail(fmt.Errorf("save geoip result: %w", errSave))
					return
				}
				resMu.Lock()
				done++
				resMu.Unlock()
			}(ip, addr)
		}
		wg.Wait()
	}
	wg.Wait()
	now := g.nowFunc().UTC()
	g.mu.Lock()
	if done > 0 {
		g.lastAt = &now
	}
	if ctx.Err() == nil {
		g.lastError = ""
		if firstErr != nil {
			g.lastError = firstErr.Error()
		}
	}
	g.mu.Unlock()
	return done, firstErr
}

// prune drops cached answers for addresses no stored event refers to any more.
func (g *GeoIP) prune(ctx context.Context) {
	if _, err := g.db.ExecContext(ctx, `DELETE FROM ip_geo WHERE ip NOT IN (SELECT DISTINCT client_ip FROM events)`); err != nil {
		log.Warnf("usage store: prune ip_geo: %v", err)
	}
}
