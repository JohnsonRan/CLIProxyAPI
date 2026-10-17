package responses

import (
	"runtime"
	"strings"
	"testing"
)

// The response translator resolves every streamed function call against the
// original request; it must not copy the request each time.
func TestFunctionCallNameResolutionDoesNotCopyTheRequest(t *testing.T) {
	request := []byte(`{"model":"gpt-6","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"input":[` +
		strings.Repeat(`{"type":"message","role":"user","content":"x"},`, 20000) + `{}]}`)
	if name, _ := splitResponsesQualifiedFunctionCallFromRequest(request, "shell", false); name != "shell" {
		t.Fatalf("name = %q", name)
	}
	for name, run := range map[string]func(){
		"splitResponsesQualifiedFunctionCallFromRequest": func() { splitResponsesQualifiedFunctionCallFromRequest(request, "shell", false) },
		"newClaudeToResponsesState":                      func() { newClaudeToResponsesState(request, false) },
	} {
		const runs = 5
		run()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			run()
		}
		runtime.ReadMemStats(&after)
		if allocated := int64(after.TotalAlloc-before.TotalAlloc) / runs; allocated > int64(len(request)/4) {
			t.Errorf("%s allocates %d bytes per call on a %d byte request", name, allocated, len(request))
		}
	}
}
