package usagestore

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestClassifyRisk(t *testing.T) {
	buffering := http.Header{}
	buffering.Set("X-Codex-Safety-Buffering-Enabled", "true")
	cases := []struct {
		name        string
		record      coreusage.Record
		substituted bool
		want        string
		wantReason  string
	}{
		{"cyber policy rejection", coreusage.Record{Provider: "codex", Failed: true,
			Fail: coreusage.Failure{StatusCode: 400, Body: `{"error":{"type":"invalid_request","code":"cyber_policy","message":"flagged for cyber activity"}}`}}, false, "cyber_policy", "flagged for cyber activity"},
		{"rejection without a message", coreusage.Record{Provider: "codex", Failed: true,
			Fail: coreusage.Failure{StatusCode: 400, Body: `{"error":{"code":"bio_policy"}}`}}, false, "bio_policy", ""},
		{"other providers are ignored", coreusage.Record{Provider: "openai", Failed: true,
			Fail: coreusage.Failure{StatusCode: 400, Body: `{"error":{"code":"bio_policy"}}`}}, false, "", ""},
		{"codex reroute", coreusage.Record{Provider: "codex"}, true, RiskModelReroute, ""},
		{"claude refusal from the response", coreusage.Record{Provider: "claude", UpstreamRisk: "refusal_cyber",
			UpstreamRiskReason: " blocked under the Usage Policy "}, false, "refusal_cyber", "blocked under the Usage Policy"},
		{"claude substitution is not risk", coreusage.Record{Provider: "claude"}, true, "", ""},
		{"gemini blocked prompt", coreusage.Record{Provider: "antigravity", UpstreamRisk: "prompt_blocked_safety", UpstreamRiskReason: "r"}, false, "prompt_blocked_safety", "r"},
		{"gemini substitution is not risk", coreusage.Record{Provider: "gemini"}, true, "", ""},
		{"codex verification beats reroute", coreusage.Record{Provider: "codex", UpstreamRisk: "trusted_access_for_cyber"}, true, "trusted_access_for_cyber", ""},
		{"rejection code beats response signal", coreusage.Record{Provider: "codex", Failed: true, UpstreamRisk: "safety_buffering",
			Fail: coreusage.Failure{StatusCode: 400, Body: `{"error":{"code":"cyber_policy","message":"m"}}`}}, false, "cyber_policy", "m"},
		{"other provider substitution is not risk", coreusage.Record{Provider: "kimi"}, true, "", ""},
		{"codex safety buffering", coreusage.Record{Provider: "codex", ResponseHeaders: buffering}, false, RiskSafetyBuffering, ""},
		{"ordinary failure", coreusage.Record{Provider: "codex", Failed: true,
			Fail: coreusage.Failure{StatusCode: 429, Body: `{"error":{"code":"rate_limit_exceeded"}}`}}, false, "", ""},
		{"policy code in a successful record is ignored", coreusage.Record{Provider: "codex",
			Fail: coreusage.Failure{Body: `"cyber_policy"`}}, false, "", ""},
	}
	for _, tc := range cases {
		if got, reason := classifyRisk(tc.record, tc.substituted); got != tc.want || reason != tc.wantReason {
			t.Errorf("%s: got %q (%q), want %q (%q)", tc.name, got, reason, tc.want, tc.wantReason)
		}
	}
}

