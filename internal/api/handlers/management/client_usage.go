package management

import (
	"context"
	"fmt"
	"math"
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
	clientUsagePercentLimit      = 100.0
	clientUsageResponseUnit      = "%"
	clientUsageResponseMode      = "quota_limited"
	clientUsageResponsePlanName  = "CLIProxyAPI"
	clientUsageCredentialOK      = "ok"
	clientUsageCredentialFailure = "error"
)

// clientUsageWindow is one quota window of a credential, expressed in percent.
// Its JSON shape matches the Sub2API /v1/usage rate_limits entries.
type clientUsageWindow struct {
	Window    string     `json:"window"`
	Scope     string     `json:"scope,omitempty"`
	Limit     float64    `json:"limit"`
	Used      float64    `json:"used"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`

	seconds int64
}

// clientUsageCredential is the per-credential quota detail. It never carries
// account identifiers; credentials are named "<provider>#<n>".
type clientUsageCredential struct {
	Name       string              `json:"name"`
	Provider   string              `json:"provider"`
	Status     string              `json:"status"`
	Plan       string              `json:"plan,omitempty"`
	Error      string              `json:"error,omitempty"`
	Windows    []clientUsageWindow `json:"windows"`
	ObservedAt time.Time           `json:"observed_at"`
}

// clientUsageAggregate averages one window duration across the credential pool.
type clientUsageAggregate struct {
	Window      string     `json:"window"`
	Limit       float64    `json:"limit"`
	Used        float64    `json:"used"`
	Remaining   float64    `json:"remaining"`
	ResetAt     *time.Time `json:"reset_at,omitempty"`
	Credentials int        `json:"credentials"`

	seconds int64
}

// clientUsageResponse follows the Sub2API /v1/usage quota_limited shape and
// extends it with per-credential details.
type clientUsageResponse struct {
	Mode        string                  `json:"mode"`
	IsValid     bool                    `json:"isValid"`
	Status      string                  `json:"status"`
	PlanName    string                  `json:"planName"`
	Unit        string                  `json:"unit"`
	Remaining   *float64                `json:"remaining,omitempty"`
	RateLimits  []clientUsageAggregate  `json:"rate_limits,omitempty"`
	Credentials []clientUsageCredential `json:"credentials"`
}

type clientUsageProbeResult struct {
	plan       string
	windows    []clientUsageWindow
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

// GetClientUsage handles GET /v1/usage for client API keys. It reports the
// quota of every supported OAuth credential in a Sub2API-compatible shape.
func (h *Handler) GetClientUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "handler unavailable", "type": "server_error"}})
		return
	}
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	h.mu.Unlock()
	if cfg == nil || !cfg.QuotaAPI.Enabled {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "usage endpoint is disabled", "type": "invalid_request_error", "code": "not_found"}})
		return
	}
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "core auth manager unavailable", "type": "server_error"}})
		return
	}

	auths := clientUsageAuths(manager.List())
	keep := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		keep[auth.ID] = struct{}{}
	}
	h.clientUsage.prune(keep)

	ttl := clientUsageCacheTTL(cfg.QuotaAPI.CacheTTLSeconds)
	results := h.clientUsageResults(auths, ttl, h.probeClientUsage)
	c.JSON(http.StatusOK, buildClientUsageResponse(clientUsageCredentials(auths, results)))
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
// ordered by provider and ID so "<provider>#<n>" names stay stable.
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

func clientUsageCredentials(auths []*coreauth.Auth, results []clientUsageProbeResult) []clientUsageCredential {
	counters := make(map[string]int)
	out := make([]clientUsageCredential, 0, len(auths))
	for i, auth := range auths {
		provider := clientUsageProvider(auth)
		counters[provider]++
		result := results[i]
		credential := clientUsageCredential{
			Name:       fmt.Sprintf("%s#%d", provider, counters[provider]),
			Provider:   provider,
			Status:     clientUsageCredentialOK,
			Plan:       result.plan,
			Windows:    result.windows,
			ObservedAt: result.observedAt,
		}
		if credential.Windows == nil {
			credential.Windows = []clientUsageWindow{}
		}
		if result.err != "" {
			credential.Status = clientUsageCredentialFailure
			credential.Error = result.err
		}
		out = append(out, credential)
	}
	return out
}

