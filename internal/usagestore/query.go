package usagestore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxHourBuckets = 31 * 24

// Aggregate sums usage over a set of requests.
type Aggregate struct {
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	Substituted      int64   `json:"substituted"`
	Tokens           Tokens  `json:"tokens"`
	Cost             float64 `json:"cost"`
	UnpricedRequests int64   `json:"unpriced_requests"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	AvgTTFTMs        float64 `json:"avg_ttft_ms"`
	latencySum       int64
	ttftSum          int64
	ttftCount        int64
}

func (a *Aggregate) add(o *Aggregate) {
	a.Requests += o.Requests
	a.Failed += o.Failed
	a.Substituted += o.Substituted
	a.Tokens.add(o.Tokens)
	a.Cost += o.Cost
	a.UnpricedRequests += o.UnpricedRequests
	a.latencySum += o.latencySum
	a.ttftSum += o.ttftSum
	a.ttftCount += o.ttftCount
}

func (a *Aggregate) finish() {
	if a.Requests > 0 {
		a.AvgLatencyMs = float64(a.latencySum) / float64(a.Requests)
	}
	if a.ttftCount > 0 {
		a.AvgTTFTMs = float64(a.ttftSum) / float64(a.ttftCount)
	}
}

// SeriesPoint is one time bucket; Time is the bucket start in unix seconds.
type SeriesPoint struct {
	Time int64 `json:"time"`
	Aggregate
}

// ModelStat aggregates one model of one provider.
type ModelStat struct {
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	PriceSource string `json:"price_source"`
	Aggregate
}

// APIKeyStat aggregates one client API key.
type APIKeyStat struct {
	APIKeyID string `json:"api_key_id"`
	APIKey   string `json:"api_key"`
	Aggregate
}

// CredentialStat aggregates one upstream credential.
type CredentialStat struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	// AuthType and Source (masked for API keys) come from the latest request on this credential.
	AuthType string `json:"auth_type"`
	Source   string `json:"source"`
	Aggregate
}

// Stats is the dashboard payload for a time range.
type Stats struct {
	From        int64            `json:"from"`
	To          int64            `json:"to"`
	Bucket      string           `json:"bucket"`
	Totals      Aggregate        `json:"totals"`
	Series      []SeriesPoint    `json:"series"`
	Models      []ModelStat      `json:"models"`
	APIKeys     []APIKeyStat     `json:"api_keys"`
	Credentials []CredentialStat `json:"credentials"`
}

// StatsFilter narrows Stats to models, providers, client keys or credentials. Zero values do not filter.
type StatsFilter struct {
	Model     []string
	Provider  []string
	APIKeyID  []string
	AuthIndex []string
}

// Stats aggregates hourly rollups in [from, to). bucket is "hour" or "day"
// (empty picks hour up to 48h); tzOffsetMinutes aligns day buckets to local midnight.
// shortcut: rollups are hourly, so half-hour time zones get day buckets shifted by 30 minutes.
func (s *Store) Stats(ctx context.Context, from, to time.Time, bucket string, tzOffsetMinutes int, filter StatsFilter) (*Stats, error) {
	fromSec := from.Unix() / 3600 * 3600
	toSec := to.Unix()
	if bucket != "hour" && bucket != "day" {
		bucket = "day"
		if toSec-fromSec <= 48*3600 {
			bucket = "hour"
		}
	}
	if bucket == "hour" && (toSec-fromSec)/3600 > maxHourBuckets {
		bucket = "day"
	}
	off := int64(tzOffsetMinutes) * 60
	bucketExpr := "hour"
	if bucket == "day" {
		bucketExpr = "((hour + ?) / 86400) * 86400 - ?"
	}
	query := `SELECT ` + bucketExpr + ` AS b, provider, model, api_key_id, MAX(api_key), auth_index, service_tier,
		SUM(requests), SUM(failed), SUM(substituted), SUM(input_uncached), SUM(cache_read), SUM(cache_write), SUM(output),
		SUM(reasoning), SUM(unclassified), SUM(total), SUM(latency_ms), SUM(ttft_ms), SUM(ttft_count)
		FROM hourly WHERE hour >= ? AND hour < ?`
	args := []any{fromSec, toSec}
	if bucket == "day" {
		args = []any{off, off, fromSec, toSec}
	}
	for _, f := range []struct {
		column string
		values []string
	}{
		{"model", filter.Model},
		{"provider", filter.Provider},
		{"api_key_id", filter.APIKeyID},
		{"auth_index", filter.AuthIndex},
	} {
		if len(f.values) > 0 {
			placeholders := make([]string, len(f.values))
			for i, v := range f.values {
				placeholders[i] = "?"
				args = append(args, v)
			}
			query += " AND " + f.column + " IN (" + strings.Join(placeholders, ",") + ")"
		}
	}
	query += " GROUP BY b, provider, model, api_key_id, auth_index, service_tier"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := &Stats{From: fromSec, To: toSec, Bucket: bucket, Series: []SeriesPoint{},
		Models: []ModelStat{}, APIKeys: []APIKeyStat{}, Credentials: []CredentialStat{}}
	series := map[int64]*Aggregate{}
	models := map[[2]string]*ModelStat{}
	keys := map[string]*APIKeyStat{}
	creds := map[string]*CredentialStat{}
	prices := s.priceCache()
	for rows.Next() {
		var b int64
		var provider, model, keyID, key, authIndex, tier string
		var a Aggregate
		t := &a.Tokens
		if errScan := rows.Scan(&b, &provider, &model, &keyID, &key, &authIndex, &tier, &a.Requests, &a.Failed,
			&a.Substituted, &t.InputUncached, &t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified, &t.Total,
			&a.latencySum, &a.ttftSum, &a.ttftCount); errScan != nil {
			return nil, errScan
		}
		source := prices.apply(&a, model, tier)

		out.Totals.add(&a)
		if series[b] == nil {
			series[b] = &Aggregate{}
		}
		series[b].add(&a)
		mk := [2]string{model, provider}
		if models[mk] == nil {
			models[mk] = &ModelStat{Model: model, Provider: provider, PriceSource: source}
		}
		models[mk].add(&a)
		if keys[keyID] == nil {
			keys[keyID] = &APIKeyStat{APIKeyID: keyID, APIKey: key}
		}
		keys[keyID].add(&a)
		if creds[authIndex] == nil {
			creds[authIndex] = &CredentialStat{AuthIndex: authIndex, Provider: provider}
		}
		creds[authIndex].add(&a)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, errRows
	}

	out.Totals.finish()
	step := int64(3600)
	start := fromSec
	if bucket == "day" {
		step = 86400
		start = ((fromSec+off)/86400)*86400 - off
	}
	for t := start; t < toSec; t += step {
		p := SeriesPoint{Time: t}
		if a := series[t]; a != nil {
			p.Aggregate = *a
		}
		p.finish()
		out.Series = append(out.Series, p)
	}
	for _, m := range models {
		m.finish()
		out.Models = append(out.Models, *m)
	}
	for _, k := range keys {
		k.finish()
		out.APIKeys = append(out.APIKeys, *k)
	}
	if len(creds) > 0 {
		if errLabels := s.credentialLabels(ctx, creds); errLabels != nil {
			return nil, errLabels
		}
	}
	for _, c := range creds {
		c.finish()
		out.Credentials = append(out.Credentials, *c)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Requests > out.Models[j].Requests })
	sort.Slice(out.APIKeys, func(i, j int) bool { return out.APIKeys[i].Requests > out.APIKeys[j].Requests })
	sort.Slice(out.Credentials, func(i, j int) bool { return out.Credentials[i].Requests > out.Credentials[j].Requests })
	return out, nil
}

func (s *Store) credentialLabels(ctx context.Context, creds map[string]*CredentialStat) error {
	rows, err := s.db.QueryContext(ctx, `SELECT auth_index, auth_type, source FROM credentials`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var authIndex, authType, source string
		if errScan := rows.Scan(&authIndex, &authType, &source); errScan != nil {
			return errScan
		}
		if c := creds[authIndex]; c != nil {
			c.AuthType, c.Source = authType, source
		}
	}
	return rows.Err()
}

// EventFilter selects stored events. Zero values do not filter.
type EventFilter struct {
	From, To    time.Time
	Model       []string
	Provider    []string
	APIKeyID    []string
	AuthIndex   []string
	Failed      *bool
	Substituted *bool
	BeforeID    int64
	AfterID     int64
	Limit       int
}

// MaxExportEvents caps one export so a wide range cannot hold the database for long.
const MaxExportEvents = 100_000

func eventQuery(f EventFilter, limit int) (string, []any) {
	var where []string
	var args []any
	cond := func(clause string, v any) {
		where = append(where, clause)
		args = append(args, v)
	}
	if !f.From.IsZero() {
		cond("ts >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		cond("ts < ?", f.To.UnixMilli())
	}
	for _, dim := range []struct {
		column string
		values []string
	}{
		{"model", f.Model},
		{"provider", f.Provider},
		{"api_key_id", f.APIKeyID},
		{"auth_index", f.AuthIndex},
	} {
		if len(dim.values) > 0 {
			placeholders := make([]string, len(dim.values))
			for i, v := range dim.values {
				placeholders[i] = "?"
				args = append(args, v)
			}
			where = append(where, fmt.Sprintf("%s IN (%s)", dim.column, strings.Join(placeholders, ",")))
		}
	}
	if f.Failed != nil {
		cond("failed = ?", *f.Failed)
	}
	if f.Substituted != nil {
		cond("substituted = ?", *f.Substituted)
	}
	if f.BeforeID > 0 {
		cond("id < ?", f.BeforeID)
	}
	if f.AfterID > 0 {
		cond("id > ?", f.AfterID)
	}
	query := `SELECT id, ts, request_id, provider, executor_type, model, alias, endpoint, auth_type, auth_index, source,
		api_key_id, api_key, access_provider, reasoning_effort, service_tier, stream, failed, status_code, error,
		latency_ms, ttft_ms, input_uncached, cache_read, cache_write, output, reasoning, unclassified, total,
		client_ip, user_agent, response_model, substituted, transport FROM events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	order := "DESC"
	if f.AfterID > 0 {
		order = "ASC"
	}
	query += " ORDER BY id " + order + " LIMIT ?"
	return query, append(args, limit)
}

