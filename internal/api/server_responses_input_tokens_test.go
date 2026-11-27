package api

import (
	"net/http"
	"testing"
)

func TestResponsesInputTokensRoutesRegistered(t *testing.T) {
	server := newTestServer(t)
	want := map[string]bool{
		"/v1/responses/input_tokens":                false,
		"/backend-api/codex/responses/input_tokens": false,
	}
	for _, route := range server.engine.Routes() {
		if _, ok := want[route.Path]; ok && route.Method == http.MethodPost {
			want[route.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("POST %s is not registered", path)
		}
	}
}
