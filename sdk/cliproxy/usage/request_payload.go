package usage

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
)

var requestPayloadCapture atomic.Bool

// SetRequestPayloadCapture toggles request body attachment on execution contexts.
// When disabled (default false), WithRequestPayload is a no-op, preventing body retention.
func SetRequestPayloadCapture(enabled bool) {
	requestPayloadCapture.Store(enabled)
}

// RequestPayloadCapture reports whether request body attachment is enabled.
func RequestPayloadCapture() bool {
	return requestPayloadCapture.Load()
}

type requestPayloadContextKey struct{}

// WithRequestPayload attaches the client request body of an execution attempt so usage
// plugins can inspect it (for example to keep the prompt of a request the upstream flagged).
// When capture is disabled, ctx is returned unchanged.
// When payload is empty, any previously attached payload is cleared.
// The slice is shared, not copied; callers must not modify it afterwards.
func WithRequestPayload(ctx context.Context, payload []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !requestPayloadCapture.Load() {
		return ctx
	}
	if len(payload) == 0 {
		return DropRequestPayload(ctx)
	}
	return context.WithValue(ctx, requestPayloadContextKey{}, payload)
}

// DropRequestPayload wraps ctx so RequestPayloadFromContext returns nil, releasing
// access to the request body before the context is queued asynchronously.
func DropRequestPayload(ctx context.Context) context.Context {
	if ctx == nil || RequestPayloadFromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, requestPayloadContextKey{}, []byte(nil))
}

// RequestPayloadFromContext returns the client request body attached by WithRequestPayload.
// The returned slice must be treated as read-only.
func RequestPayloadFromContext(ctx context.Context) []byte {
	if ctx == nil {
		return nil
	}
	payload, _ := ctx.Value(requestPayloadContextKey{}).([]byte)
	if len(payload) == 0 {
		return nil
	}
	return payload
}

// canBeRiskRecord is a lightweight caller-side superset check before enqueuing a record.
// If the record cannot possibly be a safety/risk event, the request body is dropped from ctx.
func canBeRiskRecord(record Record) bool {
	if strings.TrimSpace(record.UpstreamRisk) != "" {
		return true
	}
	if record.Failed || record.Fail.StatusCode >= 400 {
		body := record.Fail.Body
		if strings.Contains(body, "cyber_policy") ||
			strings.Contains(body, "bio_policy") ||
			strings.Contains(body, "invalid_prompt") {
			return true
		}
	}
	respModel := strings.ToLower(strings.TrimSpace(record.ResponseModel))
	if respModel != "" {
		model := strings.ToLower(strings.TrimSpace(record.Model))
		upstreamModel := strings.ToLower(strings.TrimSpace(record.UpstreamModel))
		if respModel != model && (upstreamModel == "" || respModel != upstreamModel) {
			return true
		}
	}
	if headers := http.Header(record.ResponseHeaders); headers != nil {
		if strings.EqualFold(headers.Get("X-Codex-Safety-Buffering-Enabled"), "true") {
			return true
		}
	}
	return false
}
