package usagestore

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RealtimePoint holds usage metrics for a 1-minute time bucket.
type RealtimePoint struct {
	Time         int64   `json:"time"` // unix seconds
	Requests     int64   `json:"requests"`
	Failed       int64   `json:"failed"`
	Substituted  int64   `json:"substituted"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// RealtimeCurrent holds rolling rate and health metrics over the last 1 and 5 minutes.
type RealtimeCurrent struct {
	RPM1m        float64 `json:"rpm_1m"`
	TPM1m        float64 `json:"tpm_1m"`
	RPM5m        float64 `json:"rpm_5m"`
	TPM5m        float64 `json:"tpm_5m"`
	Failed5m     int64   `json:"failed_5m"`
	AvgLatency5m float64 `json:"avg_latency_5m"`
}

// Realtime is the response payload for GET /v8/management/observability/usage/realtime.
type Realtime struct {
	Now    int64 `json:"now"`
	LastID int64 `json:"last_id"`
	// InFlight counts requests and WebSocket sessions still running when the response was built;
	// it is process-wide and ignores the dimension filters.
	InFlight int64 `json:"in_flight"`
	// InFlightRequests lists up to 100 of those calls matching the dimension filters, newest first.
	InFlightRequests []InFlightRequest `json:"in_flight_requests"`
	Current          RealtimeCurrent   `json:"current"`
	Series           []RealtimePoint   `json:"series"`
}

// Realtime calculates zero-filled per-minute buckets for the last N minutes (1..180)
// and current rolling metrics (1m and 5m).
func (s *Store) Realtime(ctx context.Context, minutes int, filter StatsFilter) (*Realtime, error) {
	if minutes < 1 {
		minutes = 1
	} else if minutes > 180 {
		minutes = 180
	}

	now := time.Now()
	nowSec := now.Unix()
	nowMs := now.UnixMilli()

	currentMinuteIdx := nowMs / 60000
	startMinuteIdx := currentMinuteIdx - int64(minutes-1)

	// Ensure query window reaches at least 5 minutes back for 5m current stats
	queryStartMinuteIdx := startMinuteIdx
	if currentMinuteIdx-5 < queryStartMinuteIdx {
		queryStartMinuteIdx = currentMinuteIdx - 5
	}
	fromMs := queryStartMinuteIdx * 60000
	toMs := (currentMinuteIdx + 1) * 60000

	threshold1m := nowMs - 60*1000
	threshold5m := nowMs - 5*60*1000

	prices := s.priceCache()
	query := `SELECT (ts / 60000) AS minute_idx, model, service_tier, ` + contextBandSQL(prices.thresholds) + ` AS band,
		COUNT(*) AS reqs,
		SUM(failed) AS failed_cnt,
		SUM(substituted) AS sub_cnt,
		SUM(total) AS total_tok,
		SUM(latency_ms) AS lat_sum,
		SUM(input_uncached),
		SUM(cache_read),
		SUM(cache_write),
		SUM(output),
		SUM(reasoning),
		SUM(unclassified),
		SUM(CASE WHEN ts >= ? THEN 1 ELSE 0 END) AS req_1m,
		SUM(CASE WHEN ts >= ? THEN total ELSE 0 END) AS tok_1m,
		SUM(CASE WHEN ts >= ? THEN 1 ELSE 0 END) AS req_5m,
		SUM(CASE WHEN ts >= ? THEN total ELSE 0 END) AS tok_5m,
		SUM(CASE WHEN ts >= ? THEN failed ELSE 0 END) AS fail_5m,
		SUM(CASE WHEN ts >= ? THEN latency_ms ELSE 0 END) AS lat_5m
	FROM events
	WHERE ts >= ? AND ts < ?`
	args := []any{threshold1m, threshold1m, threshold5m, threshold5m, threshold5m, threshold5m, fromMs, toMs}

	for _, dim := range []struct {
		column string
		values []string
	}{
		{"model", filter.Model},
		{"provider", filter.Provider},
		{"api_key_id", filter.APIKeyID},
		{"auth_index", filter.AuthIndex},
	} {
		if len(dim.values) > 0 {
			placeholders := make([]string, len(dim.values))
			for i, v := range dim.values {
				placeholders[i] = "?"
				args = append(args, v)
			}
			query += fmt.Sprintf(" AND %s IN (%s)", dim.column, strings.Join(placeholders, ","))
		}
	}
	query += " GROUP BY minute_idx, model, service_tier, band"

	series := make([]RealtimePoint, minutes)
	latSums := make([]int64, minutes)
	for i := 0; i < minutes; i++ {
		series[i].Time = (startMinuteIdx + int64(i)) * 60
	}

	var totReq1m, totTok1m, totReq5m, totTok5m, totFail5m, totLat5m int64

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var minuteIdx int64
		var band int
		var model, tier string
		var reqs, failedCnt, subCnt, totalTok, latSum int64
		var inUncached, cRead, cWrite, outTok, reasonTok, unclassTok int64
		var r1m, t1m, r5m, t5m, f5m, l5m int64
		if errScan := rows.Scan(&minuteIdx, &model, &tier, &band, &reqs, &failedCnt, &subCnt, &totalTok, &latSum,
			&inUncached, &cRead, &cWrite, &outTok, &reasonTok, &unclassTok,
			&r1m, &t1m, &r5m, &t5m, &f5m, &l5m); errScan != nil {
			return nil, errScan
		}

		totReq1m += r1m
		totTok1m += t1m
		totReq5m += r5m
		totTok5m += t5m
		totFail5m += f5m
		totLat5m += l5m

		tok := Tokens{
			InputUncached: inUncached,
			CacheRead:     cRead,
			CacheWrite:    cWrite,
			Output:        outTok,
			Reasoning:     reasonTok,
			Unclassified:  unclassTok,
			Total:         totalTok,
		}
		var a Aggregate
		a.Tokens = tok
		a.Requests = reqs
		prices.apply(&a, model, tier, prices.bandInput(band))
		cost := a.Cost

		if minuteIdx >= startMinuteIdx && minuteIdx <= currentMinuteIdx {
			idx := int(minuteIdx - startMinuteIdx)
			series[idx].Requests += reqs
			series[idx].Failed += failedCnt
			series[idx].Substituted += subCnt
			series[idx].Tokens += totalTok
			series[idx].Cost += cost
			latSums[idx] += latSum
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}

	for i := 0; i < minutes; i++ {
		if series[i].Requests > 0 {
			series[i].AvgLatencyMs = float64(latSums[i]) / float64(series[i].Requests)
		}
	}

	var current RealtimeCurrent
	current.RPM1m = float64(totReq1m)
	current.TPM1m = float64(totTok1m)
	current.RPM5m = float64(totReq5m) / 5.0
	current.TPM5m = float64(totTok5m) / 5.0
	current.Failed5m = totFail5m
	if totReq5m > 0 {
		current.AvgLatency5m = float64(totLat5m) / float64(totReq5m)
	}

	var lastID int64
	_ = s.db.QueryRowContext(ctx, "SELECT IFNULL(MAX(id), 0) FROM events").Scan(&lastID)

	return &Realtime{
		Now:              nowSec,
		LastID:           lastID,
		InFlight:         InFlight(),
		InFlightRequests: inFlightRequests(now, filter),
		Current:          current,
		Series:           series,
	}, nil
}
