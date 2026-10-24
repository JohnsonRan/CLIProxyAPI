package config

// UsageStoreConfig controls the built-in SQLite usage store
// (`observability.usage.store`).
type UsageStoreConfig struct {
	// Enable turns on persistence of usage records. Default false.
	Enable bool `yaml:"enable" json:"enable"`
	// Path is the SQLite database file. Empty uses usage.db in the logs directory.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// RetentionDays keeps per-request events this many days (default 30, negative keeps forever).
	// Hourly aggregates are never pruned.
	RetentionDays int `yaml:"retention-days,omitempty" json:"retention-days,omitempty"`
	// Pricing configures cost estimation.
	Pricing UsagePricingConfig `yaml:"pricing,omitempty" json:"pricing,omitempty"`
	// GeoIP configures offline IP geolocation and ISP lookup.
	GeoIP UsageGeoIPConfig `yaml:"geoip,omitempty" json:"geoip,omitempty"`
}

// UsageGeoIPConfig controls offline IP geolocation and ISP lookup for usage events.
type UsageGeoIPConfig struct {
	// Disable stops sending new public client IPs to the online lookup API. Default false (enabled).
	Disable bool `yaml:"disable,omitempty" json:"disable,omitempty"`
}

// UsagePricingConfig configures cost estimation for stored usage.
type UsagePricingConfig struct {
	// DisableModelsDev stops fetching prices from https://models.dev/api.json.
	DisableModelsDev bool `yaml:"disable-models-dev,omitempty" json:"disable-models-dev,omitempty"`
	// Overrides set prices (USD per million tokens) by model name and win over models.dev.
	Overrides []UsagePriceOverride `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// UsagePriceOverride is a per-model price in USD per million tokens.
type UsagePriceOverride struct {
	Model  string  `yaml:"model" json:"model"`
	Input  float64 `yaml:"input" json:"input"`
	Output float64 `yaml:"output" json:"output"`
	// CacheRead and CacheWrite default to the input price when unset.
	CacheRead  *float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
	CacheWrite *float64 `yaml:"cache-write,omitempty" json:"cache-write,omitempty"`
}
