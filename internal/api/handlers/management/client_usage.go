package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	clientUsageDefaultCacheTTL   = 5 * time.Minute
	clientUsageMinCacheTTL       = 30 * time.Second
	clientUsageProbeTimeout      = 20 * time.Second
	clientUsageProbeConcurrency  = 4
	clientUsageMaxResponseBytes  = 1 << 20
	clientUsageCredentialOK      = "ok"
	clientUsageCredentialFailure = "error"
)

// clientUsageProbeSlots bounds upstream quota probes in flight across all
// /v1/usage requests, not just within one.
var clientUsageProbeSlots = make(chan struct{}, clientUsageProbeConcurrency)

// clientUsageVersion identifies the summary contract of GET /v1/usage.
const clientUsageVersion = 2

// clientUsageUpstream is one upstream quota response. Body is the upstream
// JSON as returned, minus the account identifiers in clientUsageRedactedKeys
// and, for identity endpoints, reduced to plan fields.
type clientUsageUpstream struct {
	Endpoint   string          `json:"endpoint"`
	StatusCode int             `json:"status_code,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Error      string          `json:"error,omitempty"`

	kind     string
	optional bool
}

// clientUsageCredential is one credential's quota summary. Name is the
// credential's note, or its provider when no note is set or names are hidden.
// A relay credential (provider "relay") carries the credentials its upstream
// proxy reports in Upstream instead of windows of its own.
type clientUsageCredential struct {
	Name       string    `json:"name"`
	Provider   string    `json:"provider"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	clientUsageSummary
	Upstream  []clientUsageCredential `json:"upstream,omitempty"`
	Responses []clientUsageUpstream   `json:"responses,omitempty"`
}

type clientUsageResponse struct {
	Version     int                     `json:"version"`
	Credentials []clientUsageCredential `json:"credentials"`
}

type clientUsageProbeResult struct {
	responses  []clientUsageUpstream
	summary    clientUsageSummary
	upstream   []clientUsageCredential
	err        string
	observedAt time.Time
}

type clientUsageCacheEntry struct {
	result    clientUsageProbeResult
	expiresAt time.Time
}

// clientUsageCache reuses probe results per credential and collapses
// concurrent probes of the same credential into one upstream call.
type clientUsageCache struct {
	mu      sync.Mutex
	entries map[string]clientUsageCacheEntry
	group   singleflight.Group
	now     func() time.Time
}

func (c *clientUsageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *clientUsageCache) lookup(id string) (clientUsageProbeResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok || !c.clock().Before(entry.expiresAt) {
		return clientUsageProbeResult{}, false
	}
	return entry.result, true
}

func (c *clientUsageCache) get(id string, ttl time.Duration, probe func() clientUsageProbeResult) clientUsageProbeResult {
	if result, ok := c.lookup(id); ok {
		return result
	}
	value, _, _ := c.group.Do(id, func() (any, error) {
		if result, ok := c.lookup(id); ok {
			return result, nil
		}
		result := probe()
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[string]clientUsageCacheEntry)
		}
		c.entries[id] = clientUsageCacheEntry{result: result, expiresAt: c.clock().Add(ttl)}
		c.mu.Unlock()
		return result, nil
	})
	result, _ := value.(clientUsageProbeResult)
	return result
}

// prune drops entries of credentials that no longer exist, including their
// per-depth relay entries.
func (c *clientUsageCache) prune(keep map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		id, _, _ := strings.Cut(key, "\x00")
		if _, ok := keep[id]; !ok {
			delete(c.entries, key)
		}
	}
}

