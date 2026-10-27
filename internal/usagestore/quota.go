package usagestore

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Quota value: the core already keeps each credential's last passive quota snapshot (the
// window percentages upstream sends with every response). The store samples those snapshots,
// keeps the first observation of every new percentage, and later divides the API-equivalent
// usage of a window by its used percentage to estimate what 100% of that window is worth.

const (
	quotaSampleEvery = time.Minute
	// shortcut: covers streams up to 30 min whose quota observation is stamped at completion; upgrade when observation time is stamped at header receipt
	quotaStartPadSeconds = 30 * 60
)

const quotaSchema = `
CREATE TABLE IF NOT EXISTS quota_samples (
	auth_index TEXT NOT NULL,
	provider TEXT NOT NULL,
	window TEXT NOT NULL,
	period_s INTEGER NOT NULL,
	reset_at INTEGER NOT NULL,
	used REAL NOT NULL,
	observed_at INTEGER NOT NULL,
	PRIMARY KEY (auth_index, window, observed_at)
);
CREATE INDEX IF NOT EXISTS quota_samples_observed ON quota_samples(observed_at);
CREATE INDEX IF NOT EXISTS events_auth_ts ON events(auth_index, ts);
`

var authLister atomic.Pointer[func() []*coreauth.Auth]

// SetAuthLister registers the source of credential quota snapshots (the core auth manager's List).
func SetAuthLister(list func() []*coreauth.Auth) {
	if list == nil {
		authLister.Store(nil)
		return
	}
	authLister.Store(&list)
}

// quotaWindow is one parsed quota window of a snapshot.
type quotaWindow struct {
	ID      string  // provider window name: "primary"/"secondary" (Codex), "5h"/"7d" (Claude)
	Period  int64   // seconds
	ResetAt int64   // unix seconds
	Used    float64 // percent, 0..100 (may exceed 100 when upstream reports overuse)
}

