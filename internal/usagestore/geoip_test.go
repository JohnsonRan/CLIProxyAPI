package usagestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// fakeClock drives the resolver's pacing without real waits: sleeping advances it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) add(d time.Duration) { c.set(c.now().Add(d)) }

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.add(d)
	return nil
}

// fakeIPSB answers like api.ip.sb/geoip/{ip} and records which addresses were asked.
type fakeIPSB struct {
	mu     sync.Mutex
	asked  []string
	status map[string]int
	agents []string
	body   string // raw 200 body instead of the default answer
}

func (f *fakeIPSB) handler(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimPrefix(r.URL.Path, "/geoip/")
	f.mu.Lock()
	f.asked = append(f.asked, ip)
	f.agents = append(f.agents, r.UserAgent())
	code, body := f.status[ip], f.body
	f.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ip": ip, "country_code": "JP", "country": "Japan", "region": "Tokyo", "city": "Chiyoda",
		"asn": 4713, "isp": "NTT Communications Corporation", "organization": "Open Computer Network",
		"asn_organization": "NTT DOCOMO BUSINESS,Inc.",
	})
}

func (f *fakeIPSB) askedIPs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func openGeoTestStore(t *testing.T, fake *fakeIPSB) *Store {
	t.Helper()
	s, _ := openGeoTestStoreClock(t, fake)
	return s
}

func openGeoTestStoreClock(t *testing.T, fake *fakeIPSB) (*Store, *fakeClock) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	clock := &fakeClock{t: time.Now()}
	s.geo.endpoint = server.URL + "/geoip/"
	s.geo.nowFunc, s.geo.sleep = clock.now, clock.sleep
	return s, clock
}

