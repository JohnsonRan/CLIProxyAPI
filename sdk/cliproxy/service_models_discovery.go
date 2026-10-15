package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// compatDiscoveryTTL bounds how often a provider's /models endpoint is polled.
	compatDiscoveryTTL = 30 * time.Minute

	// compatDiscoveryTimeout bounds the discovery request through its context.
	// It is a metadata call, not an upstream conversation, like the remote model
	// updater in internal/registry; the bound keeps a hung provider from pinning
	// a registration or a background refresh.
	compatDiscoveryTimeout = 30 * time.Second

	// compatDiscoveryMaxBody caps the response body read from a provider.
	compatDiscoveryMaxBody = 8 << 20
)

// compatDiscoveryNow is overridable in tests to avoid wall-clock dependencies.
var compatDiscoveryNow = time.Now

// compatDiscoveryEntry holds the last successful discovery result for a provider.
type compatDiscoveryEntry struct {
	modelIDs  []string
	fetchedAt time.Time
}

// compatDiscoveryAfterRefresh, when set by tests, runs after each background
// refresh finishes.
var compatDiscoveryAfterRefresh func()

// compatDiscoveryState caches discovered model IDs keyed by provider, base URL
// and request inputs, and tracks in-flight background refreshes.
type compatDiscoveryState struct {
	mu         sync.Mutex
	entries    map[string]*compatDiscoveryEntry
	refreshing map[string]bool
}

// startRefresh claims the background refresh for key; false means one is
// already running.
func (s *compatDiscoveryState) startRefresh(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshing[key] {
		return false
	}
	if s.refreshing == nil {
		s.refreshing = make(map[string]bool)
	}
	s.refreshing[key] = true
	return true
}

func (s *compatDiscoveryState) finishRefresh(key string) {
	s.mu.Lock()
	delete(s.refreshing, key)
	s.mu.Unlock()
	if hook := compatDiscoveryAfterRefresh; hook != nil {
		hook()
	}
}

func (s *compatDiscoveryState) get(key string) (*compatDiscoveryEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	return entry, ok
}

func (s *compatDiscoveryState) put(key string, modelIDs []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*compatDiscoveryEntry)
	}
	s.entries[key] = &compatDiscoveryEntry{modelIDs: modelIDs, fetchedAt: at}
}

// compatDiscovery lazily initializes the per-service discovery cache.
func (s *Service) compatDiscovery() *compatDiscoveryState {
	s.compatDiscoveryOnce.Do(func() {
		s.compatDiscoveryState = &compatDiscoveryState{entries: make(map[string]*compatDiscoveryEntry)}
	})
	return s.compatDiscoveryState
}

// compatModelsWithDiscovery builds the client-visible model list for an
// OpenAI-compatible provider. Discovered models are merged underneath the
// configured ones, then exclusion patterns are applied to the result.
func (s *Service) compatModelsWithDiscovery(ctx context.Context, cfg *config.Config, compat *config.OpenAICompatibility) []*ModelInfo {
	if compat == nil {
		return nil
	}
	effective := compat
	if compat.AutoDiscoverModels {
		if modelIDs := s.discoverCompatModelIDs(ctx, cfg, compat); len(modelIDs) > 0 {
			merged := *compat
			merged.Models = mergeDiscoveredCompatModels(compat.Models, modelIDs)
			effective = &merged
		}
	}
	return applyExcludedModels(buildOpenAICompatibilityConfigModels(effective), compat.ExcludedModels)
}

