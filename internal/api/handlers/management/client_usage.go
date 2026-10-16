package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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

// clientUsageUpstream is one upstream quota response. Body is the upstream
// JSON as returned, minus the account identifiers in clientUsageRedactedKeys.
type clientUsageUpstream struct {
	Endpoint   string          `json:"endpoint"`
	StatusCode int             `json:"status_code,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// clientUsageCredential is one OAuth credential with the upstream quota
// responses the management panel reads for it. Name is the credential's note,
// or its provider when no note is set.
type clientUsageCredential struct {
	Name       string                `json:"name"`
	Provider   string                `json:"provider"`
	Status     string                `json:"status"`
	Error      string                `json:"error,omitempty"`
	ObservedAt time.Time             `json:"observed_at"`
	Responses  []clientUsageUpstream `json:"responses"`
}

type clientUsageResponse struct {
	Credentials []clientUsageCredential `json:"credentials"`
}

type clientUsageProbeResult struct {
	responses  []clientUsageUpstream
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

// prune drops entries of credentials that no longer exist.
func (c *clientUsageCache) prune(keep map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.entries {
		if _, ok := keep[id]; !ok {
			delete(c.entries, id)
		}
	}
}

// GetClientUsage handles GET /v1/usage for client API keys. It returns the
// upstream quota responses of the OAuth credentials selected for the caller's
// key in quota-api.keys; a key without a selection gets an empty list.
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
		c.JSON(http.StatusOK, clientUsageResponse{Credentials: []clientUsageCredential{}})
		return
	}
	ttl := clientUsageCacheTTL(cfg.QuotaAPI.CacheTTLSeconds)
	results := h.clientUsageResults(auths, ttl, h.probeClientUsage)
	c.JSON(http.StatusOK, clientUsageResponse{Credentials: clientUsageCredentials(auths, results)})
}

// clientUsageVisibleAuths keeps the credentials selected for the caller's key.
// Unknown or unsupported IDs in the selection are ignored.
func clientUsageVisibleAuths(pool []*coreauth.Auth, visible map[string]struct{}) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(visible))
	for _, auth := range pool {
		if _, ok := visible[auth.ID]; ok {
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
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "codex":
		return "codex"
	case "claude", "anthropic":
		return "claude"
	case "antigravity":
		return "antigravity"
	case "xai", "grok":
		return "xai"
	default:
		return ""
	}
}

// clientUsageAuths keeps enabled OAuth credentials of supported providers,
// ordered by provider and ID so the response order stays stable.
func clientUsageAuths(list []*coreauth.Auth) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(list))
	for _, auth := range list {
		if auth == nil || strings.TrimSpace(auth.ID) == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if auth.AuthKind() != coreauth.AuthKindOAuth || clientUsageProvider(auth) == "" {
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

func (h *Handler) clientUsageResults(auths []*coreauth.Auth, ttl time.Duration, probe func(*coreauth.Auth) clientUsageProbeResult) []clientUsageProbeResult {
	results := make([]clientUsageProbeResult, len(auths))
	sem := make(chan struct{}, clientUsageProbeConcurrency)
	var wg sync.WaitGroup
	for i, auth := range auths {
		wg.Add(1)
		go func(index int, candidate *coreauth.Auth) {
			defer wg.Done()
			results[index] = h.clientUsage.get(candidate.ID, ttl, func() clientUsageProbeResult {
				sem <- struct{}{}
				defer func() { <-sem }()
				return probe(candidate)
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
	return clientUsageProvider(auth)
}

func clientUsageCredentials(auths []*coreauth.Auth, results []clientUsageProbeResult) []clientUsageCredential {
	out := make([]clientUsageCredential, 0, len(auths))
	for i, auth := range auths {
		result := results[i]
		credential := clientUsageCredential{
			Name:       clientUsageName(auth),
			Provider:   clientUsageProvider(auth),
			Status:     clientUsageCredentialOK,
			ObservedAt: result.observedAt,
			Responses:  result.responses,
		}
		if credential.Responses == nil {
			credential.Responses = []clientUsageUpstream{}
		}
		if result.err != "" {
			credential.Status = clientUsageCredentialFailure
			credential.Error = result.err
		}
		out = append(out, credential)
	}
	return out
}

// probeClientUsage queries one credential's upstream quota endpoints. It runs on
// a detached, bounded context so a disconnecting client cannot fail the probe
// shared with other callers, and a stalled upstream cannot pin the cache slot.
func (h *Handler) probeClientUsage(auth *coreauth.Auth) clientUsageProbeResult {
	ctx, cancel := context.WithTimeout(context.Background(), clientUsageProbeTimeout)
	defer cancel()
	result := clientUsageProbeResult{observedAt: time.Now().UTC()}
	provider := clientUsageProvider(auth)

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
	return result
}

func clientUsageAnyOK(responses []clientUsageUpstream) bool {
	for _, response := range responses {
		if clientUsageUpstreamOK(response) {
			return true
		}
	}
	return false
}