func writeIPs(t *testing.T, s *Store, ips ...string) {
	t.Helper()
	batch := make([]Event, 0, len(ips))
	for i, ip := range ips {
		batch = append(batch, Event{Timestamp: time.Now().UnixMilli() + int64(i), Model: "m", Provider: "p", ClientIP: ip})
	}
	if err := s.write(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func eventGeo(t *testing.T, s *Store) map[string]*ClientGeo {
	t.Helper()
	events, err := s.Events(context.Background(), EventFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	out := map[string]*ClientGeo{}
	for _, e := range events {
		out[e.ClientIP] = e.ClientGeo
	}
	return out
}

func TestGeoIPLooksUpEachPublicIPOnceAndJoinsEvents(t *testing.T) {
	fake := &fakeIPSB{}
	s := openGeoTestStore(t, fake)
	writeIPs(t, s, "153.156.0.1", "153.156.0.1", "10.0.0.5", "127.0.0.1", "100.64.1.2", "fd00::1", "")

	n, err := s.geo.Resolve(context.Background(), http.DefaultClient)
	if err != nil || n != 1 {
		t.Fatalf("Resolve = %d, %v; want 1 lookup", n, err)
	}
	if asked := fake.askedIPs(); len(asked) != 1 || asked[0] != "153.156.0.1" {
		t.Fatalf("only the public address may leave the host, asked %v", asked)
	}
	if fake.agents[0] != geoIPUserAgent {
		t.Fatalf("user agent %q", fake.agents[0])
	}
	// A second round has nothing new to ask.
	if n, err = s.geo.Resolve(context.Background(), http.DefaultClient); err != nil || n != 0 {
		t.Fatalf("second Resolve = %d, %v", n, err)
	}

	geo := eventGeo(t, s)
	pub := geo["153.156.0.1"]
	if pub == nil || pub.Scope != "public" || pub.CountryCode != "JP" || pub.City != "Chiyoda" ||
		pub.ASN != 4713 || pub.ISP != "NTT Communications Corporation" || pub.Org != "Open Computer Network" {
		t.Fatalf("public geo = %+v", pub)
	}
	for ip, scope := range map[string]string{"10.0.0.5": "private", "127.0.0.1": "loopback", "100.64.1.2": "private", "fd00::1": "private"} {
		if g := geo[ip]; g == nil || g.Scope != scope || g.Country != "" {
			t.Fatalf("%s geo = %+v, want scope %s only", ip, g, scope)
		}
	}
	if g := geo[""]; g != nil {
		t.Fatalf("empty client ip must have no geo, got %+v", g)
	}

	st := s.GeoIPStatus(context.Background())
	if !st.Enabled || st.Provider != "ip.sb" || st.Resolved != 1 || st.Pending != 0 || st.LastLookupAt == nil {
		t.Fatalf("status = %+v", st)
	}
	// A restarted resolver with nothing left to look up still reports the last lookup.
	if again := NewGeoIP(s.geo.db).Status(context.Background()); again.LastLookupAt == nil ||
		!again.LastLookupAt.Equal(*st.LastLookupAt) {
		t.Fatalf("status after restart = %+v, want last lookup %v", again, st.LastLookupAt)
	}
}

func TestGeoIPKeepsUnfinishedAddressesOnServerErrors(t *testing.T) {
	fake := &fakeIPSB{status: map[string]int{"8.8.8.8": http.StatusTooManyRequests, "1.1.1.1": http.StatusBadRequest}}
	s, clock := openGeoTestStoreClock(t, fake)
	writeIPs(t, s, "1.1.1.1", "8.8.8.8", "9.9.9.9")

	_, err := s.geo.Resolve(context.Background(), http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Resolve error = %v, want the 429", err)
	}
	if ok, _, _ := ipGeoOK(t, s, "8.8.8.8"); ok != ipGeoPending {
		t.Fatalf("rate-limited address state = %d, want pending", ok)
	}
	if st := s.GeoIPStatus(context.Background()); st.Pending < 1 || !strings.Contains(st.LastError, "429") {
		t.Fatalf("status after 429 = %+v", st)
	}

	// The limit lifts: the rate-limited address is retried; a 400 is stored as not found.
	fake.setStatus("8.8.8.8", 0)
	clock.add(geoIPRateLimitGap)
	if _, err = s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	geo := eventGeo(t, s)
	if g := geo["8.8.8.8"]; g == nil || g.Country != "Japan" {
		t.Fatalf("8.8.8.8 after retry = %+v", g)
	}
	if g := geo["1.1.1.1"]; g == nil || g.Scope != "public" || g.Country != "" {
		t.Fatalf("a 400 answer must leave the address without location, got %+v", g)
	}
	if st := s.GeoIPStatus(context.Background()); st.Pending != 0 || st.LastError != "" {
		t.Fatalf("status after recovery = %+v", st)
	}
}

func TestGeoIPDisabledSendsNothing(t *testing.T) {
	fake := &fakeIPSB{}
	s := openGeoTestStore(t, fake)
	s.geo.Configure(config.UsageGeoIPConfig{Disable: true})
	writeIPs(t, s, "153.156.0.1")
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != ErrGeoIPDisabled {
		t.Fatalf("Resolve = %v, want ErrGeoIPDisabled", err)
	}
	if asked := fake.askedIPs(); len(asked) != 0 {
		t.Fatalf("disabled lookups asked %v", asked)
	}
	if st := s.GeoIPStatus(context.Background()); st.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

func TestGeoIPPruneDropsUnreferencedAnswers(t *testing.T) {
	fake := &fakeIPSB{}
	s := openGeoTestStore(t, fake)
	writeIPs(t, s, "153.156.0.1")
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM events`); err != nil {
		t.Fatalf("delete events: %v", err)
	}
	s.geo.prune(context.Background())
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ip_geo`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ip_geo rows after prune = %d, %v", n, err)
	}
}

func (f *fakeIPSB) setStatus(ip string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[string]int{}
	}
	if code == 0 {
		delete(f.status, ip)
		return
	}
	f.status[ip] = code
}

func ipGeoOK(t *testing.T, s *Store, ip string) (int, int64, bool) {
	t.Helper()
	var ok int
	var at int64
	err := s.db.QueryRow(`SELECT ok, looked_up_at FROM ip_geo WHERE ip = ?`, ip).Scan(&ok, &at)
	if err != nil {
		return 0, 0, false
	}
	return ok, at, true
}

func TestGeoIPBacklogSurvivesAnError(t *testing.T) {
	fake := &fakeIPSB{}
	s, clock := openGeoTestStoreClock(t, fake)
	ips := make([]string, 61)
	for i := range ips {
		ips[i] = fmt.Sprintf("8.0.0.%d", i+1)
	}
	writeIPs(t, s, ips...)
	newest := ips[len(ips)-1]
	fake.setStatus(newest, http.StatusTooManyRequests)

	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("expected the 429 to stop the lookups")
	}
	if st := s.GeoIPStatus(context.Background()); st.Pending == 0 || st.Resolved+st.Pending != int64(len(ips)) {
		t.Fatalf("after 429: %+v, want every address resolved or pending", st)
	}

	fake.setStatus(newest, 0)
	clock.add(geoIPRateLimitGap)
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if st := s.GeoIPStatus(context.Background()); st.Resolved != int64(len(ips)) || st.Pending != 0 {
		t.Fatalf("after draining: %+v, want all %d resolved", st, len(ips))
	}
	if asked := fake.askedIPs(); len(asked) != len(ips)+1 {
		t.Fatalf("asked %d times, want each address once plus the rate-limited try", len(asked))
	}
}

func TestGeoIPLooksUpALongRequestFinishingLate(t *testing.T) {
	fake := &fakeIPSB{}
	s, clock := openGeoTestStoreClock(t, fake)
	requestStarted := clock.now()
	clock.add(2 * time.Hour)
	// Nothing to do yet while the request is still running.
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 0 || err != nil {
		t.Fatalf("empty round = %d, %v", n, err)
	}
	// Usage records carry the request start time, hours before the row is written.
	e := eventFromRecord(context.Background(), coreusage.Record{RequestedAt: requestStarted, Model: "m"})
	e.ClientIP = "8.8.8.8"
	if err := s.write([]Event{e}); err != nil {
		t.Fatal(err)
	}
	if st := s.GeoIPStatus(context.Background()); st.Pending != 1 {
		t.Fatalf("late request not pending: %+v", st)
	}
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 1 || err != nil {
		t.Fatalf("late request lookups = %d, %v; want 1", n, err)
	}
}

func TestGeoIPPacesLookupsToTheFreeLimit(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		// Hold the request briefly so lookups overlap; the pacing itself runs on the fake clock.
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = w.Write([]byte(`{"country_code":"JP","asn":4713}`))
	}))
	t.Cleanup(server.Close)
	s, clock := openGeoTestStoreClock(t, &fakeIPSB{})
	s.geo.endpoint = server.URL + "/geoip/"
	var mu sync.Mutex
	var starts []time.Time
	s.geo.started = func(_ string, at time.Time) {
		mu.Lock()
		starts = append(starts, at)
		mu.Unlock()
	}
	ips := make([]string, 130)
	for i := range ips {
		ips[i] = fmt.Sprintf("9.0.%d.%d", i/200, i%200+1)
	}
	writeIPs(t, s, ips...)
	begin := clock.now()
	n, err := s.geo.Resolve(context.Background(), http.DefaultClient)
	if err != nil || n != len(ips) {
		t.Fatalf("Resolve = %d, %v; want %d", n, err, len(ips))
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	for i := range starts {
		if i+geoIPPerSecond < len(starts) && starts[i+geoIPPerSecond].Sub(starts[i]) < time.Second {
			t.Fatalf("more than %d lookups within a second at %d", geoIPPerSecond, i)
		}
		if i+geoIPPerMinute < len(starts) && starts[i+geoIPPerMinute].Sub(starts[i]) < time.Minute {
			t.Fatalf("more than %d lookups within a minute at %d", geoIPPerMinute, i)
		}
	}
	if m := maxInFlight.Load(); m > geoIPWorkers {
		t.Fatalf("%d lookups in flight, want at most %d", m, geoIPWorkers)
	}
	if took := clock.now().Sub(begin); took < time.Minute {
		t.Fatalf("130 lookups finished in %v of pacing, want past the first minute", took)
	}
}