// mergeDiscoveredCompatModels appends discovered model IDs that are not already
// configured. Configured entries keep priority so explicit aliases, thinking
// levels and modality metadata are never overwritten by discovery.
func mergeDiscoveredCompatModels(configured []config.OpenAICompatibilityModel, discovered []string) []config.OpenAICompatibilityModel {
	out := make([]config.OpenAICompatibilityModel, 0, len(configured)+len(discovered))
	seen := make(map[string]struct{}, len(configured)+len(discovered))
	for _, model := range configured {
		out = append(out, model)
		if name := strings.TrimSpace(model.Name); name != "" {
			seen[strings.ToLower(name)] = struct{}{}
		}
	}
	for _, id := range discovered {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[strings.ToLower(id)]; exists {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		out = append(out, config.OpenAICompatibilityModel{Name: id, Alias: id})
	}
	return out
}

// discoverCompatModelIDs returns the provider's upstream model IDs without
// holding up registration once a result is cached: a stale entry is returned
// immediately and refreshed by one background fetch per provider, which only
// updates the cache (the next registration picks it up). Only the first fetch
// for a provider runs inline. A failed fetch keeps the last known good result,
// so a transient upstream outage cannot empty the advertised model list.
func (s *Service) discoverCompatModelIDs(ctx context.Context, cfg *config.Config, compat *config.OpenAICompatibility) []string {
	cacheKey := compatDiscoveryCacheKey(compat)
	state := s.compatDiscovery()
	globalProxyURL := ""
	if cfg != nil {
		globalProxyURL = cfg.ProxyURL
	}
	if entry, ok := state.get(cacheKey); ok {
		if compatDiscoveryNow().Sub(entry.fetchedAt) >= compatDiscoveryTTL && state.startRefresh(cacheKey) {
			snapshot := *compat
			go func() {
				defer state.finishRefresh(cacheKey)
				s.fetchAndStoreCompatModelIDs(context.Background(), state, cacheKey, &snapshot, globalProxyURL)
			}()
		}
		return entry.modelIDs
	}
	return s.fetchAndStoreCompatModelIDs(ctx, state, cacheKey, compat, globalProxyURL)
}

func (s *Service) fetchAndStoreCompatModelIDs(ctx context.Context, state *compatDiscoveryState, cacheKey string, compat *config.OpenAICompatibility, globalProxyURL string) []string {
	modelIDs, errFetch := fetchOpenAICompatibilityModelIDs(ctx, compat, globalProxyURL)
	if errFetch != nil {
		if entry, ok := state.get(cacheKey); ok {
			log.Warnf("openai-compatibility model discovery failed for %q, reusing %d cached models: %v", compat.Name, len(entry.modelIDs), errFetch)
			return entry.modelIDs
		}
		log.Warnf("openai-compatibility model discovery failed for %q, no cached models available: %v", compat.Name, errFetch)
		return nil
	}
	state.put(cacheKey, modelIDs, compatDiscoveryNow())
	log.Debugf("openai-compatibility model discovery for %q returned %d models", compat.Name, len(modelIDs))
	return modelIDs
}

// compatDiscoveryCacheKey identifies what the discovery request depends on, so
// a rotated key, a new proxy or changed headers fetch a fresh list.
func compatDiscoveryCacheKey(compat *config.OpenAICompatibility) string {
	apiKey, proxyURL := firstCompatCredential(compat)
	names := make([]string, 0, len(compat.Headers))
	for name := range compat.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\x00%s", apiKey, proxyURL)
	for _, name := range names {
		_, _ = fmt.Fprintf(hash, "\x00%s=%s", name, compat.Headers[name])
	}
	return strings.ToLower(strings.TrimSpace(compat.Name)) + "|" + strings.TrimSpace(compat.BaseURL) + "|" + hex.EncodeToString(hash.Sum(nil))[:16]
}

// fetchOpenAICompatibilityModelIDs queries the provider's /models endpoint
// through the first key's proxy-url, falling back to the global proxy-url.
func fetchOpenAICompatibilityModelIDs(ctx context.Context, compat *config.OpenAICompatibility, globalProxyURL string) ([]string, error) {
	baseURL := strings.TrimSpace(compat.BaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base-url is empty")
	}
	apiKey, proxyURL := firstCompatCredential(compat)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(globalProxyURL)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, compatDiscoveryTimeout)
	defer cancel()

	endpoint := strings.TrimSuffix(baseURL, "/") + "/models"
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build discovery request: %w", errReq)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for name, value := range compat.Headers {
		// Values starting with "$" copy from a downstream client request, which
		// has no meaning for a background discovery call.
		if strings.HasPrefix(value, "$") {
			continue
		}
		req.Header.Set(name, value)
	}

	httpClient := &http.Client{}
	if proxyURL != "" {
		transport, _, errTransport := proxyutil.BuildHTTPTransport(proxyURL)
		if errTransport != nil {
			return nil, fmt.Errorf("build discovery proxy transport: %w", errTransport)
		}
		if transport != nil {
			httpClient.Transport = transport
		}
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("discovery request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("openai-compatibility discovery response close failed: %v", errClose)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("discovery returned HTTP %d", resp.StatusCode)
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, compatDiscoveryMaxBody))
	if errRead != nil {
		return nil, fmt.Errorf("read discovery response: %w", errRead)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errJSON := json.Unmarshal(body, &payload); errJSON != nil {
		return nil, fmt.Errorf("decode discovery response: %w", errJSON)
	}

	modelIDs := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, exists := seen[strings.ToLower(id)]; exists {
			continue
		}
		seen[strings.ToLower(id)] = struct{}{}
		modelIDs = append(modelIDs, id)
	}
	if len(modelIDs) == 0 {
		return nil, fmt.Errorf("discovery returned no models")
	}
	return modelIDs, nil
}

// firstCompatCredential returns the first usable API key and its proxy override.
func firstCompatCredential(compat *config.OpenAICompatibility) (apiKey, proxyURL string) {
	for i := range compat.APIKeyEntries {
		entry := &compat.APIKeyEntries[i]
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			return key, strings.TrimSpace(entry.ProxyURL)
		}
	}
	return "", ""
}
