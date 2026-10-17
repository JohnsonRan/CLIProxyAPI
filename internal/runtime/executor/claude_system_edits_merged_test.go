package executor

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
)

// checkSystemInstructionsBothWays returns what checkSystemInstructionsWithSigningModeAt
// produces and what the sequential rewrite produces for the same inputs.
func checkSystemInstructionsBothWays(t *testing.T, payload []byte, strict, keepTop bool, turnOrigin ...string) ([]byte, []byte) {
	t.Helper()
	got := checkSystemInstructionsWithSigningModeAt(append([]byte(nil), payload...), strict, false, "2.1.280", "cli", "", "2026-10-18", false, "", "", keepTop, turnOrigin...)
	system, systemBlocks, isExplicit := claudeCloakSystemBlocks(payload, false, "2.1.280", "cli", "", false, "", "", turnOrigin...)
	want := sequentialClaudeSystemInstructions(append([]byte(nil), payload...), system, systemBlocks, strict, "2026-10-18", keepTop, isExplicit)
	return got, want
}

func TestMergedClaudeSystemInstructionsMatchSequentialRewrite(t *testing.T) {
	systems := []string{
		`"You are a helpful assistant."`,
		`[{"type":"text","text":"Rule one."},{"type":"text","text":"Rule two.","cache_control":{"type":"ephemeral"}}]`,
		`[]`,
	}
	messageSets := []string{
		`[{"role":"user","content":"hello"}]`,
		`[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":"yo"},{"role":"user","content":"next"}]`,
		`[{"role":"user","content":"a"},{"role":"user","content":"b"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`,
		`[{"role":"user","content":"hello"},{"role":"system","content":[{"type":"text","text":"You are a helpful assistant."}]},{"role":"assistant","content":"x"}]`,
		`[ {"role":"user","content":"spaced"} , {"role":"assistant","content":"x"} ]`,
		`[{"role":"assistant","content":"no user"}]`,
	}
	models := []string{"claude-opus-5-5", "claude-fable-5-1", "claude-3-7-sonnet-20250219"}
	cases := 0
	for _, model := range models {
		for _, system := range append(systems, "") {
			for _, messages := range messageSets {
				body := fmt.Sprintf(`{"model":%q,`, model)
				if system != "" {
					body += `"system":` + system + `,`
				}
				body += `"messages":` + messages + `,"max_tokens":1024}`
				for _, strict := range []bool{false, true} {
					for _, keepTop := range []bool{false, true} {
						for _, origin := range [][]string{nil, {"user", "explicit_cache_mode"}} {
							got, want := checkSystemInstructionsBothWays(t, []byte(body), strict, keepTop, origin...)
							cases++
							if !bytes.Equal(got, want) {
								t.Errorf("%s strict=%v keepTop=%v origin=%v:\n got  %s\n want %s", body, strict, keepTop, origin, got, want)
							}
						}
					}
				}
			}
		}
	}
	if cases == 0 {
		t.Fatal("no cases ran")
	}
}

func TestMergedClaudeSystemInstructionsCopyTheBodyOnce(t *testing.T) {
	body := largeClaudeCodeBody(100)
	got, want := checkSystemInstructionsBothWays(t, body, false, false)
	if !bytes.Equal(got, want) {
		t.Fatal("merged rewrite differs from the sequential one")
	}
	system, systemBlocks, isExplicit := claudeCloakSystemBlocks(body, false, "2.1.280", "cli", "", false, "", "")
	measure := func(run func()) int64 {
		const runs = 5
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			run()
		}
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc-before.TotalAlloc) / runs
	}
	merged := measure(func() {
		if _, ok := mergedClaudeSystemInstructions(body, system, systemBlocks, false, "2026-10-18", false, isExplicit); !ok {
			t.Fatal("merged path declined the body")
		}
	})
	sequential := measure(func() {
		sequentialClaudeSystemInstructions(body, system, systemBlocks, false, "2026-10-18", false, isExplicit)
	})
	t.Logf("merged %d bytes, sequential %d bytes, body %d bytes", merged, sequential, len(body))
	if merged > int64(len(body))*3/2 {
		t.Fatalf("merged rewrite allocates %d bytes per call on a %d byte body", merged, len(body))
	}
}