func TestGeoIPRateLimitPausesNewLookupsForAMinute(t *testing.T) {
	fake := &fakeIPSB{status: map[string]int{"8.8.8.8": http.StatusTooManyRequests}}
	s, _ := openGeoTestStoreClock(t, fake)
	writeIPs(t, s, "8.8.8.8")
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("expected the 429")
	}
	s.geo.mu.Lock()
	pausedUntil := s.geo.pausedUntil
	s.geo.mu.Unlock()
	fake.setStatus("8.8.8.8", 0)
	var first time.Time
	s.geo.started = func(_ string, at time.Time) {
		if first.IsZero() {
			first = at
		}
	}
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 1 || err != nil {
		t.Fatalf("retry = %d, %v", n, err)
	}
	if first.Before(pausedUntil) {
		t.Fatalf("lookup started at %v, before the pause ended at %v", first, pausedUntil)
	}
}

// roundTrip turns a function into an http.RoundTripper for tests that control each request.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"country_code":"JP","asn":4713}`))}
}

// waitUntil spins until cond holds; it synchronizes goroutines and asserts no timing.
func waitUntil(t *testing.T, cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Error("condition not reached")
			return false
		}
		runtime.Gosched()
	}
	return true
}

func TestGeoIPPacesActualRequestsWhenWorkersAreBusy(t *testing.T) {
	s, clock := openGeoTestStoreClock(t, &fakeIPSB{})
	for i := 1; i <= 11; i++ {
		writeIPs(t, s, fmt.Sprintf("9.9.9.%d", i))
	}
	base := clock.now()
	var mu sync.Mutex
	var sent []time.Time
	hold := make(chan struct{})
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		sent = append(sent, clock.now())
		n := len(sent)
		mu.Unlock()
		if n <= geoIPWorkers {
			<-hold // the first workers stay busy while the clock moves on
		}
		return jsonResponse(http.StatusOK), nil
	})}
	go func() {
		if waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(sent) == geoIPWorkers }) {
			clock.add(10 * time.Second)
		}
		close(hold)
	}()
	n, err := s.geo.Resolve(context.Background(), client)
	if err != nil || n != 11 {
		t.Fatalf("Resolve = %d, %v", n, err)
	}
	sort.Slice(sent, func(i, j int) bool { return sent[i].Before(sent[j]) })
	for i := 0; i+geoIPPerSecond < len(sent); i++ {
		if d := sent[i+geoIPPerSecond].Sub(sent[i]); d < time.Second {
			t.Fatalf("%d requests sent within %v (offsets from start: %v)", geoIPPerSecond+1, d, offsetsFrom(base, sent))
		}
	}
}