// GetClientUsage handles GET /v1/usage for client API keys. It returns the
// quota summaries of the credentials selected for the caller's key in
// quota-api.keys; a key without a selection gets an empty list. Selected
// API-key upstreams relay their own GET /v1/usage, up to clientUsageMaxDepth
// proxies deep.
func (h *Handler) GetClientUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "handler unavailable", "type": "server_error"}})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	h.mu.Unlock()
	if cfg == nil || manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "core auth manager unavailable", "type": "server_error"}})
		return
	}

	pool := clientUsageAuths(manager.List())
	keep := make(map[string]struct{}, len(pool))
	for _, auth := range pool {
		keep[auth.ID] = struct{}{}
	}
	// Prune against the whole pool so other keys' cached results survive.
	h.clientUsage.prune(keep)

	auths := clientUsageVisibleAuths(pool, cfg.QuotaAPI.VisibleCredentials(c.GetString("userApiKey")))
	if len(auths) == 0 {
		c.JSON(http.StatusOK, clientUsageResponse{Version: clientUsageVersion, Credentials: []clientUsageCredential{}})
		return
	}
	depth := clientUsageRequestDepth(c.GetHeader(clientUsageDepthHeader))
	ttl := clientUsageCacheTTL(cfg.QuotaAPI.CacheTTLSeconds)
	results := h.clientUsageResults(auths, ttl, depth, h.probeClientUsage)
	c.JSON(http.StatusOK, clientUsageResponse{
		Version:     clientUsageVersion,
		Credentials: clientUsageCredentials(auths, results, cfg.QuotaAPI.HideNames, cfg.QuotaAPI.IncludeRaw),
	})
}

// clientUsageVisibleAuths keeps the credentials selected for the caller's key,
// matched by auth ID or auth index. Unknown or unsupported entries are ignored.
func clientUsageVisibleAuths(pool []*coreauth.Auth, visible map[string]struct{}) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(visible))
	for _, auth := range pool {
		_, byID := visible[auth.ID]
		_, byIndex := visible[auth.EnsureIndex()]
		if byID || (byIndex && auth.Index != "") {
			out = append(out, auth)
		}
	}
	return out
}

func clientUsageCacheTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return clientUsageDefaultCacheTTL
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl < clientUsageMinCacheTTL {
		return clientUsageMinCacheTTL
	}
	return ttl
}

// clientUsageProvider returns the supported quota provider key, or "".
func clientUsageProvider(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if clientUsageIsRelay(auth) {
		return clientUsageRelayProvider
	}
	if auth.AuthKind() != coreauth.AuthKindOAuth {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex":
		return "codex"
	case "claude", "anthropic":
		return "claude"
	case "antigravity":
		return "antigravity"
	case "xai", "grok":
		return "xai"
	case clientUsageCommandCodeProvider:
		return clientUsageCommandCodeProvider
	default:
		return ""
	}
}

