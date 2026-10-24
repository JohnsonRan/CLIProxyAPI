package usagestore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// fakeIPSB answers like api.ip.sb/geoip/{ip} and records which addresses were asked.
type fakeIPSB struct {
	mu     sync.Mutex
	asked  []string
	status map[string]int
	agents []string
}

func (f *fakeIPSB) handler(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimPrefix(r.URL.Path, "/geoip/")
	f.mu.Lock()
	f.asked = append(f.asked, ip)
	f.agents = append(f.agents, r.UserAgent())
	code := f.status[ip]
	f.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
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
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	s.geo.endpoint = server.URL + "/geoip/"
	s.geo.gap = 0
	return s
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
}

func TestGeoIPKeepsUnfinishedAddressesOnServerErrors(t *testing.T) {
	fake := &fakeIPSB{status: map[string]int{"8.8.8.8": http.StatusTooManyRequests, "1.1.1.1": http.StatusBadRequest}}
	s := openGeoTestStore(t, fake)
	writeIPs(t, s, "1.1.1.1", "8.8.8.8", "9.9.9.9") // newest first: 9.9.9.9, 8.8.8.8, 1.1.1.1

	_, err := s.geo.Resolve(context.Background(), http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Resolve error = %v, want the 429", err)
	}
	st := s.GeoIPStatus(context.Background())
	if st.Resolved != 1 || st.Pending != 2 || !strings.Contains(st.LastError, "429") {
		t.Fatalf("status after 429 = %+v", st)
	}

	// The limit lifts: the 429 address and the one behind it are retried; a 400 is final.
	fake.mu.Lock()
	delete(fake.status, "8.8.8.8")
	fake.mu.Unlock()
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
	if st = s.GeoIPStatus(context.Background()); st.Pending != 0 || st.LastError != "" {
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
