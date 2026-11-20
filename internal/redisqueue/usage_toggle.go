package redisqueue

import "sync/atomic"

var usageStatisticsEnabled atomic.Bool

func init() {
	usageStatisticsEnabled.Store(true)
}

// SetUsageStatisticsEnabled toggles whether usage records are enqueued into the redisqueue payload buffer.
// This is controlled by the config field `usage-statistics-enabled` and the corresponding management API.
func SetUsageStatisticsEnabled(enabled bool) { usageStatisticsEnabled.Store(enabled) }

// UsageStatisticsEnabled reports whether the usage queue plugin should publish records.
func UsageStatisticsEnabled() bool { return usageStatisticsEnabled.Load() }

var excludeEmbeddingUsage atomic.Bool

// SetExcludeEmbeddingUsage toggles whether embedding requests are left out of
// the usage queue. Controlled by `observability.usage.exclude-embeddings`.
func SetExcludeEmbeddingUsage(exclude bool) { excludeEmbeddingUsage.Store(exclude) }

// ExcludeEmbeddingUsage reports whether embedding usage records are dropped.
func ExcludeEmbeddingUsage() bool { return excludeEmbeddingUsage.Load() }
