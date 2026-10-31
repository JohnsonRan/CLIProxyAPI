package usagestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const maxErrorBytes = 512

var (
	applyMu  sync.Mutex
	current  atomic.Pointer[Store]
	proxyURL atomic.Value // string
	// excludeEmbeddings mirrors observability.usage.exclude-embeddings, which the usage queue also honours.
	excludeEmbeddings atomic.Bool
)

func init() {
	coreusage.RegisterPlugin(plugin{})
}

// Current returns the active store, or nil when the store is disabled.
func Current() *Store { return current.Load() }

// Apply opens, reconfigures or closes the store to match cfg.
func Apply(cfg *config.Config) {
	applyMu.Lock()
	defer applyMu.Unlock()
	// Home mode keeps every record, as it does for the usage queue.
	excludeEmbeddings.Store(cfg != nil && cfg.UsageExcludeEmbeddings && !cfg.Home.Enabled)
	if cfg == nil || !cfg.UsageStore.Enable {
		closeLocked()
		return
	}
	proxyURL.Store(strings.TrimSpace(cfg.ProxyURL))
	sc := cfg.UsageStore
	path := resolvePath(cfg)
	s := current.Load()
	if s != nil && s.path != path {
		closeLocked()
		s = nil
	}
	if s != nil {
		s.SetRetentionDays(sc.RetentionDays)
		riskPromptsOff.Store(sc.RiskPrompts.Disable)
		coreusage.SetRequestPayloadCapture(!sc.RiskPrompts.Disable)
		s.pricer.Configure(sc.Pricing)
		if s.geo != nil {
			s.geo.Configure(sc.GeoIP)
		}
		return
	}
	pricer := NewPricer(filepath.Join(filepath.Dir(path), "models-dev-prices.json"))
	pricer.Configure(sc.Pricing)
	opened, err := Open(path, sc.RetentionDays, pricer)
	if err != nil {
		coreusage.SetRequestPayloadCapture(false)
		log.Errorf("usage store disabled: %v", err)
		return
	}
	if opened.geo != nil {
		opened.geo.Configure(sc.GeoIP)
	}
	riskPromptsOff.Store(sc.RiskPrompts.Disable)
	coreusage.SetRequestPayloadCapture(!sc.RiskPrompts.Disable)
	current.Store(opened)
	go opened.refreshPrices()
	opened.startResolver()
	opened.startQuotaSampler()
	opened.startQuotaProber()
	log.Infof("usage store enabled: %s", path)
}

// Shutdown flushes and closes the active store.
func Shutdown() {
	applyMu.Lock()
	defer applyMu.Unlock()
	closeLocked()
}

func closeLocked() {
	coreusage.SetRequestPayloadCapture(false)
	riskPromptsOff.Store(true)
	if s := current.Swap(nil); s != nil {
		s.Close()
		log.Info("usage store closed")
	}
}

func resolvePath(cfg *config.Config) string {
	if p := strings.TrimSpace(cfg.UsageStore.Path); p != "" {
		return filepath.Clean(p)
	}
	return filepath.Join(internallogging.ResolveLogDirectory(cfg), "usage.db")
}

// RefreshPrices downloads models.dev prices now.
func (s *Store) RefreshPrices(ctx context.Context) error {
	client := &http.Client{}
	proxy, _ := proxyURL.Load().(string)
	util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: proxy}, client)
	return s.pricer.Refresh(ctx, client)
}

func (s *Store) resolveRound() {
	client := &http.Client{}
	proxy, _ := proxyURL.Load().(string)
	util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: proxy}, client)
	if _, err := s.geo.Resolve(s.ctx, client); err != nil && !errors.Is(err, ErrGeoIPDisabled) && s.ctx.Err() == nil {
		log.Warnf("usage store: look up client ips: %v", err)
	}
}

// Pricer returns the store's price resolver.
func (s *Store) Pricer() *Pricer { return s.pricer }

func (s *Store) refreshPrices() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if s.pricer.stale() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := s.RefreshPrices(ctx); err != nil {
				log.Warnf("usage store: refresh models.dev prices: %v", err)
			}
			cancel()
		}
		select {
		case <-ticker.C:
		case <-s.stop:
			return
		}
	}
}

// startResolver runs the background lookup loop once per store; Close waits for it.
func (s *Store) startResolver() {
	s.resolver.Do(func() {
		s.lookups.Add(1)
		go func() {
			defer s.lookups.Done()
			s.resolveClientIPs()
		}()
	})
}

// resolveClientIPs drains pending client IPs (paced by GeoIP.Resolve) each minute, or sooner when
// the writer registers a new public address, until the store closes.
func (s *Store) resolveClientIPs() {
	ticker := time.NewTicker(geoIPRoundEvery)
	defer ticker.Stop()
	for {
		if s.ctx.Err() != nil {
			return
		}
		if !s.geo.isDisabled() {
			s.resolveRound()
		}
		select {
		case <-ticker.C:
		case <-s.geo.kick:
		case <-s.ctx.Done():
			return
		}
	}
}

