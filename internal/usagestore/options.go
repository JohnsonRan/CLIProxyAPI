package usagestore

import (
	"context"
	"time"
)

// ModelOption represents a model observed in usage in the time range.
type ModelOption struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
}

// ProviderOption represents a provider observed in usage in the time range.
type ProviderOption struct {
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
}

// APIKeyOption represents an API key observed in usage in the time range.
type APIKeyOption struct {
	APIKeyID string `json:"api_key_id"`
	APIKey   string `json:"api_key"`
	Requests int64  `json:"requests"`
}

// CredentialOption represents a credential observed in usage in the time range.
type CredentialOption struct {
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	AuthType  string `json:"auth_type"`
	Source    string `json:"source"`
	Requests  int64  `json:"requests"`
}

// Options contains distinct dimension values observed in the given time range,
// sorted by request count descending.
type Options struct {
	Models      []ModelOption      `json:"models"`
	Providers   []ProviderOption   `json:"providers"`
	APIKeys     []APIKeyOption     `json:"api_keys"`
	Credentials []CredentialOption `json:"credentials"`
}

// Options queries distinct dimension values from hourly in [from, to), sorted by requests desc.
// Credentials receive metadata from the credentials table.
func (s *Store) Options(ctx context.Context, from, to time.Time) (*Options, error) {
	fromSec := from.Unix() / 3600 * 3600
	toSec := to.Unix()
	out := &Options{
		Models:      make([]ModelOption, 0),
		Providers:   make([]ProviderOption, 0),
		APIKeys:     make([]APIKeyOption, 0),
		Credentials: make([]CredentialOption, 0),
	}

	mRows, err := s.db.QueryContext(ctx, `SELECT model, provider, SUM(requests) AS reqs
		FROM hourly WHERE hour >= ? AND hour < ? AND model != ''
		GROUP BY model, provider ORDER BY reqs DESC`, fromSec, toSec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mRows.Close() }()
	for mRows.Next() {
		var m ModelOption
		if errScan := mRows.Scan(&m.Model, &m.Provider, &m.Requests); errScan != nil {
			return nil, errScan
		}
		out.Models = append(out.Models, m)
	}
	if err = mRows.Err(); err != nil {
		return nil, err
	}

	pRows, err := s.db.QueryContext(ctx, `SELECT provider, SUM(requests) AS reqs
		FROM hourly WHERE hour >= ? AND hour < ? AND provider != ''
		GROUP BY provider ORDER BY reqs DESC`, fromSec, toSec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pRows.Close() }()
	for pRows.Next() {
		var p ProviderOption
		if errScan := pRows.Scan(&p.Provider, &p.Requests); errScan != nil {
			return nil, errScan
		}
		out.Providers = append(out.Providers, p)
	}
	if err = pRows.Err(); err != nil {
		return nil, err
	}

	kRows, err := s.db.QueryContext(ctx, `SELECT api_key_id, MAX(api_key), SUM(requests) AS reqs
		FROM hourly WHERE hour >= ? AND hour < ? AND api_key_id != ''
		GROUP BY api_key_id ORDER BY reqs DESC`, fromSec, toSec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = kRows.Close() }()
	for kRows.Next() {
		var k APIKeyOption
		if errScan := kRows.Scan(&k.APIKeyID, &k.APIKey, &k.Requests); errScan != nil {
			return nil, errScan
		}
		out.APIKeys = append(out.APIKeys, k)
	}
	if err = kRows.Err(); err != nil {
		return nil, err
	}

	cRows, err := s.db.QueryContext(ctx, `SELECT auth_index, MAX(provider), SUM(requests) AS reqs
		FROM hourly WHERE hour >= ? AND hour < ? AND auth_index != ''
		GROUP BY auth_index ORDER BY reqs DESC`, fromSec, toSec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cRows.Close() }()
	for cRows.Next() {
		var c CredentialOption
		if errScan := cRows.Scan(&c.AuthIndex, &c.Provider, &c.Requests); errScan != nil {
			return nil, errScan
		}
		out.Credentials = append(out.Credentials, c)
	}
	if err = cRows.Err(); err != nil {
		return nil, err
	}

	if len(out.Credentials) > 0 {
		labelRows, errLabels := s.db.QueryContext(ctx, `SELECT auth_index, provider, auth_type, source FROM credentials`)
		if errLabels == nil {
			defer func() { _ = labelRows.Close() }()
			type credMeta struct{ provider, authType, source string }
			metaMap := make(map[string]credMeta, len(out.Credentials))
			for labelRows.Next() {
				var idx, prov, aType, src string
				if labelRows.Scan(&idx, &prov, &aType, &src) == nil {
					metaMap[idx] = credMeta{provider: prov, authType: aType, source: src}
				}
			}
			for i := range out.Credentials {
				if meta, ok := metaMap[out.Credentials[i].AuthIndex]; ok {
					out.Credentials[i].AuthType = meta.authType
					out.Credentials[i].Source = meta.source
					if out.Credentials[i].Provider == "" {
						out.Credentials[i].Provider = meta.provider
					}
				}
			}
		}
	}

	return out, nil
}
