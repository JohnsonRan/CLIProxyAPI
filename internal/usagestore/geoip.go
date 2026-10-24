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
)

const (
	defaultGeoIPEndpoint = "https://api.ip.sb/geoip/"
	// ip.sb allows 100 requests per minute (5 per second) per caller; stay well below it.
	geoIPLookupGap    = 700 * time.Millisecond
	geoIPRoundSize    = 60
	geoIPRoundEvery   = time.Minute
	geoIPRetryAfter   = 24 * time.Hour
	geoIPUserAgent    = "CLIProxyAPI-usage-store"
	geoIPMaxBodyBytes = 64 << 10
	geoIPQueueLimit   = 10000
)

var (
	// ErrGeoIPDisabled is returned when a lookup round is requested while lookups are disabled.
	ErrGeoIPDisabled = errors.New("geoip is disabled")
	cgnatPrefix      = netip.MustParsePrefix("100.64.0.0/10")
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
	looked_up_at INTEGER NOT NULL DEFAULT 0
);`

// GeoIP resolves new public client IPs through an online API in small, rate-limited rounds
// and stores the answers in the usage database.
type GeoIP struct {
	db       *sql.DB
	endpoint string
	gap      time.Duration
	nowFunc  func() time.Time
	kick     chan struct{}

	mu        sync.Mutex
	disabled  bool
	lastAt    *time.Time
	lastError string
	// queue holds public client IPs awaiting a lookup; scanned is the highest event id already
	// examined, so each refill reads only new events instead of the whole table.
	queue   []string
	queued  map[string]struct{}
	scanned int64
	running sync.Mutex
}

// NewGeoIP returns a resolver that caches results in db.
func NewGeoIP(db *sql.DB) *GeoIP {
	return &GeoIP{
		db:       db,
		endpoint: defaultGeoIPEndpoint,
		gap:      geoIPLookupGap,
		nowFunc:  time.Now,
		kick:     make(chan struct{}, 1),
		queued:   map[string]struct{}{},
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

// Status reports lookup progress.
func (g *GeoIP) Status(ctx context.Context) GeoIPStatus {
	if err := g.refill(ctx); err != nil {
		log.Debugf("usage store: refill geoip queue: %v", err)
	}
	st := GeoIPStatus{Enabled: !g.isDisabled(), Provider: "ip.sb"}
	g.mu.Lock()
	st.LastLookupAt, st.LastError, st.Pending = g.lastAt, g.lastError, int64(len(g.queue))
	g.mu.Unlock()
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ip_geo WHERE ok = 1`).Scan(&st.Resolved); err != nil {
		log.Debugf("usage store: count resolved ips: %v", err)
	}
	return st
}

func (g *GeoIP) enqueue(ip string) {
	if _, scope := ipScope(ip); scope != "public" {
		return
	}
	// shortcut: an address past the queue limit is skipped until a restart rescans the table; raise the limit or persist the backlog if that bites.
	if _, ok := g.queued[ip]; ok || len(g.queue) >= geoIPQueueLimit {
		return
	}
	g.queued[ip] = struct{}{}
	g.queue = append(g.queue, ip)
}

// refill queues client IPs of events written since the last scan that have no cached answer,
// plus failed lookups that are due for a retry.
func (g *GeoIP) refill(ctx context.Context) error {
	g.mu.Lock()
	from := g.scanned
	g.mu.Unlock()
	var hi sql.NullInt64
	if err := g.db.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&hi); err != nil {
		return err
	}
	var fresh []string
	if hi.Valid && hi.Int64 > from {
		// Newest first: the addresses on screen get their location first.
		rows, err := g.db.QueryContext(ctx, `SELECT e.client_ip FROM events e
			WHERE e.id > ? AND e.id <= ? AND e.client_ip <> ''
			AND NOT EXISTS (SELECT 1 FROM ip_geo g WHERE g.ip = e.client_ip)
			GROUP BY e.client_ip ORDER BY MAX(e.id) DESC`, from, hi.Int64)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ip string
			if errScan := rows.Scan(&ip); errScan != nil {
				_ = rows.Close()
				return errScan
			}
			fresh = append(fresh, ip)
		}
		if errRows := rows.Err(); errRows != nil {
			_ = rows.Close()
			return errRows
		}
		_ = rows.Close()
	}
	retryBefore := g.nowFunc().Add(-geoIPRetryAfter).UnixMilli()
	rows, err := g.db.QueryContext(ctx, `SELECT ip FROM ip_geo WHERE ok = 0 AND looked_up_at < ?`, retryBefore)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ip string
		if errScan := rows.Scan(&ip); errScan != nil {
			return errScan
		}
		fresh = append(fresh, ip)
	}
	if errRows := rows.Err(); errRows != nil {
		return errRows
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ip := range fresh {
		g.enqueue(ip)
	}
	if hi.Valid && hi.Int64 > g.scanned {
		g.scanned = hi.Int64
	}
	return nil
}

