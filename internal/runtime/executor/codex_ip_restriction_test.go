package executor

import (
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestCodexTemporaryIPRestrictionClassifiedAs503(t *testing.T) {
	body := []byte(`{"error":{"message":"Your IP is not authorized to make this request.","type":"invalid_request_error"}}`)

	errHTTP := newCodexStatusErrWithCooling(http.StatusUnauthorized, body, false)
	if errHTTP.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("HTTP status = %d, want %d", errHTTP.StatusCode(), http.StatusServiceUnavailable)
	}
	if errHTTP.RetryAfter() == nil || *errHTTP.RetryAfter() != 30*time.Second {
		t.Fatalf("HTTP retryAfter = %v, want 30s", errHTTP.RetryAfter())
	}
	if got := gjson.Get(errHTTP.Error(), "error.code").String(); got != "ip_temporarily_restricted" {
		t.Fatalf("HTTP error.code = %q, want ip_temporarily_restricted", got)
	}

	wsPayload := []byte(`{"type":"error","status":401,"error":{"message":"Your IP is not authorized to make this request."}}`)
	errWS, ok := parseCodexWebsocketErrorWithCooling(wsPayload, false)
	if !ok {
		t.Fatal("expected parseCodexWebsocketErrorWithCooling to return true")
	}
	statusCarrier, okCarrier := errWS.(interface{ StatusCode() int })
	if !okCarrier || statusCarrier.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("WS error = %#v, want StatusCode %d", errWS, http.StatusServiceUnavailable)
	}
}
