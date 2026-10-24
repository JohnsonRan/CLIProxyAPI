package usagestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestExcludeEmbeddingsSkipsEmbeddingRecords(t *testing.T) {
	cfg := &config.Config{}
	cfg.UsageStore = config.UsageStoreConfig{Enable: true, Path: filepath.Join(t.TempDir(), "usage.db"),
		Pricing: config.UsagePricingConfig{DisableModelsDev: true}}
	embedCtx := coreusage.WithRequestKind(context.Background(), coreusage.RequestKindEmbeddings)
	record := func(model string) coreusage.Record {
		return coreusage.Record{Model: model, Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1}}
	}
	handle := func(ctx context.Context, model string) { plugin{}.HandleUsage(ctx, record(model)) }

	cfg.UsageExcludeEmbeddings = true
	Apply(cfg)
	handle(embedCtx, "excluded")
	handle(context.Background(), "chat")
	// Home mode keeps every record, like the usage queue.
	cfg.Home.Enabled = true
	Apply(cfg)
	handle(embedCtx, "home")
	cfg.Home.Enabled = false
	// Hot reload turns the switch off.
	cfg.UsageExcludeEmbeddings = false
	Apply(cfg)
	handle(embedCtx, "kept")

	// Reopen to flush the writer.
	cfg.UsageStore.Enable = false
	Apply(cfg)
	cfg.UsageStore.Enable = true
	Apply(cfg)
	defer Shutdown()
	events, err := Current().Events(context.Background(), EventFilter{})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	got := map[string]bool{}
	for _, e := range events {
		got[e.Model] = true
	}
	if len(events) != 3 || got["excluded"] || !got["chat"] || !got["home"] || !got["kept"] {
		t.Fatalf("stored models = %v", got)
	}
}