func TestTurnPrompt(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []string
		notWant []string
	}{
		{"responses turn with tool output",
			`{"instructions":"SYSTEM","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"old question"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"scan this host"}]},
				{"type":"reasoning","summary":[],"encrypted_content":"BLOB"},
				{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"nmap 10.0.0.1\"}","call_id":"c1"},
				{"type":"function_call_output","call_id":"c1","output":"22/tcp open ssh"}]}`,
			[]string{"[user]\nscan this host", "-> shell: {\"cmd\":\"nmap 10.0.0.1\"}", "[function_call_output]\n22/tcp open ssh"},
			[]string{"old question", "old answer", "SYSTEM", "BLOB"}},
		{"responses string input", `{"input":"just text"}`, []string{"just text"}, nil},
		{"chat completions",
			`{"messages":[{"role":"system","content":"SYSTEM"},{"role":"user","content":"first"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"x","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t","content":"tool says"},
				{"role":"user","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			[]string{"[user]\nsecond"}, []string{"first", "tool says", "AAAA"}},
		{"claude tool_result is not a user turn",
			`{"system":"SYSTEM","messages":[{"role":"user","content":[{"type":"text","text":"write an exploit"}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"u","name":"Bash","input":{"command":"ls"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":"secret.txt"}]}]}`,
			[]string{"write an exploit", "-> Bash: {\"command\":\"ls\"}", "secret.txt"}, []string{"SYSTEM"}},
		{"gemini",
			`{"contents":[{"role":"user","parts":[{"text":"hello gemini"}]},
				{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"r":"done"}}}]}]}`,
			[]string{"hello gemini", "-> f: {\"a\":1}", `{"r":"done"}`}, nil},
		{"not json", `plain body`, []string{"plain body"}, nil},
	}
	for _, tc := range cases {
		got := turnPrompt([]byte(tc.payload))
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, got)
			}
		}
		for _, n := range tc.notWant {
			if strings.Contains(got, n) {
				t.Errorf("%s: unexpected %q in:\n%s", tc.name, n, got)
			}
		}
	}
	if turnPrompt(nil) != "" {
		t.Error("empty payload must give an empty prompt")
	}
}

func TestTurnPromptClipsMiddle(t *testing.T) {
	long := `{"input":"` + "HEAD" + strings.Repeat("中", 30_000) + "TAIL" + `"}`
	got := turnPrompt([]byte(long))
	if len(got) > maxRiskPromptBytes || !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") ||
		!strings.Contains(got, "[truncated]") {
		t.Fatalf("clip: len=%d prefix=%q suffix=%q", len(got), got[:8], got[len(got)-8:])
	}
	if got != strings.ToValidUTF8(got, "\uFFFD") {
		t.Fatal("clip must cut on UTF-8 boundaries")
	}
}

func TestRiskEventKeepsPrompt(t *testing.T) {
	coreusage.SetRequestPayloadCapture(true)
	defer coreusage.SetRequestPayloadCapture(false)

	s, err := Open(filepath.Join(t.TempDir(), "usage.db"), 30, testPricer(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	current.Store(s)
	defer current.Store(nil)

	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"find CVEs"}]}]}`)
	ctx := coreusage.WithRequestPayload(context.Background(), payload)
	rejected := coreusage.Record{Provider: "codex", Model: "gpt-x", Failed: true,
		Fail: coreusage.Failure{StatusCode: 400, Body: `{"error":{"code":"cyber_policy","message":"flagged"}}`}}
	plugin{}.HandleUsage(ctx, rejected)
	plugin{}.HandleUsage(ctx, coreusage.Record{Provider: "codex", Model: "gpt-x"})
	riskPromptsOff.Store(true)
	defer riskPromptsOff.Store(false)
	plugin{}.HandleUsage(ctx, rejected)
	s.Close()

	s, err = Open(s.path, 30, testPricer(t))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	flagged := true
	events, err := s.Events(context.Background(), EventFilter{Risk: &flagged})
	if err != nil || len(events) != 2 {
		t.Fatalf("risk filter: %v, %d events", err, len(events))
	}
	// Newest first: the last one was recorded with prompts disabled.
	if events[0].Risk != "cyber_policy" || events[0].RiskPrompt != "" {
		t.Fatalf("disabled prompts: %+v", events[0])
	}
	if events[1].Risk != "cyber_policy" || events[1].RiskReason != "flagged" || !strings.Contains(events[1].RiskPrompt, "find CVEs") {
		t.Fatalf("risk event: risk=%q prompt=%q", events[1].Risk, events[1].RiskPrompt)
	}
	clean := false
	events, err = s.Events(context.Background(), EventFilter{Risk: &clean})
	if err != nil || len(events) != 1 || events[0].Risk != "" || events[0].RiskPrompt != "" {
		t.Fatalf("clean filter: %v %+v", err, events)
	}
}