func offsetsFrom(base time.Time, ts []time.Time) []time.Duration {
	out := make([]time.Duration, len(ts))
	for i, t := range ts {
		out[i] = t.Sub(base)
	}
	return out
}

func TestGeoIPRateLimitStopsLookupsWaitingForAWorker(t *testing.T) {
	s, _ := openGeoTestStoreClock(t, &fakeIPSB{})
	for i := 1; i <= geoIPWorkers+1; i++ {
		writeIPs(t, s, fmt.Sprintf("8.8.8.%d", i))
	}
	var requests atomic.Int64
	paused := func() bool {
		s.geo.mu.Lock()
		defer s.geo.mu.Unlock()
		return !s.geo.pausedUntil.IsZero()
	}
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		n := requests.Add(1)
		if n == 1 {
			// Answer 429 once every worker is busy, so the next address waits for a slot.
			waitUntil(t, func() bool { return requests.Load() == geoIPWorkers })
			return jsonResponse(http.StatusTooManyRequests), nil
		}
		waitUntil(t, paused)
		return jsonResponse(http.StatusOK), nil
	})}
	if _, err := s.geo.Resolve(context.Background(), client); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Resolve error = %v, want the 429", err)
	}
	if n := requests.Load(); n != geoIPWorkers {
		t.Fatalf("%d requests sent, want %d: the one waiting for a worker must not start after the 429", n, geoIPWorkers)
	}
	if st := s.GeoIPStatus(context.Background()); st.Pending != 2 {
		t.Fatalf("pending = %d, want the 429 address and the unsent one", st.Pending)
	}
}