type plugin struct{}

func (plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	s := current.Load()
	if s == nil {
		return
	}
	if excludeEmbeddings.Load() && coreusage.RequestKindFromContext(ctx) == coreusage.RequestKindEmbeddings {
		return
	}
	s.Enqueue(eventFromRecord(ctx, record))
}

func eventFromRecord(ctx context.Context, record coreusage.Record) Event {
	ts := record.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	failed := record.Failed
	status := record.Fail.StatusCode
	if respStatus := internallogging.GetResponseStatus(ctx); respStatus >= 400 {
		failed = true
		if status <= 0 {
			status = respStatus
		}
	}
	if failed && status <= 0 {
		status = 500
	}
	if !failed {
		status = 200
	}
	errBody := ""
	if failed {
		errBody = truncate(strings.TrimSpace(record.Fail.Body), maxErrorBytes)
	}

	apiKey := strings.TrimSpace(record.APIKey)
	apiKeyID := ""
	if apiKey != "" {
		sum := sha256.Sum256([]byte(apiKey))
		apiKeyID = hex.EncodeToString(sum[:6])
	}
	accessProvider := strings.TrimSpace(record.AccessProvider)
	if accessProvider == "" {
		accessProvider = strings.TrimSpace(coreusage.AccessProviderFromContext(ctx))
	}
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	tier := firstNonEmpty(record.EffectiveServiceTier, record.ResponseServiceTier, record.ServiceTier,
		record.RequestServiceTier, coreusage.ServiceTierFromContext(ctx))
	meta := internallogging.GetClientRequestMetadata(ctx)
	source := strings.TrimSpace(record.Source)
	if strings.EqualFold(strings.TrimSpace(record.AuthType), "apikey") ||
		strings.EqualFold(strings.TrimSpace(record.AuthType), "api_key") ||
		(apiKey != "" && source == apiKey) {
		// API-key credentials and client-key fallbacks report keys as their source.
		source = util.HideAPIKey(source)
	}

	b := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown

	served := strings.TrimSpace(record.ResponseModel)
	expectedModel := strings.TrimSpace(record.UpstreamModel)
	if expectedModel == "" {
		expectedModel = record.Model
	}
	substituted := served != "" && helps.IsModelSubstituted(expectedModel, served) && (record.Model == "" || helps.IsModelSubstituted(record.Model, served))

	e := Event{
		Timestamp:       ts.UnixMilli(),
		RequestID:       strings.TrimSpace(internallogging.GetRequestID(ctx)),
		Provider:        orUnknown(record.Provider),
		ExecutorType:    strings.TrimSpace(record.ExecutorType),
		Model:           orUnknown(record.Model),
		Alias:           strings.TrimSpace(record.Alias),
		Endpoint:        strings.TrimSpace(internallogging.GetEndpoint(ctx)),
		AuthType:        strings.TrimSpace(record.AuthType),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		Source:          source,
		APIKeyID:        apiKeyID,
		APIKey:          util.HideAPIKey(apiKey),
		AccessProvider:  accessProvider,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     strings.ToLower(tier),
		Stream:          record.Stream || coreusage.StreamFromContext(ctx),
		Failed:          failed,
		StatusCode:      status,
		Error:           errBody,
		LatencyMs:       record.Latency.Milliseconds(),
		TTFTMs:          record.TTFT.Milliseconds(),
		Tokens: Tokens{
			InputUncached: b.Input.UncachedTokens,
			CacheRead:     b.Input.CacheReadTokens,
			CacheWrite:    b.Input.CacheWriteTokens,
			Output:        b.Output.TotalTokens,
			Reasoning:     b.Output.ReasoningTokens,
			Unclassified:  b.UnclassifiedTokens,
			Total:         b.TotalTokens,
		},
		ClientIP:      firstNonEmpty(meta.ResolvedClientIP, meta.ClientIP),
		UserAgent:     truncate(meta.UserAgent, 256),
		ResponseModel: served,
		Transport:     transportOf(meta.WebSocket, record.Stream || coreusage.StreamFromContext(ctx)),
		Substituted:   substituted,
	}
	e.Risk, e.RiskReason = classifyRisk(record, substituted)
	if e.Risk != "" && !riskPromptsOff.Load() {
		e.RiskPrompt = turnPrompt(coreusage.RequestPayloadFromContext(ctx))
	}
	return e
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func orUnknown(v string) string {
	if v = strings.TrimSpace(v); v == "" {
		return "unknown"
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// transportOf names the downstream transport recorded with an event.
func transportOf(webSocket, stream bool) string {
	switch {
	case webSocket:
		return "ws"
	case stream:
		return "sse"
	default:
		return "http"
	}
}