// eachEvent runs the filtered query and calls fn for every priced event.
func (s *Store) eachEvent(ctx context.Context, f EventFilter, limit int, fn func(*Event) error) error {
	query, args := eventQuery(f, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	prices := s.priceCache()
	for rows.Next() {
		var e Event
		t := &e.Tokens
		if errScan := rows.Scan(&e.ID, &e.Timestamp, &e.RequestID, &e.Provider, &e.ExecutorType, &e.Model, &e.Alias,
			&e.Endpoint, &e.AuthType, &e.AuthIndex, &e.Source, &e.APIKeyID, &e.APIKey, &e.AccessProvider,
			&e.ReasoningEffort, &e.ServiceTier, &e.Stream, &e.Failed, &e.StatusCode, &e.Error, &e.LatencyMs, &e.TTFTMs,
			&t.InputUncached, &t.CacheRead, &t.CacheWrite, &t.Output, &t.Reasoning, &t.Unclassified, &t.Total,
			&e.ClientIP, &e.UserAgent, &e.ResponseModel, &e.Substituted, &e.Transport); errScan != nil {
			return errScan
		}
		var a Aggregate
		a.Requests, a.Tokens = 1, e.Tokens
		if prices.apply(&a, e.Model, e.ServiceTier) != "" {
			e.Cost, e.Priced = a.Cost, true
		}
		if errFn := fn(&e); errFn != nil {
			return errFn
		}
	}
	return rows.Err()
}

// Events returns events newest first (or oldest first when AfterID > 0), paginated by BeforeID/AfterID.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	events := make([]Event, 0)
	err := s.eachEvent(ctx, f, f.Limit, func(e *Event) error {
		events = append(events, *e)
		return nil
	})
	return events, err
}

