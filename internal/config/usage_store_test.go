package config

import "testing"

func TestUsageStoreUsesV8Layout(t *testing.T) {
	data := []byte(`observability:
  usage:
    store:
      enable: true
      path: /data/usage.db
      retention-days: 7
      pricing:
        disable-models-dev: true
        overrides:
          - model: my-model
            input: 1.5
            output: 6
            cache-read: 0
      geoip:
        disable: true
`)
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := cfg.UsageStore
	if !s.Enable || s.Path != "/data/usage.db" || s.RetentionDays != 7 || !s.Pricing.DisableModelsDev || !s.GeoIP.Disable {
		t.Fatalf("unexpected store config: %+v", s)
	}
	if len(s.Pricing.Overrides) != 1 || s.Pricing.Overrides[0].Model != "my-model" || s.Pricing.Overrides[0].Output != 6 {
		t.Fatalf("unexpected overrides: %+v", s.Pricing.Overrides)
	}
	if o := s.Pricing.Overrides[0]; o.CacheRead == nil || *o.CacheRead != 0 || o.CacheWrite != nil {
		t.Fatalf("cache prices: read=%v write=%v", o.CacheRead, o.CacheWrite)
	}
	if errValidate := ValidateV8Config(data); errValidate != nil {
		t.Fatalf("store is not a valid v8 key: %v", errValidate)
	}
}
