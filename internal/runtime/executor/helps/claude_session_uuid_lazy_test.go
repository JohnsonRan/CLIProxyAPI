package helps

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// claudeSessionUUIDEager is the previous ClaudeAgentSessionUUIDForRequest for
// unconfirmed callers, which stripped metadata.user_id from both payloads up
// front.
func claudeSessionUUIDEager(headers http.Header, original, translated []byte, metadataSets ...map[string]any) string {
	headers = headers.Clone()
	for key := range headers {
		if strings.EqualFold(key, "X-Claude-Code-Session-Id") {
			delete(headers, key)
		}
	}
	return claudeAgentSessionUUID(headers, withoutClaudeMetadataUserID(original), withoutClaudeMetadataUserID(translated), metadataSets...)
}

func TestClaudeAgentSessionUUIDForRequestMatchesEagerStrip(t *testing.T) {
	headers := http.Header{"X-Claude-Code-Session-Id": {"11111111-1111-1111-1111-111111111111"}}
	payloads := []string{
		``,
		`{}`,
		`{"prompt_cache_key":"pck-1","input":[]}`,
		`{"metadata":{"user_id":"user_abc_account__session_22222222-2222-2222-2222-222222222222"}}`,
		`{"metadata":{"user_id":"x"},"session_id":"s-1"}`,
		`{"conversation":{"id":"conv-1"}}`,
	}
	for _, original := range payloads {
		for _, translated := range payloads {
			want := claudeSessionUUIDEager(headers, []byte(original), []byte(translated))
			got := ClaudeAgentSessionUUIDForRequest(headers, []byte(original), []byte(translated), false)
			// An empty identity yields a random UUID on both paths.
			if want != got && !(isRandomSession(want) && isRandomSession(got)) {
				t.Errorf("original %s translated %s: got %s, want %s", original, translated, got, want)
			}
		}
	}
}

func isRandomSession(id string) bool {
	return len(id) == 36 && id[14] == '4'
}

func TestClaudeAgentSessionUUIDForRequestDoesNotCopyAnUnusedTranslation(t *testing.T) {
	original := []byte(`{"prompt_cache_key":"pck-1","input":[]}`)
	translated := []byte(`{"metadata":{"user_id":"u"},"messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 20000) + `{}]}`)
	ClaudeAgentSessionUUIDForRequest(nil, original, translated, false)
	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		ClaudeAgentSessionUUIDForRequest(nil, original, translated, false)
	}
	runtime.ReadMemStats(&after)
	if allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs; allocated > int64(len(translated)/4) {
		t.Fatalf("allocates %d bytes per call with a %d byte translation it does not read", allocated, len(translated))
	}
}
