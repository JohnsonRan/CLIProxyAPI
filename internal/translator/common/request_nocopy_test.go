package common

import (
	"runtime"
	"strings"
	"testing"
)

func allocatedPerCall(run func()) int64 {
	const runs = 5
	run()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		run()
	}
	runtime.ReadMemStats(&after)
	return int64(after.TotalAlloc-before.TotalAlloc) / runs
}

// Reading a few fields of a large request must not copy the request.
func TestRequestFieldReadersDoNotCopyTheBody(t *testing.T) {
	body := []byte(`{"model":"gpt-6","input":[` + strings.Repeat(`{"type":"message","role":"user","content":"x"},`, 20000) +
		`{}],"metadata":{"user_id":"user-1"}}`)
	if got := RequestModelName(body, nil); got != "gpt-6" {
		t.Fatalf("RequestModelName = %q", got)
	}
	if got := DeriveClaudeUserID(body); got != "user-1" {
		t.Fatalf("DeriveClaudeUserID = %q", got)
	}
	for name, run := range map[string]func(){
		"RequestModelName":   func() { RequestModelName(body, nil) },
		"DeriveClaudeUserID": func() { DeriveClaudeUserID(body) },
	} {
		if allocated := allocatedPerCall(run); allocated > int64(len(body)/4) {
			t.Errorf("%s allocates %d bytes per call on a %d byte body", name, allocated, len(body))
		}
	}
}
