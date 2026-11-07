package api

import (
	"net/http"
	"testing"
)

func TestDecisionsRouteRegistered(t *testing.T) {
	server := newTestServer(t)
	for _, route := range server.engine.Routes() {
		if route.Path == "/v1/decisions" && route.Method == http.MethodPost {
			return
		}
	}
	t.Fatal("POST /v1/decisions is not registered")
}
