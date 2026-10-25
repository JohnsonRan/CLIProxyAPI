package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Usage plugins read the client request from the execution context to keep the prompt of
// requests the upstream flagged; every execute path builds its context through this helper.
func TestContextWithRequestedModelAliasCarriesRequestPayload(t *testing.T) {
	payload := []byte(`{"input":"hello"}`)
	ctx := contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{OriginalRequest: payload}, "m")
	if got := coreusage.RequestPayloadFromContext(ctx); string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	if got := coreusage.RequestPayloadFromContext(contextWithRequestedModelAlias(context.Background(), cliproxyexecutor.Options{}, "m")); got != nil {
		t.Fatalf("empty request must not attach a payload, got %q", got)
	}
}