func signalValue(signals map[string]string, name string) (string, bool) {
	if v, ok := signals[name]; ok {
		return strings.TrimSpace(v), true
	}
	for k, v := range signals {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func signalFloat(signals map[string]string, name string) (float64, bool) {
	raw, ok := signalValue(signals, name)
	if !ok || raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func isValidUsed(used float64) bool {
	return !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0 && used <= 1000
}

// claudeFableWindow is the Claude weekly window of Fable models. On Max and premium seats Fable may use up to half of the weekly limit and also draws
// on the shared 7d window; where Fable is not included the window reports 100% or more.
const claudeFableWindow = "7d_oi"

// parseQuotaWindows reads the credential-wide windows of a passive snapshot, plus the Claude
// Fable window, which quotaPool assigns to the "fable" pool so only Fable traffic is divided by
// it. Other model-specific windows (Codex additional limits) are skipped: their usage is not the
// credential's whole traffic, so dividing it by the credential's cost would be wrong.
func parseQuotaWindows(provider string, signals map[string]string, observedAt time.Time) []quotaWindow {
	if len(signals) == 0 || observedAt.IsZero() {
		return nil
	}
	var out []quotaWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		for _, id := range []string{"primary", "secondary"} {
			prefix := "X-Codex-" + strings.ToUpper(id[:1]) + id[1:] + "-"
			used, okUsed := signalFloat(signals, prefix+"Used-Percent")
			minutes, okMinutes := signalFloat(signals, prefix+"Window-Minutes")
			if !okUsed || !okMinutes || !isValidUsed(used) || minutes <= 0 || minutes > 365*24*60 {
				continue
			}
			periodSeconds := minutes * 60
			if math.IsNaN(periodSeconds) || math.IsInf(periodSeconds, 0) || periodSeconds <= 0 || periodSeconds > 1e10 {
				continue
			}
			period := int64(periodSeconds)
			var reset int64
			if at, ok := signalFloat(signals, prefix+"Reset-At"); ok && at > 0 && at < 1e12 {
				reset = int64(at)
			} else if after, okAfter := signalFloat(signals, prefix+"Reset-After-Seconds"); okAfter && after >= 0 && after < 1e12 {
				reset = observedAt.Unix() + int64(after)
			} else {
				continue
			}
			if reset <= 0 {
				continue
			}
			out = append(out, quotaWindow{ID: id, Period: period, ResetAt: reset, Used: used})
		}
	case "claude":
		for _, w := range []struct {
			id     string
			period int64
		}{{"5h", 5 * 3600}, {"7d", 7 * 86400}, {claudeFableWindow, 7 * 86400}} {
			prefix := "Anthropic-Ratelimit-Unified-" + w.id + "-"
			utilization, okUsed := signalFloat(signals, prefix+"Utilization")
			rawReset, _ := signalValue(signals, prefix+"Reset")
			reset, okReset := parseResetTime(rawReset)
			if !okUsed || !okReset || utilization < 0 {
				continue
			}
			used := utilization * 100
			if !isValidUsed(used) || reset <= 0 || w.period <= 0 {
				continue
			}
			out = append(out, quotaWindow{ID: w.id, Period: w.period, ResetAt: reset, Used: used})
		}
	}
	return out
}

// parseResetTime reads a reset instant as unix seconds, RFC3339 or an HTTP date, the forms the
// Claude cooldown parser (helps.parseUnixOrTimestamp) accepts for the same headers.
func parseResetTime(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if sec, err := strconv.ParseFloat(raw, 64); err == nil {
		if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 || sec > 1e12 {
			return 0, false
		}
		return int64(sec), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		sec := t.Unix()
		return sec, sec > 0 && sec < 1e12
	}
	if t, err := http.ParseTime(raw); err == nil {
		sec := t.Unix()
		return sec, sec > 0 && sec < 1e12
	}
	return 0, false
}

type quotaLast struct {
	resetAt  int64
	used     float64
	observed int64
}

// quotaObservation is the parsed windows of one credential's quota report at one instant.
type quotaObservation struct {
	authIndex, provider string
	windows             []quotaWindow
	observed            int64 // unix ms
}

// startQuotaSampler runs the snapshot sampler until the store closes.
func (s *Store) startQuotaSampler() {
	s.lookups.Add(1)
	go func() {
		defer s.lookups.Done()
		last := map[string]quotaLast{}
		ticker := time.NewTicker(quotaSampleEvery)
		defer ticker.Stop()
		for {
			s.sampleQuota(last)
			select {
			case <-ticker.C:
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// sampleQuota stores the windows whose reset or percentage changed since the last sample.
// Keeping only changes records the moment each percentage was first reached, which is where
// usage-so-far best matches the reported percentage.
func (s *Store) sampleQuota(last map[string]quotaLast) {
	list := authLister.Load()
	if list == nil {
		return
	}
	var obs []quotaObservation
	for _, auth := range (*list)() {
		// API keys have no subscription quota; windows relayed by an upstream belong to its pool, not the key.
		if auth == nil || auth.Disabled || auth.Quota.ObservedAt.IsZero() || auth.AuthKind() == coreauth.AuthKindAPIKey {
			continue
		}
		authIndex := auth.EnsureIndex()
		if authIndex == "" {
			continue
		}
		obs = append(obs, quotaObservation{authIndex, auth.Provider,
			parseQuotaWindows(auth.Provider, auth.Quota.Signals, auth.Quota.ObservedAt), auth.Quota.ObservedAt.UnixMilli()})
	}
	s.recordQuota(last, obs)
}

// recordQuota stores the windows whose reset or percentage changed since last; last is owned
// by the calling sampler goroutine.
func (s *Store) recordQuota(last map[string]quotaLast, obs []quotaObservation) {
	type row struct {
		authIndex, provider string
		w                   quotaWindow
		observed            int64
	}
	var rows []row
	stagedLast := make(map[string]quotaLast)
	for _, o := range obs {
		authIndex, observed := o.authIndex, o.observed
		for _, w := range o.windows {
			key := authIndex + "\x00" + w.ID
			prev, seen := last[key]
			if seen && observed <= prev.observed {
				continue
			}
			tol := w.Period / 2
			if tol <= 0 {
				tol = 5 * 60
			}
			sameWindow := seen && absInt64(w.ResetAt-prev.resetAt) <= tol
			if sameWindow {
				if prev.resetAt < w.ResetAt {
					w.ResetAt = prev.resetAt
				}
				if w.Used == prev.used {
					stagedLast[key] = quotaLast{resetAt: prev.resetAt, used: prev.used, observed: observed}
					continue
				}
			}
			stagedLast[key] = quotaLast{resetAt: w.ResetAt, used: w.Used, observed: observed}
			rows = append(rows, row{authIndex, o.provider, w, observed})
		}
	}
	if len(rows) == 0 {
		for k, v := range stagedLast {
			last[k] = v
		}
		return
	}
	tx, err := s.db.BeginTx(s.ctx, nil)
	if err != nil {
		log.Warnf("usage store: record quota samples: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		if _, errExec := tx.Exec(`INSERT OR IGNORE INTO quota_samples (auth_index, provider, window, period_s, reset_at, used, observed_at)
			VALUES (?,?,?,?,?,?,?)`, r.authIndex, strings.ToLower(r.provider), r.w.ID, r.w.Period, r.w.ResetAt, r.w.Used, r.observed); errExec != nil {
			log.Warnf("usage store: record quota samples: %v", errExec)
			return
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		log.Warnf("usage store: record quota samples: %v", errCommit)
		return
	}
	for k, v := range stagedLast {
		last[k] = v
	}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// QuotaWindowValue is one quota window instance of a credential with its API-equivalent usage.
type QuotaWindowValue struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	// Pool names the model family a window covers when a credential has separate quotas per
	// family (Antigravity "gemini" and "claude-gpt"); empty when the window covers all traffic.
	Pool          string `json:"pool"`
	Window        string `json:"window"`
	PeriodSeconds int64  `json:"period_seconds"`
	// StartAt is the nominal window start (ResetAt - PeriodSeconds). The accounting bounds start
	// up to 30 minutes earlier (StartAt - 30m, not before the previous instance's reset) to cover
	// streams whose quota headers are observed upon completion, and end at the first observation
	// of the highest percentage, exclusive of ResetAt.
	StartAt int64 `json:"start_at"` // unix seconds
	ResetAt int64 `json:"reset_at"` // unix seconds
	// UsedPercent is the highest percentage seen in the window; ObservedAt (unix ms) is when it was first seen.
	UsedPercent float64 `json:"used_percent"`
	OwnPercent  float64 `json:"own_percent"`
	ObservedAt  int64   `json:"observed_at"`
	Samples     int     `json:"samples"`
	// Usage covers this credential's requests in the adjusted accounting bounds.
	Usage Aggregate `json:"usage"`
	// FullCost and FullTokens extrapolate Usage to 100% of the window; null when OwnPercent is 0.
	FullCost   *float64 `json:"full_cost"`
	FullTokens *float64 `json:"full_tokens"`
	// Partial is true when part of [StartAt, ObservedAt] is past the event retention window.
	Partial         bool    `json:"partial"`
	ExternalPercent float64 `json:"external_percent"`
	ExternalChecked bool    `json:"external_checked"`

	samplesList []samplePoint `json:"-"`
	inRange     bool          `json:"-"`
	inFitRange  bool          `json:"-"`
}

type samplePoint struct {
	observedAt  int64
	usedPercent float64
}

// QuotaValue lists quota window instances observed in a time range and the fitted statistical models.
type QuotaValue struct {
	From    int64              `json:"from"`
	To      int64              `json:"to"`
	Windows []QuotaWindowValue `json:"windows"`
	Models  []QuotaModel       `json:"models"`
}

type quotaSample struct {
	authIndex, provider, window string
	period, resetAt, observed   int64
	used                        float64
	inRange                     bool
	inFitRange                  bool
}

// groupQuotaSamples groups samples into window instances by matching each sample against any
// existing instance of the same credential and window whose ResetAt is within period/2.
// The minimum ResetAt seen in the instance is kept. Within an instance the highest percentage
// wins and keeps its first observation time, so a stale lower snapshot that arrives late
// (out-of-order responses) does not shrink the window.
func groupQuotaSamples(samples []quotaSample) []QuotaWindowValue {
	byKey := make(map[string][]int)
	var out []QuotaWindowValue
	for _, sm := range samples {
		key := sm.authIndex + "\x00" + sm.window
		indices := byKey[key]
		var targetIdx = -1
		tol := sm.period / 2
		if tol <= 0 {
			tol = 5 * 60
		}
		for _, idx := range indices {
			if absInt64(sm.resetAt-out[idx].ResetAt) <= tol {
				targetIdx = idx
				break
			}
		}
		if targetIdx == -1 {
			out = append(out, QuotaWindowValue{
				AuthIndex:     sm.authIndex,
				Provider:      sm.provider,
				Pool:          quotaPool(sm.provider, sm.window),
				Window:        sm.window,
				PeriodSeconds: sm.period,
				ResetAt:       sm.resetAt,
				StartAt:       sm.resetAt - sm.period,
				UsedPercent:   sm.used,
				ObservedAt:    sm.observed,
				Samples:       1,
				samplesList:   []samplePoint{{observedAt: sm.observed, usedPercent: sm.used}},
				inRange:       sm.inRange,
				inFitRange:    sm.inFitRange,
			})
			byKey[key] = append(byKey[key], len(out)-1)
		} else {
			target := &out[targetIdx]
			target.Samples++
			if sm.inRange {
				target.inRange = true
			}
			if sm.inFitRange {
				target.inFitRange = true
			}
			if sm.resetAt < target.ResetAt {
				target.ResetAt = sm.resetAt
				target.StartAt = sm.resetAt - target.PeriodSeconds
			}
			if sm.used > target.UsedPercent {
				target.UsedPercent = sm.used
				target.ObservedAt = sm.observed
			}
			target.samplesList = append(target.samplesList, samplePoint{observedAt: sm.observed, usedPercent: sm.used})
		}
	}
	return out
}

// QuotaValue returns the window instances of credentials (all when authIndex is empty) with a
// sample in [from, to), newest first, each with the usage it took to reach its percentage,
// and the fitted statistical valuation models.
func (s *Store) QuotaValue(ctx context.Context, from, to time.Time, authIndex []string) (*QuotaValue, error) {
	fitDays := int(s.retention.Load())
	if fitDays <= 0 || fitDays > 60 {
		fitDays = 60
	}
	fitFrom := to.Add(-time.Duration(fitDays) * 24 * time.Hour)
	sampleFrom := from
	if fitFrom.Before(sampleFrom) {
		sampleFrom = fitFrom
	}
	lookbackFrom := sampleFrom.Add(-8 * 24 * time.Hour)
	query := `SELECT auth_index, provider, window, period_s, reset_at, used, observed_at FROM quota_samples
		WHERE observed_at >= ? AND observed_at < ?`
	args := []any{lookbackFrom.UnixMilli(), to.UnixMilli()}
	if len(authIndex) > 0 {
		query += " AND auth_index IN (" + strings.TrimSuffix(strings.Repeat("?,", len(authIndex)), ",") + ")"
		for _, v := range authIndex {
			args = append(args, v)
		}
	}
	query += " ORDER BY auth_index, window, observed_at"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var samples []quotaSample
	for rows.Next() {
		var sm quotaSample
		if errScan := rows.Scan(&sm.authIndex, &sm.provider, &sm.window, &sm.period, &sm.resetAt, &sm.used, &sm.observed); errScan != nil {
			_ = rows.Close()
			return nil, errScan
		}
		sm.inRange = sm.observed >= from.UnixMilli() && sm.observed < to.UnixMilli()
		sm.inFitRange = sm.observed >= fitFrom.UnixMilli() && sm.observed < to.UnixMilli()
		samples = append(samples, sm)
	}
	_ = rows.Close()
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}

	windows := groupQuotaSamples(samples)
	prices := s.priceCache()
	retentionStart := int64(math.MinInt64)
	if days := s.retention.Load(); days >= 0 {
		retentionStart = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	}

	// Sort by credential, window, and ResetAt ascending so we can identify preceding instances.
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].AuthIndex != windows[j].AuthIndex {
			return windows[i].AuthIndex < windows[j].AuthIndex
		}
		if windows[i].Window != windows[j].Window {
			return windows[i].Window < windows[j].Window
		}
		return windows[i].ResetAt < windows[j].ResetAt
	})

	type winSegmentsData struct {
		segments []quotaSegment
	}
	winSegs := make([]winSegmentsData, len(windows))

	for i := range windows {
		w := &windows[i]
		sort.Slice(w.samplesList, func(a, b int) bool {
			return w.samplesList[a].observedAt < w.samplesList[b].observedAt
		})
	}
	exhausted := quotaExhaustedIntervals(windows)

	for i := range windows {
		w := &windows[i]
		if !w.inRange && !w.inFitRange {
			continue
		}
		startMs := w.StartAt * 1000
		fromMs := startMs - quotaStartPadSeconds*1000
		if i > 0 && windows[i-1].AuthIndex == w.AuthIndex && windows[i-1].Window == w.Window {
			prevResetMs := windows[i-1].ResetAt * 1000
			if prevResetMs > fromMs {
				fromMs = prevResetMs
			}
		}
		toMs := w.ObservedAt
		resetMs := w.ResetAt * 1000
		resetIsBinding := false
		if resetMs <= toMs {
			toMs = resetMs
			resetIsBinding = true
		}

		var incSamples []samplePoint
		for _, sp := range w.samplesList {
			if sp.observedAt > toMs {
				continue
			}
			if len(incSamples) > 0 && sp.observedAt == incSamples[len(incSamples)-1].observedAt {
				incSamples[len(incSamples)-1].usedPercent = sp.usedPercent
			} else if len(incSamples) == 0 || sp.usedPercent > incSamples[len(incSamples)-1].usedPercent {
				incSamples = append(incSamples, sp)
			}
		}

		exclSQL := quotaExhaustedSQL(exhausted, w, fromMs, toMs)
		segs, extPercent, extChecked, errUsage := s.windowUsage(ctx, prices, w.AuthIndex, w.Pool, exclSQL, i, fromMs, toMs, resetIsBinding, incSamples, &w.Usage)
		if errUsage != nil {
			return nil, errUsage
		}
		winSegs[i] = winSegmentsData{segments: segs}
		w.ExternalPercent = extPercent
		w.ExternalChecked = extChecked
		w.OwnPercent = w.UsedPercent - extPercent
		if w.OwnPercent < 0 {
			w.OwnPercent = 0
		}
		w.Partial = startMs < retentionStart
		if w.OwnPercent > 0 {
			cost := w.Usage.Cost * 100 / w.OwnPercent
			tokens := float64(w.Usage.Tokens.Total) * 100 / w.OwnPercent
			w.FullCost, w.FullTokens = &cost, &tokens
		}
	}

	// Fit models per (auth_index, pool, period_seconds)
	type fitUnitKey struct {
		authIndex     string
		pool          string
		periodSeconds int64
	}
	unitWindows := make(map[fitUnitKey][]int)
	unitProviders := make(map[fitUnitKey]string)
	for i := range windows {
		w := &windows[i]
		if w.inFitRange {
			k := fitUnitKey{w.AuthIndex, w.Pool, w.PeriodSeconds}
			unitWindows[k] = append(unitWindows[k], i)
			unitProviders[k] = w.Provider
		}
	}
	for i := range windows {
		w := &windows[i]
		if w.inRange {
			k := fitUnitKey{w.AuthIndex, w.Pool, w.PeriodSeconds}
			if _, ok := unitWindows[k]; !ok {
				unitWindows[k] = nil
				unitProviders[k] = w.Provider
			}
		}
	}

	var models []QuotaModel
	for unit, winIndices := range unitWindows {
		hasInRange := false
		for i := range windows {
			if windows[i].inRange && windows[i].AuthIndex == unit.authIndex && windows[i].Pool == unit.pool && windows[i].PeriodSeconds == unit.periodSeconds {
				hasInRange = true
				break
			}
		}
		if !hasInRange {
			continue
		}

		var allSegs []quotaSegment
		var allWeights []float64
		for _, wIdx := range winIndices {
			segs := winSegs[wIdx].segments
			for _, s := range segs {
				// shortcut: fixed 14-day half-life for recency weighting
				ageDays := float64(to.UnixMilli()-s.endMs) / (24 * 3600 * 1000)
				if ageDays < 0 {
					ageDays = 0
				}
				w := math.Pow(0.5, ageDays/14.0)
				allSegs = append(allSegs, s)
				allWeights = append(allWeights, w)
			}
		}

		res1 := fitLayer1(allSegs, allWeights)
		qm := QuotaModel{
			AuthIndex:        unit.authIndex,
			Provider:         unitProviders[unit],
			Pool:             unit.pool,
			PeriodSeconds:    unit.periodSeconds,
			Status:           res1.status,
			Windows:          res1.windowsCount,
			Segments:         res1.segmentsCount,
			ExternalSegments: res1.externalSegments,
			CostUsed:         res1.costUsed,
			PctPerUSD:        nil,
			FullCost:         res1.fullCost,
			FullCostLow:      res1.fullCostLow,
			FullCostHigh:     res1.fullCostHigh,
			CIPercent:        res1.ciPercent,
			Features:         []QuotaModelFeature{},
			ModelValues:      []QuotaModelValue{},
		}

		if res1.status == "fitted" {
			kVal := res1.k
			qm.PctPerUSD = &kVal
			feats, mvals := fitLayer2(allSegs, allWeights, res1.k, res1.isExternal)
			qm.Features = feats
			qm.ModelValues = mvals

			// Recompute external deduction using model k for windows in this unit
			for i := range windows {
				w := &windows[i]
				if w.AuthIndex == unit.authIndex && w.Pool == unit.pool && w.PeriodSeconds == unit.periodSeconds {
					segs := winSegs[i].segments
					if len(segs) > 0 {
						extPercent := 0.0
						for _, s := range segs {
							if s.unpriced {
								continue
							}
							if s.deltaU >= 2 && res1.k*s.cost < 0.2*s.deltaU {
								excess := s.deltaU - res1.k*s.cost
								if excess > 0 {
									extPercent += excess
								}
							}
						}
						if extPercent > w.UsedPercent {
							extPercent = w.UsedPercent
						}
						w.ExternalPercent = extPercent
						w.ExternalChecked = true
						w.OwnPercent = w.UsedPercent - extPercent
						if w.OwnPercent < 0 {
							w.OwnPercent = 0
						}
						if w.OwnPercent > 0 {
							cost := w.Usage.Cost * 100.0 / w.OwnPercent
							tokens := float64(w.Usage.Tokens.Total) * 100.0 / w.OwnPercent
							w.FullCost = &cost
							w.FullTokens = &tokens
						} else {
							w.FullCost = nil
							w.FullTokens = nil
						}
					}
				}
			}
		}

		models = append(models, qm)
	}

	sort.Slice(models, func(i, j int) bool {
		if models[i].AuthIndex != models[j].AuthIndex {
			return models[i].AuthIndex < models[j].AuthIndex
		}
		if models[i].Pool != models[j].Pool {
			return models[i].Pool < models[j].Pool
		}
		return models[i].PeriodSeconds < models[j].PeriodSeconds
	})
	if models == nil {
		models = []QuotaModel{}
	}

	var inRangeWindows []QuotaWindowValue
	for _, w := range windows {
		if w.inRange {
			inRangeWindows = append(inRangeWindows, w)
		}
	}
	windows = inRangeWindows

	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].ResetAt != windows[j].ResetAt {
			return windows[i].ResetAt > windows[j].ResetAt
		}
		if windows[i].AuthIndex != windows[j].AuthIndex {
			return windows[i].AuthIndex < windows[j].AuthIndex
		}
		return windows[i].Window < windows[j].Window
	})
	if windows == nil {
		windows = []QuotaWindowValue{}
	}

	for i := range windows {
		w := &windows[i]
		w.UsedPercent = sanitizeFinite(w.UsedPercent, 0)
		w.OwnPercent = sanitizeFinite(w.OwnPercent, 0)
		w.ExternalPercent = sanitizeFinite(w.ExternalPercent, 0)
		w.Usage.Cost = sanitizeFinite(w.Usage.Cost, 0)
		w.Usage.AvgLatencyMs = sanitizeFinite(w.Usage.AvgLatencyMs, 0)
		w.Usage.AvgTTFTMs = sanitizeFinite(w.Usage.AvgTTFTMs, 0)
		w.Usage.OutputTPS = sanitizeFinite(w.Usage.OutputTPS, 0)
		w.FullCost = sanitizeFinitePtr(w.FullCost)
		w.FullTokens = sanitizeFinitePtr(w.FullTokens)
	}

	for i := range models {
		m := &models[i]
		m.CostUsed = sanitizeFinite(m.CostUsed, 0)
		m.PctPerUSD = sanitizeFinitePtr(m.PctPerUSD)
		m.FullCost = sanitizeFinitePtr(m.FullCost)
		m.FullCostLow = sanitizeFinitePtr(m.FullCostLow)
		m.FullCostHigh = sanitizeFinitePtr(m.FullCostHigh)
		m.CIPercent = sanitizeFinitePtr(m.CIPercent)
		for j := range m.Features {
			f := &m.Features[j]
			f.Multiplier = sanitizeFinite(f.Multiplier, 1.0)
			f.Share = sanitizeFinite(f.Share, 0)
		}
		var cleanValues []QuotaModelValue
		for _, mv := range m.ModelValues {
			if isFinite(mv.FullCost) && mv.FullCost > 0 {
				cleanValues = append(cleanValues, mv)
			}
		}
		if cleanValues == nil {
			cleanValues = []QuotaModelValue{}
		}
		m.ModelValues = cleanValues
	}

	return &QuotaValue{From: from.Unix(), To: to.Unix(), Windows: windows, Models: models}, nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

func sanitizeFinite(f, fallback float64) float64 {
	if !isFinite(f) {
		return fallback
	}
	return f
}

func sanitizeFinitePtr(p *float64) *float64 {
	if p == nil || !isFinite(*p) {
		return nil
	}
	return p
}

// quotaExhausted is a span in which a window instance stood at 100% or more. Requests its pool
// covers were then either rejected or paid with usage credits, so they drew on no other window.
type quotaExhausted struct {
	authIndex, window, pool string
	fromMs, toMs            int64
}

// quotaExhaustedIntervals lists the exhausted spans of window instances. windows must have sorted
// samplesList. A span starts at the first sample at 100% or more, or at the nominal window start
// when no earlier sample is below 100% (exhausted before sampling began, or a Fable window on a
// plan that does not include Fable), and ends at the reset.
func quotaExhaustedIntervals(windows []QuotaWindowValue) []quotaExhausted {
	var out []quotaExhausted
	for i := range windows {
		w := &windows[i]
		for k, sp := range w.samplesList {
			if sp.usedPercent < 100 {
				continue
			}
			from := sp.observedAt
			if k == 0 {
				from = w.StartAt * 1000
			}
			if to := w.ResetAt * 1000; from < to {
				out = append(out, quotaExhausted{w.AuthIndex, w.Window, w.Pool, from, to})
			}
			break
		}
	}
	return out
}

// quotaExhaustedSQL excludes from w's events those inside an exhausted span of another window of
// the same credential whose pool covers them. w's own exhaustion needs no exclusion: its usage
// already ends where its highest percentage was first seen.
func quotaExhaustedSQL(exhausted []quotaExhausted, w *QuotaWindowValue, fromMs, toMs int64) string {
	var b strings.Builder
	for _, e := range exhausted {
		if e.authIndex != w.AuthIndex || e.window == w.Window || e.toMs <= fromMs || e.fromMs > toMs {
			continue
		}
		fmt.Fprintf(&b, " AND NOT (ts >= %d AND ts < %d%s)", e.fromMs, e.toMs, quotaPoolModelSQL(e.pool))
	}
	return b.String()
}

// windowUsage sums one credential's events of a pool in [fromMs, toMs] into a, priced per served model
// and tier, collects per-interval segments, and evaluates fallback passive external usage over the quota
// samples. exclSQL holds extra WHERE conditions (quotaExhaustedSQL).
func (s *Store) windowUsage(ctx context.Context, prices *queryPrices, authIndex, pool, exclSQL string, windowIdx int, fromMs, toMs int64, resetIsBinding bool, samples []samplePoint, a *Aggregate) ([]quotaSegment, float64, bool, error) {
	op := "<="
	if resetIsBinding {
		op = "<"
	}
	poolSQL := quotaPoolModelSQL(pool) + exclSQL

	numSamples := len(samples)
	var caseSQL string
	if numSamples <= 1 {
		caseSQL = "0 AS interval_idx"
	} else {
		var b strings.Builder
		b.WriteString("(CASE")
		for i := 0; i < numSamples-1; i++ {
			fmt.Fprintf(&b, " WHEN ts <= %d THEN %d", samples[i].observedAt, i)
		}
		fmt.Fprintf(&b, " ELSE %d END) AS interval_idx", numSamples-1)
		caseSQL = b.String()
	}

	query := fmt.Sprintf(`SELECT model, `+quotaServedModelSQL+` AS served, service_tier, `+contextBandSQL(prices.thresholds)+` AS band,
		`+caseSQL+`,
		COUNT(*), SUM(failed), SUM(substituted), SUM(input_uncached), SUM(cache_read), SUM(cache_write), SUM(output),
		SUM(reasoning), SUM(unclassified), SUM(total), SUM(latency_ms), SUM(`+firstTokenSQL+`), SUM(`+firstTokenSQL+` > 0),
		SUM(`+genOutputSQL+`), SUM(`+genMsSQL+`)
		FROM events WHERE auth_index = ? AND ts >= ? AND ts %s ?%s
		GROUP BY model, served, service_tier, band, interval_idx`, op, poolSQL)

	rows, err := s.db.QueryContext(ctx, query, authIndex, fromMs, toMs)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()

	segments := make([]quotaSegment, numSamples)
	var prevUsed float64 = 0
	for i := 0; i < numSamples; i++ {
		segments[i] = quotaSegment{
			windowIdx: windowIdx,
			deltaU:    samples[i].usedPercent - prevUsed,
			endMs:     samples[i].observedAt,
			kindCosts: make(map[segmentModelKind]float64),
		}
		prevUsed = samples[i].usedPercent
	}

	intervalTokens := make([]int64, max(1, numSamples))
	for rows.Next() {
		var requested, model, tier string
		var band, intervalIdx int
		var part Aggregate
		t := &part.Tokens
		if errScan := rows.Scan(&requested, &model, &tier, &band, &intervalIdx, &part.Requests, &part.Failed, &part.Substituted, &t.InputUncached,
			&t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified, &t.Total, &part.latencySum,
			&part.ttftSum, &part.ttftCount, &part.genOutput, &part.genMs); errScan != nil {
			return nil, 0, false, errScan
		}
		// Price what served the request; keep the requested model when the served name has no price.
		if _, ok := prices.lookup(model, tier, prices.bandInput(band)); !ok {
			model = requested
		}
		prices.apply(&part, model, tier, prices.bandInput(band))
		a.add(&part)
		if intervalIdx >= 0 && intervalIdx < len(intervalTokens) {
			intervalTokens[intervalIdx] += t.Total
		}
		if intervalIdx >= 0 && intervalIdx < len(segments) {
			seg := &segments[intervalIdx]
			if part.UnpricedRequests > 0 {
				seg.unpriced = true
			} else {
				price, ok := prices.lookup(model, tier, prices.bandInput(band))
				if !ok {
					seg.unpriced = true
				} else {
					inToks := t.InputUncached + t.Unclassified
					crToks := t.CacheRead
					cwToks := t.CacheWrite
					outToks := t.Output

					inCost := float64(inToks) * price.Input / 1e6
					crCost := float64(crToks) * price.CacheRead / 1e6
					cwCost := float64(cwToks) * price.CacheWrite / 1e6
					rPrice := price.Reasoning
					if rPrice <= 0 {
						rPrice = price.Output
					}
					outCost := (float64(t.Output-t.Reasoning)*price.Output + float64(t.Reasoning)*rPrice) / 1e6

					seg.cost += inCost + crCost + cwCost + outCost

					if inToks > 0 || inCost > 0 {
						mk := segmentModelKind{model: model, kind: "input"}
						seg.kindCosts[mk] += inCost
					}
					if crToks > 0 || crCost > 0 {
						mk := segmentModelKind{model: model, kind: "cache_read"}
						seg.kindCosts[mk] += crCost
					}
					if cwToks > 0 || cwCost > 0 {
						mk := segmentModelKind{model: model, kind: "cache_write"}
						seg.kindCosts[mk] += cwCost
					}
					if outToks > 0 || outCost > 0 {
						mk := segmentModelKind{model: model, kind: "output"}
						seg.kindCosts[mk] += outCost
					}
				}
			}
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, 0, false, errRows
	}
	a.finish()

	extPercent, extChecked := detectExternalUsage(samples, intervalTokens, segments)
	return segments, extPercent, extChecked, nil
}

func detectExternalUsage(samples []samplePoint, intervalTokens []int64, segments []quotaSegment) (float64, bool) {
	if len(samples) < 3 {
		return 0, false
	}

	type intervalStat struct {
		deltaUsed float64
		tokens    int64
		rate      float64
		unpriced  bool
	}

	var intervals []intervalStat
	var prevUsed float64 = 0
	for i, sm := range samples {
		delta := sm.usedPercent - prevUsed
		prevUsed = sm.usedPercent
		if delta > 0 {
			var toks int64
			if i < len(intervalTokens) {
				toks = intervalTokens[i]
			}
			unpriced := false
			if i < len(segments) {
				unpriced = segments[i].unpriced
			}
			intervals = append(intervals, intervalStat{
				deltaUsed: delta,
				tokens:    toks,
				rate:      float64(toks) / delta,
				unpriced:  unpriced,
			})
		}
	}

	if len(intervals) < 3 {
		return 0, false
	}

	rates := make([]float64, len(intervals))
	for i, it := range intervals {
		rates[i] = it.rate
	}
	sort.Float64s(rates)

	var median float64
	mid := len(rates) / 2
	if len(rates)%2 == 1 {
		median = rates[mid]
	} else {
		median = (rates[mid-1] + rates[mid]) / 2.0
	}

	var extPercent float64
	for _, it := range intervals {
		if it.unpriced {
			continue
		}
		if it.deltaUsed >= 2 && float64(it.tokens) < 0.2*median*it.deltaUsed {
			extPercent += it.deltaUsed
		}
	}
	lastUsed := samples[len(samples)-1].usedPercent
	if extPercent > lastUsed {
		extPercent = lastUsed
	}
	return extPercent, true
}
