package usagestore

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Antigravity sends no quota headers with its responses, so its windows are read from the
// retrieveUserQuotaSummary report instead. The report is fetched only for credentials that
// served a request since their last fetch, at most once per sampler tick, so idle
// credentials cause no upstream traffic.

// QuotaProbe fetches a credential's Antigravity retrieveUserQuotaSummary response body.
type QuotaProbe func(ctx context.Context, auth *coreauth.Auth) ([]byte, error)

var quotaProbe atomic.Pointer[QuotaProbe]

// SetQuotaProbe registers the fetcher for active quota reports (nil disables probing).
func SetQuotaProbe(probe QuotaProbe) {
	if probe == nil {
		quotaProbe.Store(nil)
		return
	}
	quotaProbe.Store(&probe)
}

func isAntigravity(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "antigravity")
}

// markQuotaActive records that a credential served a request, so the prober reads its quota.
func (s *Store) markQuotaActive(provider, authIndex string) {
	if authIndex != "" && isAntigravity(provider) {
		s.quotaActive.Store(authIndex, struct{}{})
	}
}

// startQuotaProber runs the active quota prober until the store closes. It has its own
// goroutine so slow upstream reports never delay the passive snapshot sampler.
func (s *Store) startQuotaProber() {
	s.lookups.Add(1)
	go func() {
		defer s.lookups.Done()
		last := map[string]quotaLast{}
		ticker := time.NewTicker(quotaSampleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-s.ctx.Done():
				return
			}
			s.probeQuota(last)
		}
	}()
}

func (s *Store) probeQuota(last map[string]quotaLast) {
	probe, list := quotaProbe.Load(), authLister.Load()
	if probe == nil || list == nil {
		return
	}
	var obs []quotaObservation
	for _, auth := range (*list)() {
		if auth == nil || auth.Disabled || !isAntigravity(auth.Provider) || auth.AuthKind() == coreauth.AuthKindAPIKey {
			continue
		}
		authIndex := auth.EnsureIndex()
		if _, active := s.quotaActive.LoadAndDelete(authIndex); !active {
			continue
		}
		body, err := (*probe)(s.ctx, auth)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			log.Debugf("usage store: antigravity quota probe for %s: %v", authIndex, err)
			continue
		}
		obs = append(obs, quotaObservation{authIndex, auth.Provider, parseAntigravityQuota(body), time.Now().UnixMilli()})
	}
	s.recordQuota(last, obs)
}

// antigravityQuotaPool maps a quota group to the model family it covers. Groups that cannot be
// attributed to models are skipped: their usage could not be priced.
func antigravityQuotaPool(group string) string {
	name := strings.ToLower(group)
	switch {
	case strings.Contains(name, "gemini"):
		return "gemini"
	case strings.Contains(name, "claude") || strings.Contains(name, "gpt"):
		return "claude-gpt"
	}
	return ""
}

// quotaPool returns the pool of a stored window: the part of an Antigravity window ID before
// its period suffix, empty for credential-wide windows.
func quotaPool(provider, window string) string {
	if !isAntigravity(provider) {
		return ""
	}
	if i := strings.LastIndexByte(window, '-'); i > 0 {
		return window[:i]
	}
	return ""
}

// quotaPoolModelSQL restricts an events query to the models a pool's quota covers. Events store
// the upstream model name, and SQLite LIKE ignores ASCII case.
func quotaPoolModelSQL(pool string) string {
	switch pool {
	case "gemini":
		return " AND model LIKE 'gemini%'"
	case "claude-gpt":
		return " AND (model LIKE 'claude%' OR model LIKE 'gpt%')"
	}
	return ""
}

func firstString(r gjson.Result, paths ...string) string {
	for _, p := range paths {
		if v := r.Get(p); v.Exists() {
			return strings.TrimSpace(v.String())
		}
	}
	return ""
}

// parseAntigravityQuota reads the windows of a retrieveUserQuotaSummary body, either bare or
// wrapped in "response". Window IDs are "<pool>-<5h|daily|weekly>".
func parseAntigravityQuota(body []byte) []quotaWindow {
	root := gjson.ParseBytes(body)
	if r := root.Get("response"); r.IsObject() {
		root = r
	}
	var out []quotaWindow
	seen := map[string]bool{}
	for _, group := range root.Get("groups").Array() {
		pool := antigravityQuotaPool(firstString(group, "displayName", "display_name"))
		if pool == "" {
			continue
		}
		for _, bucket := range group.Get("buckets").Array() {
			var window string
			var period int64
			switch strings.ToLower(firstString(bucket, "window")) {
			case "5h", "five-hour", "five_hour":
				window, period = "5h", 5*3600
			case "daily", "day":
				window, period = "daily", 86400
			case "weekly", "week":
				window, period = "weekly", 7*86400
			default:
				continue
			}
			var frac gjson.Result
			for _, p := range []string{"remainingFraction", "remaining_fraction", "remaining.remainingFraction"} {
				if frac = bucket.Get(p); frac.Exists() {
					break
				}
			}
			if frac.Type != gjson.Number {
				continue
			}
			used := math.Round((1-frac.Float())*100*1e6) / 1e6
			// An untouched bucket reports a rolling reset (request time plus the full window); storing
			// it would pin the window to a reset earlier than the one its first use fixes.
			if !isValidUsed(used) || used <= 0 {
				continue
			}
			reset, ok := parseResetTime(firstString(bucket, "resetTime", "reset_time"))
			if !ok {
				continue
			}
			id := pool + "-" + window
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, quotaWindow{ID: id, Period: period, ResetAt: reset, Used: used})
		}
	}
	return out
}