// clientUsageAuths keeps enabled OAuth credentials of supported providers and
// API-key upstreams with a base URL, ordered by provider and ID so the
// response order stays stable.
func clientUsageAuths(list []*coreauth.Auth) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(list))
	for _, auth := range list {
		if auth == nil || strings.TrimSpace(auth.ID) == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if clientUsageProvider(auth) == "" {
			continue
		}
		out = append(out, auth)
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := clientUsageProvider(out[i]), clientUsageProvider(out[j])
		if pi != pj {
			return pi < pj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// clientUsageResults probes the credentials concurrently. Results are cached
// per credential; relays are cached per request depth as well, so a request
// arriving back through a relay loop never waits on the probe that caused it.
// Relays hold no probe slot: they wait on another proxy, not on a provider.
func (h *Handler) clientUsageResults(auths []*coreauth.Auth, ttl time.Duration, depth int, probe func(*coreauth.Auth, int) clientUsageProbeResult) []clientUsageProbeResult {
	results := make([]clientUsageProbeResult, len(auths))
	var wg sync.WaitGroup
	for i, auth := range auths {
		wg.Add(1)
		go func(index int, candidate *coreauth.Auth) {
			defer wg.Done()
			key := candidate.ID
			if depth > 0 && clientUsageIsRelay(candidate) {
				key += "\x00" + strconv.Itoa(depth)
			}
			results[index] = h.clientUsage.get(key, ttl, func() clientUsageProbeResult {
				if !clientUsageIsRelay(candidate) {
					clientUsageProbeSlots <- struct{}{}
					defer func() { <-clientUsageProbeSlots }()
				}
				return probe(candidate, depth)
			})
		}(i, auth)
	}
	wg.Wait()
	return results
}

// clientUsageName is the credential's configured note, else its provider.
func clientUsageName(auth *coreauth.Auth) string {
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		return note
	}
	if note := clientUsageString(auth.Metadata, "note"); note != "" {
		return note
	}
	if clientUsageIsRelay(auth) {
		if label := strings.TrimSpace(auth.Label); label != "" {
			return label
		}
	}
	return clientUsageProvider(auth)
}

func clientUsageCredentials(auths []*coreauth.Auth, results []clientUsageProbeResult, hideNames, includeRaw bool) []clientUsageCredential {
	out := make([]clientUsageCredential, 0, len(auths))
	for i, auth := range auths {
		result := results[i]
		credential := clientUsageCredential{
			Name:               clientUsageName(auth),
			Provider:           clientUsageProvider(auth),
			Status:             clientUsageCredentialOK,
			ObservedAt:         result.observedAt,
			clientUsageSummary: result.summary,
			Upstream:           clientUsageScrubUpstream(result.upstream, hideNames),
		}
		if credential.Windows == nil {
			credential.Windows = []clientUsageWindow{}
		}
		if hideNames {
			credential.Name = credential.Provider
		}
		if includeRaw {
			credential.Responses = result.responses
			if credential.Responses == nil {
				credential.Responses = []clientUsageUpstream{}
			}
		}
		if result.err != "" {
			credential.Status = clientUsageCredentialFailure
			credential.Error = result.err
		}
		out = append(out, credential)
	}
	return out
}

// clientUsageScrubUpstream copies relayed credentials without their raw
// responses, which stay with the proxy that read them, and with names hidden
// when this proxy hides its own.
func clientUsageScrubUpstream(upstream []clientUsageCredential, hideNames bool) []clientUsageCredential {
	if upstream == nil {
		return nil
	}
	out := make([]clientUsageCredential, len(upstream))
	for i, credential := range upstream {
		credential.Responses = nil
		if hideNames {
			credential.Name = credential.Provider
		}
		credential.Upstream = clientUsageScrubUpstream(credential.Upstream, hideNames)
		out[i] = credential
	}
	return out
}

// probeClientUsage queries one credential's quota. It runs on a detached,
// bounded context so a disconnecting client cannot fail the probe shared with
// other callers, and a stalled upstream cannot pin the cache slot.
func (h *Handler) probeClientUsage(auth *coreauth.Auth, depth int) clientUsageProbeResult {
	ctx, cancel := context.WithTimeout(context.Background(), clientUsageProbeTimeout)
	defer cancel()
	provider := clientUsageProvider(auth)
	if provider == clientUsageRelayProvider {
		return h.probeRelayClientUsage(ctx, auth, depth)
	}
	result := clientUsageProbeResult{observedAt: time.Now().UTC()}

	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if token == "" {
		result.err = "auth token unavailable"
		if errToken != nil {
			result.err = "auth token refresh failed"
		}
		return result
	}

	var errProbe error
	switch provider {
	case "codex":
		result.responses = h.probeCodexClientUsage(ctx, auth, token)
	case "claude":
		result.responses = h.probeClaudeClientUsage(ctx, auth, token)
	case "antigravity":
		result.responses, errProbe = h.probeAntigravityClientUsage(ctx, auth, token)
	case "xai":
		result.responses = h.probeXAIClientUsage(ctx, auth, token)
	case clientUsageCommandCodeProvider:
		result.responses = h.probeCommandCodeClientUsage(ctx, auth, token)
	default:
		errProbe = fmt.Errorf("unsupported provider")
	}
	if errProbe == nil && !clientUsageAnyOK(result.responses) {
		errProbe = fmt.Errorf("no upstream quota endpoint succeeded")
	}
	if errProbe != nil {
		log.WithFields(log.Fields{"provider": provider, "auth_index": auth.Index}).WithError(errProbe).Debug("client usage: quota probe failed")
		result.err = errProbe.Error()
	}
	result.summary = summarizeClientUsage(provider, result.responses, result.observedAt)
	return result
}

// clientUsageAnyOK reports whether a required quota endpoint answered.
// Optional endpoints only add detail.
func clientUsageAnyOK(responses []clientUsageUpstream) bool {
	for _, response := range responses {
		if !response.optional && clientUsageUpstreamOK(response) {
			return true
		}
	}
	return false
}
