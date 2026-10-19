package pluginhost

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// newHostWithoutTranslationHooks mirrors a production host: plugins are
// loaded, but none of them registers a translation hook. Paths are already
// clean, as discovered plugin paths are, so the identity check does not
// allocate on Windows either.
func newHostWithoutTranslationHooks() *Host {
	observer := func(ctx context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	return newHostWithRecords(
		capabilityRecord{id: "observer-1", path: filepath.Join("testdata", "observer-1.plugin"), priority: 20, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			RequestInterceptor: requestInterceptorFunc(observer),
		}}},
		capabilityRecord{id: "observer-2", path: filepath.Join("testdata", "observer-2.plugin"), priority: 10, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			RequestInterceptor: requestInterceptorFunc(observer),
		}}},
	)
}

// TestTranslationHooksDoNotCopyRecords pins one allocation per call: the copy
// of the body the hooks return. The records of the runtime snapshot used to be
// copied as well, on every streamed chunk.
func TestTranslationHooksDoNotCopyRecords(t *testing.T) {
	host := newHostWithoutTranslationHooks()
	ctx := context.Background()
	from, to := sdktranslator.FormatOpenAI, sdktranslator.FormatClaude
	body := []byte(`{"type":"message_delta"}`)

	hooks := map[string]func(){
		"NormalizeRequest": func() { host.NormalizeRequest(ctx, from, to, "model", body, true) },
		"TranslateRequest": func() { host.TranslateRequest(ctx, from, to, "model", body, true) },
		"NormalizeResponseBefore": func() {
			host.NormalizeResponseBefore(ctx, from, to, "model", nil, nil, body, true)
		},
		"TranslateResponse": func() { host.TranslateResponse(ctx, from, to, "model", nil, nil, body, true) },
		"NormalizeResponseAfter": func() {
			host.NormalizeResponseAfter(ctx, from, to, "model", nil, nil, body, true)
		},
	}
	for name, call := range hooks {
		if allocs := testing.AllocsPerRun(100, call); allocs != 1 {
			t.Errorf("%s allocs = %v, want 1 (the body copy)", name, allocs)
		}
	}
}

func TestTranslateResponseStopsAtFirstTranslator(t *testing.T) {
	calls := 0
	translator := func(out string) responseTranslatorFunc {
		return func(ctx context.Context, req pluginapi.ResponseTransformRequest) (pluginapi.PayloadResponse, error) {
			calls++
			return pluginapi.PayloadResponse{Body: []byte(out)}, nil
		}
	}
	host := newHostWithRecords(
		capabilityRecord{id: "high", priority: 20, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ResponseTranslator: translator("high"),
		}}},
		capabilityRecord{id: "low", priority: 10, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ResponseTranslator: translator("low"),
		}}},
	)

	got, ok := host.TranslateResponse(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "model", nil, nil, []byte("body"), true)
	if !ok || string(got) != "high" || calls != 1 {
		t.Fatalf("TranslateResponse() = %q, %v after %d calls, want \"high\", true after 1 call", got, ok, calls)
	}
}

func TestTranslationHooksOnNilHost(t *testing.T) {
	var host *Host
	got := host.NormalizeResponseAfter(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "model", nil, nil, []byte("body"), true)
	if string(got) != "body" {
		t.Fatalf("NormalizeResponseAfter() on nil host = %q, want body", got)
	}
}

func BenchmarkNormalizeResponseWithoutHooks(b *testing.B) {
	host := newHostWithoutTranslationHooks()
	ctx := context.Background()
	body := []byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)
	b.ReportAllocs()
	for b.Loop() {
		host.NormalizeResponseBefore(ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "model", nil, nil, body, true)
		host.NormalizeResponseAfter(ctx, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "model", nil, nil, body, true)
	}
}
