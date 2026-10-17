package executor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeClaudeSamplingSequential is the previous implementation, one sjson
// delete per key.
func normalizeClaudeSamplingSequential(body []byte, nativeOwned bool) []byte {
	thinkingActive := false
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "enabled", "adaptive", "auto":
		thinkingActive = true
	}
	if !nativeOwned {
		body, _ = sjson.DeleteBytes(body, "temperature")
		body, _ = sjson.DeleteBytes(body, "top_p")
		if thinkingActive {
			body, _ = sjson.DeleteBytes(body, "top_k")
		}
		return body
	}
	if thinkingActive {
		if temperature := gjson.GetBytes(body, "temperature"); temperature.Exists() && temperature.Num != 1 {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
		if topP := gjson.GetBytes(body, "top_p"); topP.Exists() && topP.Num < 0.95 {
			body, _ = sjson.DeleteBytes(body, "top_p")
		}
		body, _ = sjson.DeleteBytes(body, "top_k")
		return body
	}
	if gjson.GetBytes(body, "temperature").Exists() && gjson.GetBytes(body, "top_p").Exists() {
		body, _ = sjson.DeleteBytes(body, "top_p")
	}
	return body
}

func TestNormalizeClaudeSamplingMatchesSequentialDeletes(t *testing.T) {
	bodies := []string{
		`{"model":"m","temperature":0.7,"top_p":0.9,"top_k":40,"messages":[]}`,
		`{"model":"m","temperature":1,"top_p":0.99,"top_k":40,"thinking":{"type":"adaptive"},"messages":[]}`,
		`{"model":"m","temperature":0.2,"top_p":0.5,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[]}`,
		`{"model":"m","top_k":3,"thinking":{"type":"disabled"}}`,
		`{"temperature":0.5,"thinking":{"type":"auto"}}`,
		`{"model":"m","messages":[]}`,
		`{ "temperature" : 0.5 , "top_p" : 0.9 , "thinking" : { "type" : "adaptive" } }`,
	}
	for _, body := range bodies {
		for _, nativeOwned := range []bool{false, true} {
			want := normalizeClaudeSamplingSequential([]byte(body), nativeOwned)
			got := normalizeClaudeSamplingForUpstream([]byte(body), nativeOwned)
			if !bytes.Equal(got, want) {
				t.Errorf("%s native=%v:\n got  %s\n want %s", body, nativeOwned, got, want)
			}
		}
	}
}
