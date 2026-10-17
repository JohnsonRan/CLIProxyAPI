package thinking

import (
	"strings"
	"testing"
)

// The translator, the translator registry and ApplyThinking each apply the
// summary config; only the first write may copy the body.
func TestApplySummaryConfigDoesNotCopyForAnUnchangedDisplay(t *testing.T) {
	body := []byte(`{"messages":[` + strings.Repeat(`{"role":"user","content":"x"},`, 2000) + `{}],"thinking":{"type":"adaptive"}}`)
	config := SummaryConfig{Mode: SummaryEnabled}
	once := applySummaryConfigForProvider(body, "claude", "claude-opus-5-5", "claude", nil, config)
	if !strings.HasSuffix(string(once), `"thinking":{"type":"adaptive","display":"summarized"}}`) {
		t.Fatalf("first apply: %s", once[len(once)-80:])
	}
	twice := applySummaryConfigForProvider(once, "claude", "claude-opus-5-5", "claude", nil, config)
	if &twice[0] != &once[0] {
		t.Fatal("re-applying the same summary display copied the body")
	}
}