// take removes up to n addresses from the head of the queue.
func (g *GeoIP) take(n int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n > len(g.queue) {
		n = len(g.queue)
	}
	out := append([]string(nil), g.queue[:n]...)
	g.queue = g.queue[n:]
	for _, ip := range out {
		delete(g.queued, ip)
	}
	return out
}

// requeue puts addresses that were not looked up back at the head of the queue.
func (g *GeoIP) requeue(ips []string) {
	if len(ips) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rest := g.queue
	g.queue = nil
	for _, ip := range append(append([]string(nil), ips...), rest...) {
		g.enqueue(ip)
	}
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

// lookup asks the API about one address. A 4xx answer is final (stored as not found); network
// errors and 5xx/429 are returned so the round stops and the address is retried later.
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
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fmt.Errorf("geoip lookup: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return &ipGeoRow{}, nil
	}
	var r ipSBResponse
	if errDecode := json.Unmarshal(body, &r); errDecode != nil {
		return nil, fmt.Errorf("decode geoip response: %w", errDecode)
	}
	return &ipGeoRow{
		CountryCode: strings.TrimSpace(r.CountryCode), Country: strings.TrimSpace(r.Country),
		Region: strings.TrimSpace(r.Region), City: strings.TrimSpace(r.City),
		ASN: r.ASN, ISP: strings.TrimSpace(r.ISP), Org: strings.TrimSpace(r.Org), OK: true,
	}, nil
}

func (g *GeoIP) save(ctx context.Context, ip string, row *ipGeoRow) error {
	ok := 0
	if row.OK {
		ok = 1
	}
	_, err := g.db.ExecContext(ctx, `INSERT INTO ip_geo (ip, country_code, country, region, city, asn, isp, org, ok, looked_up_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET country_code = excluded.country_code, country = excluded.country,
			region = excluded.region, city = excluded.city, asn = excluded.asn, isp = excluded.isp,
			org = excluded.org, ok = excluded.ok, looked_up_at = excluded.looked_up_at`,
		ip, row.CountryCode, row.Country, row.Region, row.City, row.ASN, row.ISP, row.Org, ok,
		g.nowFunc().UnixMilli())
	return err
}

// Resolve looks up at most one round of pending addresses, paced below the API rate limit.
func (g *GeoIP) Resolve(ctx context.Context, client *http.Client) (int, error) {
	if g.isDisabled() {
		return 0, ErrGeoIPDisabled
	}
	// One round at a time; a manual refresh while the loop runs just waits for it.
	g.running.Lock()
	defer g.running.Unlock()
	if client == nil {
		client = &http.Client{}
	}
	if err := g.refill(ctx); err != nil {
		return 0, fmt.Errorf("list new client ips: %w", err)
	}
	ips := g.take(geoIPRoundSize)
	done := 0
	var lookupErr error
	for i, ip := range ips {
		if g.isDisabled() {
			g.requeue(ips[i:])
			break
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				lookupErr = ctx.Err()
			case <-time.After(g.gap):
			}
			if lookupErr != nil {
				g.requeue(ips[i:])
				break
			}
		}
		addr, _ := ipScope(ip)
		row, errLookup := g.lookup(ctx, client, addr)
		if errLookup != nil {
			lookupErr = errLookup
			g.requeue(ips[i:])
			break
		}
		if errSave := g.save(ctx, ip, row); errSave != nil {
			lookupErr = fmt.Errorf("save geoip result: %w", errSave)
			g.requeue(ips[i:])
			break
		}
		done++
	}
	now := g.nowFunc().UTC()
	g.mu.Lock()
	if done > 0 {
		g.lastAt = &now
	}
	if lookupErr != nil {
		g.lastError = lookupErr.Error()
	} else {
		g.lastError = ""
	}
	g.mu.Unlock()
	return done, lookupErr
}

// prune drops cached answers for addresses no stored event refers to any more.
func (g *GeoIP) prune(ctx context.Context) {
	if _, err := g.db.ExecContext(ctx, `DELETE FROM ip_geo WHERE ip NOT IN (SELECT DISTINCT client_ip FROM events)`); err != nil {
		log.Warnf("usage store: prune ip_geo: %v", err)
	}
}
