package usagestore

import (
	"context"
	"sort"
	"strings"
	"time"
)

// maxLatencySamples bounds the rows read for percentiles; larger ranges are sampled evenly by id.
const maxLatencySamples = 200_000

// LatencyBucketBoundsMs are the upper bounds of the latency histogram buckets; the last bucket is open.
var LatencyBucketBoundsMs = []int64{500, 1000, 2000, 5000, 10_000, 30_000, 60_000}

// Percentiles holds latency percentiles in milliseconds.
type Percentiles struct {
	Count int64 `json:"count"`
	P50   int64 `json:"p50"`
	P90   int64 `json:"p90"`
	P95   int64 `json:"p95"`
	P99   int64 `json:"p99"`
	Max   int64 `json:"max"`
}

// ModelLatency is the latency profile of one model.
type ModelLatency struct {
	Model   string      `json:"model"`
	Latency Percentiles `json:"latency"`
	TTFT    Percentiles `json:"ttft"`
}

// HeatCell is the activity of one weekday (0 = Sunday) and local hour.
type HeatCell struct {
	Weekday  int     `json:"weekday"`
	Hour     int     `json:"hour"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Failed   int64   `json:"failed"`
	Cost     float64 `json:"cost"`
}

// Analysis is the latency and activity breakdown for a time range.
type Analysis struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Sampled is true when percentiles were computed from an even sample of the events.
	Sampled bool        `json:"sampled"`
	Latency Percentiles `json:"latency"`
	TTFT    Percentiles `json:"ttft"`
	// Histogram counts successful requests per LatencyBucketBoundsMs bucket plus one open bucket.
	Histogram       []int64        `json:"histogram"`
	HistogramBounds []int64        `json:"histogram_bounds_ms"`
	Models          []ModelLatency `json:"models"`
	Heatmap         []HeatCell     `json:"heatmap"`
}

func percentiles(values []int64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	at := func(q float64) int64 { return values[int(q*float64(len(values)-1)+0.5)] }
	return Percentiles{Count: int64(len(values)), P50: at(0.5), P90: at(0.9), P95: at(0.95), P99: at(0.99), Max: values[len(values)-1]}
}

// Analysis computes latency percentiles from successful events in [from, to) (events are pruned after
// the retention window) and a weekday x hour heatmap from hourly rollups aligned to tzOffsetMinutes,
// matching the supplied dimension filters.
func (s *Store) Analysis(ctx context.Context, from, to time.Time, tzOffsetMinutes int, filter StatsFilter) (*Analysis, error) {
	out := &Analysis{From: from.Unix(), To: to.Unix(), HistogramBounds: LatencyBucketBoundsMs,
		Histogram: make([]int64, len(LatencyBucketBoundsMs)+1), Models: []ModelLatency{}, Heatmap: []HeatCell{}}

	whereEvents := "ts >= ? AND ts < ? AND failed = 0"
	argsEvents := []any{from.UnixMilli(), to.UnixMilli()}
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
				argsEvents = append(argsEvents, v)
			}
			whereEvents += " AND " + dim.column + " IN (" + strings.Join(placeholders, ",") + ")"
		}
	}

	var total, minID int64
	countQuery := "SELECT COUNT(*), IFNULL(MIN(id),0) FROM events WHERE " + whereEvents
	if err := s.db.QueryRowContext(ctx, countQuery, argsEvents...).Scan(&total, &minID); err != nil {
		return nil, err
	}
	step := int64(1)
	if total > maxLatencySamples {
		step = (total + maxLatencySamples - 1) / maxLatencySamples
		out.Sampled = true
	}
	dataQuery := "SELECT model, latency_ms, ttft_ms FROM events WHERE " + whereEvents + " AND (id - ?) % ? = 0"
	dataArgs := append(append([]any(nil), argsEvents...), minID, step)
	rows, err := s.db.QueryContext(ctx, dataQuery, dataArgs...)
	if err != nil {
		return nil, err
	}
	var latency, ttft []int64
	perModel := map[string]*[2][]int64{}
	for rows.Next() {
		var model string
		var l, f int64
		if errScan := rows.Scan(&model, &l, &f); errScan != nil {
			_ = rows.Close()
			return nil, errScan
		}
		m := perModel[model]
		if m == nil {
			m = &[2][]int64{}
			perModel[model] = m
		}
		if l > 0 {
			latency = append(latency, l)
			m[0] = append(m[0], l)
			out.Histogram[sort.Search(len(LatencyBucketBoundsMs), func(i int) bool { return l <= LatencyBucketBoundsMs[i] })]++
		}
		if f > 0 {
			ttft = append(ttft, f)
			m[1] = append(m[1], f)
		}
	}
	_ = rows.Close()
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}
	out.Latency, out.TTFT = percentiles(latency), percentiles(ttft)
	for model, m := range perModel {
		out.Models = append(out.Models, ModelLatency{Model: model, Latency: percentiles(m[0]), TTFT: percentiles(m[1])})
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Latency.Count > out.Models[j].Latency.Count })

	off := int64(tzOffsetMinutes) * 60
	hquery := `SELECT hour, model, service_tier, SUM(requests), SUM(failed), SUM(input_uncached),
		SUM(cache_read), SUM(cache_write), SUM(output), SUM(reasoning), SUM(unclassified), SUM(total)
		FROM hourly WHERE hour >= ? AND hour < ?`
	hargs := []any{from.Unix() / 3600 * 3600, to.Unix()}
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
				hargs = append(hargs, v)
			}
			hquery += " AND " + dim.column + " IN (" + strings.Join(placeholders, ",") + ")"
		}
	}
	hquery += " GROUP BY hour, model, service_tier"
	hrows, err := s.db.QueryContext(ctx, hquery, hargs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = hrows.Close() }()
	cells := map[[2]int]*HeatCell{}
	prices := s.priceCache()
	for hrows.Next() {
		var hour int64
		var model, tier string
		var a Aggregate
		t := &a.Tokens
		if errScan := hrows.Scan(&hour, &model, &tier, &a.Requests, &a.Failed, &t.InputUncached, &t.CacheRead,
			&t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified, &t.Total); errScan != nil {
			return nil, errScan
		}
		// Hourly rollups have no context bands, so the heatmap uses base prices.
		prices.apply(&a, model, tier, 0)
		local := hour + off
		if local < 0 {
			continue
		}
		// 1970-01-01 was a Thursday (weekday 4).
		key := [2]int{int((local/86400 + 4) % 7), int(local % 86400 / 3600)}
		c := cells[key]
		if c == nil {
			c = &HeatCell{Weekday: key[0], Hour: key[1]}
			cells[key] = c
		}
		c.Requests += a.Requests
		c.Failed += a.Failed
		c.Tokens += a.Tokens.Total
		c.Cost += a.Cost
	}
	if errRows := hrows.Err(); errRows != nil {
		return nil, errRows
	}
	for _, c := range cells {
		out.Heatmap = append(out.Heatmap, *c)
	}
	sort.Slice(out.Heatmap, func(i, j int) bool {
		if out.Heatmap[i].Weekday != out.Heatmap[j].Weekday {
			return out.Heatmap[i].Weekday < out.Heatmap[j].Weekday
		}
		return out.Heatmap[i].Hour < out.Heatmap[j].Hour
	})
	return out, nil
}
