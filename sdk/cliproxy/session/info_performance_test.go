package session

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestHasExplicitSessionDoesNotRescanLargePayload(t *testing.T) {
	payload := []byte(`{"messages":["` + strings.Repeat("x", 1<<20) + `"]}`)

	singleLookup := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			root := util.ParseGJSONBytesNoCopy(payload)
			if root.Get("missing_session_id").Exists() {
				b.Fatal("unexpected session ID")
			}
		}
	})
	detection := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if hasExplicitSession(nil, payload) {
				b.Fatal("unexpected explicit session")
			}
		}
	})

	if detection.NsPerOp() > singleLookup.NsPerOp()*12 {
		t.Fatalf("explicit session detection rescans large payload: detection %d ns/op, single lookup %d ns/op", detection.NsPerOp(), singleLookup.NsPerOp())
	}
}

func TestExtractSessionInfoDoesNotRescanLargePayload(t *testing.T) {
	payload := []byte(`{"messages":["` + strings.Repeat("x", 1<<20) + `"]}`)

	singleLookup := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			root := util.ParseGJSONBytesNoCopy(payload)
			if root.Get("missing_session_id").Exists() {
				b.Fatal("unexpected session ID")
			}
		}
	})
	extraction := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if info, ok := ExtractSessionInfo(nil, payload, nil); ok {
				b.Fatalf("unexpected session: %q", info.SessionID)
			}
		}
	})

	// A request without session fields must not rescan a large message for every candidate key.
	if extraction.NsPerOp() > singleLookup.NsPerOp()*12 {
		t.Fatalf("session extraction rescans large payload: extraction %d ns/op, single lookup %d ns/op", extraction.NsPerOp(), singleLookup.NsPerOp())
	}
}

func BenchmarkExtractSessionInfoCodexResponsesPayload(b *testing.B) {
	// ~1 MiB Codex-like payload with large input array and tools, no metadata.
	padding := strings.Repeat("x", 1024)
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-5","input":[`)
	for i := 0; i < 1000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + padding + `"}]}`)
	}
	sb.WriteString(`],"tools":[{"type":"function","name":"exec","description":"execute shell","parameters":{"type":"object"}}]}`)
	payload := []byte(sb.String())

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		ExtractSessionInfo(nil, payload, nil)
	}
}

func BenchmarkExtractSessionInfoClaudePayload(b *testing.B) {
	// Claude body with metadata.user_id.
	padding := strings.Repeat("x", 1<<20)
	payload := []byte(`{"model":"claude-3-7-sonnet","metadata":{"user_id":"{\"session_id\":\"session_12345\",\"parent_session_id\":\"session_parent\",\"agent_id\":\"agent_sub\"}"},"messages":[{"role":"user","content":"` + padding + `"}]}`)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		ExtractSessionInfo(nil, payload, nil)
	}
}
