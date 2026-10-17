package executor

import (
	"bytes"
	"runtime"
	"testing"
)

func TestUpgradeClaudeCacheControlTTLMatchesPerPathRewrite(t *testing.T) {
	payloads := map[string]string{
		"all locations": `{"tools":[{"name":"a","cache_control":{"type":"ephemeral"}},{"name":"b"}],` +
			`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","scope":"global"}}],` +
			`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]},` +
			`{"role":"assistant","content":"plain"},` +
			`{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"y","cache_control":{"type":"ephemeral"}}]}]}`,
		"already has ttl":       `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`,
		"non-string type":       `{"system":[{"type":"text","text":"s","cache_control":{"type":1}}]}`,
		"null control":          `{"messages":[{"role":"user","content":[{"type":"text","text":"s","cache_control":null}]}]}`,
		"no blocks":             `{"model":"m","messages":[]}`,
		"spaced json":           "{ \"system\" : [ { \"type\" : \"text\" , \"cache_control\" : { \"scope\" : \"org\" , \"type\" : \"ephemeral\" } } ] }",
		"escaped strings":       `{"messages":[{"role":"user","content":[{"type":"text","text":"a\"}\\u003c","cache_control":{"type":"ephem\u0065ral"}}]}]}`,
		"cache_control in text": `{"messages":[{"role":"user","content":[{"type":"text","text":"\"cache_control\":{\"type\":\"ephemeral\"}"}]}]}`,
	}
	for name, payload := range payloads {
		for _, ttl := range []string{"1h", "5m"} {
			want := upgradeClaudeCacheControlTTLByPath([]byte(payload), ttl)
			got := upgradeClaudeCacheControlTTL([]byte(payload), ttl)
			if !bytes.Equal(got, want) {
				t.Errorf("%s (ttl %s):\n got  %s\n want %s", name, ttl, got, want)
			}
		}
	}
}

func TestUpgradeClaudeCacheControlTTLRewritesTheBodyOnce(t *testing.T) {
	body := largeClaudeCodeBody(100)
	// Breakpoints on the system prompt, the first and the last message.
	body = bytes.Replace(body, []byte(`{"type":"text","text":"0 `), []byte(`{"type":"text","cache_control":{"type":"ephemeral"},"text":"0 `), 1)
	body = bytes.Replace(body, []byte(`{"type":"text","text":"99 `), []byte(`{"type":"text","cache_control":{"type":"ephemeral"},"text":"99 `), 1)

	want := upgradeClaudeCacheControlTTLByPath(body, "1h")
	if bytes.Equal(want, body) {
		t.Fatal("test body has no upgradable cache_control")
	}
	if got := upgradeClaudeCacheControlTTL(body, "1h"); !bytes.Equal(got, want) {
		t.Fatal("batched upgrade differs from the per-path rewrite")
	}

	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		upgradeClaudeCacheControlTTL(body, "1h")
	}
	runtime.ReadMemStats(&after)
	// One output buffer plus bookkeeping; three blocks used to cost three copies.
	if allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs; allocated > int64(len(body))*3/2 {
		t.Fatalf("upgrade allocates %d bytes per call on a %d byte body", allocated, len(body))
	}
}
