package executor

import (
	"fmt"
	"strings"
	"testing"
)

// TestAntigravityReplayFingerprintSkipsSignatureDeletesSafely pins the context
// fingerprint against the legacy oracle for parts with and without signature
// keys, including a key spelled with a \u escape, which the "hought" pre-check
// must not skip.
func TestAntigravityReplayFingerprintSkipsSignatureDeletesSafely(t *testing.T) {
	payload := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"sys"}]},"tools":[{"functionDeclarations":[{"name":"run"}]}],"contents":[` +
		`{"role":"user","parts":[{"text":"hi <b> & bye"}]},` +
		`{"role":"model","parts":[{"text":"plan","thoughtSignature":"sig-a"},{"functionCall":{"name":"run","args":{"cmd":"ls"}},"thought_signature":"sig-b"}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"run","response":{"out":"x"}}}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"run","args":{"cmd":"pwd"}},"extra_content":{"google":{"thought_signature":"sig-c"}}}]},` +
		`{"role":"model","parts":[{"text":"t","t\u0068oughtSignature":"sig-d"}]},` +
		`{"role":"user","parts":[{"text":"caf\u00e9"}]}` +
		`]}}`)
	index := newAntigravityReplayRequestIndex(payload)
	for i := 0; i <= 6; i++ {
		if got, want := index.contextFingerprint(i), legacyAntigravityReplayContextFingerprint(payload, i); got != want {
			t.Fatalf("fingerprint before content %d: got %q, want %q", i, got, want)
		}
	}
}

func BenchmarkAntigravityReplayContextFingerprint(b *testing.B) {
	var contents []string
	for i := 0; i < 200; i++ {
		contents = append(contents,
			fmt.Sprintf(`{"role":"model","parts":[{"functionCall":{"name":"run","args":{"cmd":"go test ./pkg%d/...","timeout":120}}}]}`, i),
			fmt.Sprintf(`{"role":"user","parts":[{"functionResponse":{"name":"run","response":{"output":"%s"}}}]}`, strings.Repeat("ok\\n", 40)))
	}
	payload := []byte(`{"request":{"contents":[` + strings.Join(contents, ",") + `]}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		newAntigravityReplayRequestIndex(payload).contextFingerprint(len(contents))
	}
}
