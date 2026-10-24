package cliproxy

import (
	"context"
	"sync"
	"time"

	commandcodeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// commandCodeCatalogTTL bounds how long a fetched catalog is reused before
	// the next model registration refetches it.
	commandCodeCatalogTTL = 30 * time.Minute
	// commandCodeCatalogTimeout bounds the metadata request so a hung upstream
	// cannot pin model registration.
	commandCodeCatalogTimeout = 30 * time.Second
	// commandCodeCatalogRetry spaces out refetches after a failed attempt so an
	// unreachable upstream does not stall every registration batch.
	commandCodeCatalogRetry = time.Minute
)

type commandCodeCatalogFetcher func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error)

// commandCodeModelCatalog caches the public Command Code model catalog. It is
// shared by every Command Code credential because the catalog is not account-specific.
type commandCodeModelCatalog struct {
	mu        sync.Mutex
	models    []*registry.ModelInfo
	fetchedAt time.Time
	attempted time.Time
	now       func() time.Time
	fetch     commandCodeCatalogFetcher
}

var commandCodeModels = &commandCodeModelCatalog{fetch: fetchCommandCodeCatalog}

func fetchCommandCodeCatalog(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error) {
	baseURL, apiKey := "", ""
	if auth != nil && auth.Attributes != nil {
		baseURL = auth.Attributes["base_url"]
		apiKey = auth.Attributes["api_key"]
	}
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, commandCodeCatalogTimeout)
	return commandcodeauth.FetchModels(ctx, client, baseURL, apiKey)
}

// get returns a copy of the catalog, refetching it when it is missing or older
// than commandCodeCatalogTTL. A failed fetch keeps the last good catalog and
// is not retried for commandCodeCatalogRetry.
func (c *commandCodeModelCatalog) get(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) []*registry.ModelInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	due := c.models == nil || now().Sub(c.fetchedAt) >= commandCodeCatalogTTL
	if due && (c.attempted.IsZero() || now().Sub(c.attempted) >= commandCodeCatalogRetry) {
		c.attempted = now()
		fetchCtx, cancel := context.WithTimeout(ctx, commandCodeCatalogTimeout)
		models, errFetch := c.fetch(fetchCtx, cfg, auth)
		cancel()
		switch {
		case errFetch != nil:
			log.WithError(errFetch).Warn("command code: failed to fetch model catalog")
		case len(models) == 0:
			log.Warn("command code: model catalog is empty")
		default:
			c.models = models
			c.fetchedAt = now()
		}
	}
	out := make([]*registry.ModelInfo, 0, len(c.models))
	for _, model := range c.models {
		if model == nil {
			continue
		}
		clone := *model
		out = append(out, &clone)
	}
	return out
}
