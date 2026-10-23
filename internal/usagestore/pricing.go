package usagestore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpfetch"
)

const (
	modelsDevURL     = "https://models.dev/api.json"
	modelsDevMaxSize = 64 << 20
	priceRefresh     = 24 * time.Hour
)

// Price is a model price in USD per million tokens.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	// Reasoning bills reasoning tokens when positive; otherwise they use Output.
	Reasoning float64 `json:"reasoning,omitempty"`
	// Tiers holds service-tier prices keyed by service tier (priority, flex).
	Tiers map[string]Price `json:"tiers,omitempty"`
}

// Cost returns the estimated USD cost of t. Unclassified tokens are billed at the input price.
// shortcut: long-context tiers (e.g. context_over_200k) are ignored; add per-event pricing if they matter.
func (p Price) Cost(t Tokens) float64 {
	reasoning := p.Reasoning
	if reasoning <= 0 {
		reasoning = p.Output
	}
	return (float64(t.InputUncached+t.Unclassified)*p.Input +
		float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheWrite)*p.CacheWrite +
		float64(t.Output-t.Reasoning)*p.Output +
		float64(t.Reasoning)*reasoning) / 1e6
}

func (p Price) forTier(tier string) Price {
	if tp, ok := p.Tiers[strings.ToLower(tier)]; ok {
		return tp
	}
	return p
}

// Pricer resolves model prices from config overrides and models.dev.
type Pricer struct {
	mu        sync.RWMutex
	overrides map[string]Price
	modelsDev map[string]Price
	updatedAt time.Time
	disabled  bool
	cachePath string
	lastError string
}

// NewPricer returns a Pricer that caches models.dev prices at cachePath.
func NewPricer(cachePath string) *Pricer {
	p := &Pricer{cachePath: cachePath, overrides: map[string]Price{}}
	p.loadCache()
	return p
}

// Configure applies config overrides and the models.dev switch.
func (p *Pricer) Configure(cfg config.UsagePricingConfig) {
	overrides := make(map[string]Price, len(cfg.Overrides))
	for _, o := range cfg.Overrides {
		name := strings.ToLower(strings.TrimSpace(o.Model))
		if name == "" {
			continue
		}
		price := Price{Input: o.Input, Output: o.Output, CacheRead: o.Input, CacheWrite: o.Input}
		if o.CacheRead != nil {
			price.CacheRead = *o.CacheRead
		}
		if o.CacheWrite != nil {
			price.CacheWrite = *o.CacheWrite
		}
		overrides[name] = price
	}
	p.mu.Lock()
	p.overrides = overrides
	p.disabled = cfg.DisableModelsDev
	p.mu.Unlock()
}

// Lookup returns the price for model under serviceTier and its source: "override", "models.dev" or "".
func (p *Pricer) Lookup(model, serviceTier string) (Price, string, bool) {
	if p == nil {
		return Price{}, "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := candidateKeys(model)
	for _, k := range keys {
		if price, ok := p.overrides[k]; ok {
			return price, "override", true
		}
	}
	if p.disabled {
		return Price{}, "", false
	}
	for _, k := range keys {
		if price, ok := p.modelsDev[k]; ok {
			return price.forTier(serviceTier), "models.dev", true
		}
	}
	return Price{}, "", false
}

// Status reports when models.dev prices were last loaded and the last refresh error.
func (p *Pricer) Status() (updatedAt time.Time, enabled bool, count int, lastError string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.updatedAt, !p.disabled, len(p.modelsDev), p.lastError
}

func (p *Pricer) stale() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.disabled && time.Since(p.updatedAt) >= priceRefresh
}

// Refresh downloads models.dev prices and caches them.
func (p *Pricer) Refresh(ctx context.Context, client httpfetch.Doer) error {
	p.mu.RLock()
	disabled := p.disabled
	p.mu.RUnlock()
	if disabled {
		return fmt.Errorf("models.dev pricing is disabled")
	}
	data, err := httpfetch.GetBytes(ctx, client, modelsDevURL, map[string]string{"Accept": "application/json"}, modelsDevMaxSize)
	if err == nil {
		var prices map[string]Price
		if prices, err = parseModelsDev(data); err == nil {
			p.mu.Lock()
			p.modelsDev, p.updatedAt, p.lastError = prices, time.Now(), ""
			p.mu.Unlock()
			p.saveCache(prices)
			return nil
		}
	}
	p.mu.Lock()
	p.lastError = err.Error()
	p.mu.Unlock()
	return err
}