func TestGeoIPFailurePauseOutlivesThePass(t *testing.T) {
	s, clock := openGeoTestStoreClock(t, &fakeIPSB{status: map[string]int{"8.8.8.8": http.StatusServiceUnavailable}})
	writeIPs(t, s, "8.8.8.8")
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("expected the 503")
	}
	failedAt := clock.now()
	var first time.Time
	s.geo.started = func(_ string, at time.Time) {
		if first.IsZero() {
			first = at
		}
	}
	// A pass started right away (e.g. by a new address) waits for the pause instead of retrying.
	_, _ = s.geo.Resolve(context.Background(), http.DefaultClient)
	if first.IsZero() || first.Sub(failedAt) < time.Minute {
		t.Fatalf("retry started %v after the 503, want at least a minute", first.Sub(failedAt))
	}
}

func TestGeoIPDisableWhilePacingStopsNewLookups(t *testing.T) {
	s, clock := openGeoTestStoreClock(t, &fakeIPSB{})
	writeIPs(t, s, "8.8.8.8", "1.1.1.1")
	var count atomic.Int64
	var once sync.Once
	s.geo.sleep = func(ctx context.Context, d time.Duration) error {
		once.Do(func() { s.geo.Configure(config.UsageGeoIPConfig{Disable: true}) })
		return clock.sleep(ctx, d)
	}
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		count.Add(1)
		return jsonResponse(http.StatusOK), nil
	})}
	_, _ = s.geo.Resolve(context.Background(), client)
	if n := count.Load(); n > 1 {
		t.Fatalf("%d lookups, the second started after lookups were disabled", n)
	}
}