// ExportEvents streams up to MaxExportEvents matching events newest first.
func (s *Store) ExportEvents(ctx context.Context, f EventFilter, fn func(*Event) error) error {
	return s.eachEvent(ctx, f, MaxExportEvents, fn)
}

// ModelPrice is the effective price of a model seen in the store.
type ModelPrice struct {
	Model  string `json:"model"`
	Source string `json:"source"`
	Price  *Price `json:"price"`
}

// ModelPrices lists every stored model with its effective price.
func (s *Store) ModelPrices(ctx context.Context) ([]ModelPrice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM hourly ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ModelPrice{}
	for rows.Next() {
		var model string
		if errScan := rows.Scan(&model); errScan != nil {
			return nil, errScan
		}
		mp := ModelPrice{Model: model}
		if price, source, ok := s.pricer.Lookup(model, ""); ok {
			mp.Source, mp.Price = source, &price
		}
		out = append(out, mp)
	}
	return out, rows.Err()
}

type priceKey struct{ model, tier string }

type priceEntry struct {
	price  Price
	source string
	ok     bool
}

type queryPrices struct {
	pricer *Pricer
	cache  map[priceKey]priceEntry
}

func (s *Store) priceCache() *queryPrices {
	return &queryPrices{pricer: s.pricer, cache: map[priceKey]priceEntry{}}
}

// apply adds the cost of a to it and returns the price source.
func (q *queryPrices) apply(a *Aggregate, model, tier string) string {
	k := priceKey{model, tier}
	e, seen := q.cache[k]
	if !seen {
		e.price, e.source, e.ok = q.pricer.Lookup(model, tier)
		q.cache[k] = e
	}
	if e.ok {
		a.Cost = e.price.Cost(a.Tokens)
	} else {
		a.UnpricedRequests = a.Requests
	}
	return e.source
}