type priceCache struct {
	UpdatedAt time.Time        `json:"updated_at"`
	Prices    map[string]Price `json:"prices"`
}

func (p *Pricer) loadCache() {
	if p.cachePath == "" {
		return
	}
	data, err := os.ReadFile(p.cachePath)
	if err != nil {
		return
	}
	var c priceCache
	if json.Unmarshal(data, &c) == nil && len(c.Prices) > 0 {
		p.modelsDev, p.updatedAt = c.Prices, c.UpdatedAt
	}
}

func (p *Pricer) saveCache(prices map[string]Price) {
	if p.cachePath == "" {
		return
	}
	p.mu.RLock()
	data, err := json.Marshal(priceCache{UpdatedAt: p.updatedAt, Prices: prices})
	p.mu.RUnlock()
	if err != nil {
		return
	}
	tmp := p.cachePath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p.cachePath)
	}
}

// Vendors whose own listing wins over resellers that mirror the same model id.
var preferredProviders = []string{"openai", "anthropic", "google", "xai", "deepseek", "moonshotai", "zhipuai", "alibaba", "mistral", "minimax", "meta"}

type mdCost struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	Reasoning  float64  `json:"reasoning"`
}

func (c *mdCost) price() Price {
	p := Price{Input: c.Input, Output: c.Output, CacheRead: c.Input, CacheWrite: c.Input, Reasoning: c.Reasoning}
	if c.CacheRead != nil {
		p.CacheRead = *c.CacheRead
	}
	if c.CacheWrite != nil {
		p.CacheWrite = *c.CacheWrite
	}
	return p
}

type mdModel struct {
	Cost         *mdCost `json:"cost"`
	Experimental struct {
		Modes map[string]struct {
			Cost     *mdCost `json:"cost"`
			Provider struct {
				Body struct {
					ServiceTier string `json:"service_tier"`
				} `json:"body"`
			} `json:"provider"`
		} `json:"modes"`
	} `json:"experimental"`
}

func parseModelsDev(data []byte) (map[string]Price, error) {
	var providers map[string]struct {
		Models map[string]mdModel `json:"models"`
	}
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("parse models.dev: %w", err)
	}
	order := append([]string(nil), preferredProviders...)
	rest := make([]string, 0, len(providers))
	for id := range providers {
		rest = append(rest, id)
	}
	sort.Strings(rest)
	order = append(order, rest...)
	out := make(map[string]Price)
	for _, id := range order {
		for modelID, m := range providers[id].Models {
			key := strings.ToLower(modelID)
			if _, exists := out[key]; exists || m.Cost == nil {
				continue
			}
			price := m.Cost.price()
			for _, mode := range m.Experimental.Modes {
				tier := strings.ToLower(mode.Provider.Body.ServiceTier)
				if tier == "" || mode.Cost == nil {
					continue
				}
				if price.Tiers == nil {
					price.Tiers = map[string]Price{}
				}
				price.Tiers[tier] = mode.Cost.price()
			}
			out[key] = price
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("parse models.dev: no priced models")
	}
	return out, nil
}

var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// candidateKeys lists lookup keys from most to least specific.
func candidateKeys(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return nil
	}
	keys := []string{m}
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range keys {
			if existing == k {
				return
			}
		}
		keys = append(keys, k)
	}
	// Thinking suffixes such as "model(high)" do not change the price.
	if i := strings.IndexByte(m, '('); i > 0 {
		m = m[:i]
		add(m)
	}
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
		add(m)
	}
	m = strings.TrimSuffix(m, "-latest")
	add(m)
	add(dateSuffix.ReplaceAllString(m, ""))
	return keys
}