// aggregateClientUsage averages each window duration across healthy
// credentials. Per credential, the account-wide window of a duration is used;
// when only scoped windows exist (for example Antigravity model families),
// the most constrained one represents the credential.
func aggregateClientUsage(credentials []clientUsageCredential) []clientUsageAggregate {
	type accumulator struct {
		sum     float64
		count   int
		resetAt *time.Time
		seconds int64
	}
	byWindow := make(map[string]*accumulator)
	for _, credential := range credentials {
		if credential.Status != clientUsageCredentialOK {
			continue
		}
		chosen := make(map[string]clientUsageWindow)
		for _, window := range credential.Windows {
			current, exists := chosen[window.Window]
			switch {
			case !exists:
			case window.Scope == "" && current.Scope != "":
			case (window.Scope == "") == (current.Scope == "") && window.Remaining < current.Remaining:
			default:
				continue
			}
			chosen[window.Window] = window
		}
		for key, window := range chosen {
			acc := byWindow[key]
			if acc == nil {
				acc = &accumulator{seconds: window.seconds}
				byWindow[key] = acc
			}
			acc.sum += window.Remaining
			acc.count++
			if window.ResetAt != nil && (acc.resetAt == nil || window.ResetAt.Before(*acc.resetAt)) {
				resetAt := *window.ResetAt
				acc.resetAt = &resetAt
			}
		}
	}
	out := make([]clientUsageAggregate, 0, len(byWindow))
	for key, acc := range byWindow {
		remaining := roundClientUsage(acc.sum / float64(acc.count))
		out = append(out, clientUsageAggregate{
			Window:      key,
			Limit:       clientUsagePercentLimit,
			Used:        roundClientUsage(clientUsagePercentLimit - remaining),
			Remaining:   remaining,
			ResetAt:     acc.resetAt,
			Credentials: acc.count,
			seconds:     acc.seconds,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].seconds != out[j].seconds {
			return out[i].seconds < out[j].seconds
		}
		return out[i].Window < out[j].Window
	})
	return out
}

func buildClientUsageResponse(credentials []clientUsageCredential) clientUsageResponse {
	if credentials == nil {
		credentials = []clientUsageCredential{}
	}
	resp := clientUsageResponse{
		Mode:        clientUsageResponseMode,
		IsValid:     true,
		Status:      "active",
		PlanName:    clientUsageResponsePlanName,
		Unit:        clientUsageResponseUnit,
		RateLimits:  aggregateClientUsage(credentials),
		Credentials: credentials,
	}
	for _, aggregate := range resp.RateLimits {
		if resp.Remaining == nil || aggregate.Remaining < *resp.Remaining {
			remaining := aggregate.Remaining
			resp.Remaining = &remaining
		}
	}
	return resp
}

// probeClientUsage queries one credential's upstream quota endpoint. It runs on
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
		result.plan, result.windows, errProbe = h.probeCodexClientUsage(ctx, auth, token, result.observedAt)
	case "claude":
		result.windows, errProbe = h.probeClaudeClientUsage(ctx, auth, token)
	case "antigravity":
		result.windows, errProbe = h.probeAntigravityClientUsage(ctx, auth, token)
	case "xai":
		result.plan, result.windows, errProbe = h.probeXAIClientUsage(ctx, auth, token)
	default:
		errProbe = fmt.Errorf("unsupported provider")
	}
	if errProbe == nil && len(result.windows) == 0 {
		errProbe = fmt.Errorf("upstream returned no quota windows")
	}
	if errProbe != nil {
		log.WithFields(log.Fields{"provider": provider, "auth_index": auth.Index}).WithError(errProbe).Debug("client usage: quota probe failed")
		result.err = errProbe.Error()
		result.windows = nil
	}
	return result
}

func newClientUsageWindow(seconds int64, scope string, usedPercent float64, resetAt *time.Time) clientUsageWindow {
	if math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) {
		usedPercent = 0
	}
	used := roundClientUsage(math.Max(0, math.Min(clientUsagePercentLimit, usedPercent)))
	return clientUsageWindow{
		Window:    clientUsageDuration(seconds),
		Scope:     scope,
		Limit:     clientUsagePercentLimit,
		Used:      used,
		Remaining: roundClientUsage(clientUsagePercentLimit - used),
		ResetAt:   resetAt,
		seconds:   seconds,
	}
}

// clientUsageDuration renders a window length the way Sub2API does ("5h", "7d").
func clientUsageDuration(seconds int64) string {
	switch {
	case seconds <= 0:
		return "unknown"
	case seconds%(24*60*60) == 0:
		return fmt.Sprintf("%dd", seconds/(24*60*60))
	case seconds%(60*60) == 0:
		return fmt.Sprintf("%dh", seconds/(60*60))
	case seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func roundClientUsage(value float64) float64 {
	return math.Round(value*100) / 100
}