func TestGeoIPBackfillsClientIPsOfOlderDatabases(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	writeIPs(t, s, "8.8.8.8", "10.0.0.5")
	// A database written before client IPs were registered has events but no ip_geo rows.
	if _, err = s.db.Exec(`DELETE FROM ip_geo`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(filepath.Join(dir, "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if ok, _, found := ipGeoOK(t, s, "8.8.8.8"); !found || ok != ipGeoPending {
		t.Fatalf("public address after backfill = %d (found %v), want pending", ok, found)
	}
	if ok, _, found := ipGeoOK(t, s, "10.0.0.5"); !found || ok != ipGeoLocal {
		t.Fatalf("private address after backfill = %d (found %v), want local", ok, found)
	}
}

func TestGeoIPFindsNewClientAfterRetentionReusesEventID(t *testing.T) {
	fake := &fakeIPSB{}
	s := openGeoTestStore(t, fake)
	writeIPs(t, s, "8.8.8.8")
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 1 || err != nil {
		t.Fatalf("first round = %d, %v", n, err)
	}
	var firstID int64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM events`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	// Retention removes every row; SQLite then hands out the same id again.
	if _, err := s.db.Exec(`UPDATE events SET ts = 0`); err != nil {
		t.Fatal(err)
	}
	s.purge()
	writeIPs(t, s, "1.1.1.1")
	var reusedID int64
	if err := s.db.QueryRow(`SELECT MAX(id) FROM events`).Scan(&reusedID); err != nil || reusedID != firstID {
		t.Fatalf("expected id %d to be reused, got %d (%v)", firstID, reusedID, err)
	}
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 1 || err != nil {
		t.Fatalf("new client after retention = %d lookups, %v; want 1", n, err)
	}
}

func TestGeoIPMarksLocalAddressesOnceAndNeverSendsThem(t *testing.T) {
	fake := &fakeIPSB{}
	s, clock := openGeoTestStoreClock(t, fake)
	writeIPs(t, s, "10.0.0.5", "not-an-ip")
	// Local addresses are marked by the writer, before any lookup runs.
	ok, at, found := ipGeoOK(t, s, "10.0.0.5")
	if !found || ok != ipGeoLocal {
		t.Fatalf("10.0.0.5 marker = %d (found %v), want local", ok, found)
	}
	if ok, _, found = ipGeoOK(t, s, "not-an-ip"); !found || ok != ipGeoLocal {
		t.Fatalf("unparsable address marker = %d (found %v), want local", ok, found)
	}
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 0 || err != nil {
		t.Fatalf("Resolve = %d, %v", n, err)
	}
	clock.add(48 * time.Hour)
	writeIPs(t, s, "10.0.0.5")
	if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	if _, again, _ := ipGeoOK(t, s, "10.0.0.5"); again != at {
		t.Fatalf("local marker rewritten: %d -> %d", at, again)
	}
	if asked := fake.askedIPs(); len(asked) != 0 {
		t.Fatalf("local addresses were sent: %v", asked)
	}
	if geo := eventGeo(t, s)["10.0.0.5"]; geo == nil || geo.Scope != "private" || geo.Country != "" {
		t.Fatalf("local geo = %+v", geo)
	}
}

func TestGeoIPRetriesNotFoundOnlyAfterADay(t *testing.T) {
	fake := &fakeIPSB{status: map[string]int{"1.1.1.1": http.StatusBadRequest}}
	s, clock := openGeoTestStoreClock(t, fake)
	base := clock.now()
	writeIPs(t, s, "1.1.1.1")
	for _, step := range []time.Duration{0, time.Hour, 23 * time.Hour} {
		clock.set(base.Add(step))
		if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
			t.Fatal(err)
		}
	}
	if asked := fake.askedIPs(); len(asked) != 1 {
		t.Fatalf("asked %d times within a day, want 1", len(asked))
	}
	clock.set(base.Add(geoIPRetryAfter + time.Minute))
	if st := s.GeoIPStatus(context.Background()); st.Pending != 1 {
		t.Fatalf("due retry not pending: %+v", st)
	}
	if n, err := s.geo.Resolve(context.Background(), http.DefaultClient); n != 1 || err != nil {
		t.Fatalf("retry round = %d, %v", n, err)
	}
	if asked := fake.askedIPs(); len(asked) != 2 {
		t.Fatalf("asked %d times after a day, want 2", len(asked))
	}
}

func TestGeoIPEmptyAnswerIsNotCachedAsFound(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"ip":"8.8.8.8"}`} {
		fake := &fakeIPSB{body: body}
		s := openGeoTestStore(t, fake)
		writeIPs(t, s, "8.8.8.8")
		if _, err := s.geo.Resolve(context.Background(), http.DefaultClient); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		ok, _, found := ipGeoOK(t, s, "8.8.8.8")
		if !found || ok != ipGeoNotFound {
			t.Fatalf("%s: stored ok=%d found=%v, want not found", body, ok, found)
		}
		if st := s.GeoIPStatus(context.Background()); st.Resolved != 0 {
			t.Fatalf("%s: %+v", body, st)
		}
	}
}

func TestGeoIPStatusHasNoSideEffects(t *testing.T) {
	fake := &fakeIPSB{}
	s := openGeoTestStore(t, fake)
	writeIPs(t, s, "10.0.0.5", "8.8.8.8")
	snapshot := func() string {
		rows, err := s.db.Query(`SELECT ip, ok, looked_up_at FROM ip_geo ORDER BY ip`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var ip string
			var ok int
			var at int64
			if err := rows.Scan(&ip, &ok, &at); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s=%d@%d", ip, ok, at))
		}
		return strings.Join(out, ",")
	}
	before := snapshot()
	for i := 0; i < 2; i++ {
		if st := s.GeoIPStatus(context.Background()); st.Pending != 1 || st.Resolved != 0 {
			t.Fatalf("status %d = %+v", i, st)
		}
	}
	if after := snapshot(); after != before {
		t.Fatalf("status changed ip_geo: %s -> %s", before, after)
	}
	if asked := fake.askedIPs(); len(asked) != 0 {
		t.Fatalf("status sent %v", asked)
	}
}

func TestGeoIPCloseCancelsAnInFlightLookup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		if first {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.geo.endpoint = server.URL + "/geoip/"
	writeIPs(t, s, "8.8.8.8")
	s.startResolver()
	<-entered

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	// The handler never answers, so Close can only return if it cancels the lookup.
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel the in-flight lookup")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("lookups after Close: %d requests, want 1", requests)
	}
}
