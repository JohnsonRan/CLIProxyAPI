package usage

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"unsafe"
	"weak"
)

var requestPayloadCapture atomic.Bool

// SetRequestPayloadCapture toggles request body attachment on execution contexts.
// When disabled (default false), WithRequestPayload attaches an empty holder that shadows
// any outer payload, preventing body retention and accidental inheritance.
func SetRequestPayloadCapture(enabled bool) {
	requestPayloadCapture.Store(enabled)
}

// RequestPayloadCapture reports whether request body attachment is enabled.
func RequestPayloadCapture() bool {
	return requestPayloadCapture.Load()
}

// requestPayloadHolder maintains a weak pointer to the request body slice.
// Using a weak pointer ensures that execution contexts held in the asynchronous usage queue
// do not prevent unflagged request bodies from being garbage collected once execution finishes,
// without requiring explicit release points across provider and conductor call paths.
//
// Degradation: If a caller discards all strong references to the request payload before
// Publish runs, the weak pointer will resolve to nil and the flagged event will simply record
// an empty prompt. It will never inherit or attribute another request's prompt.
type requestPayloadHolder struct {
	ptr weak.Pointer[byte]
	len int
}

func (h *requestPayloadHolder) bytes() []byte {
	if h == nil || h.len <= 0 {
		return nil
	}
	p := h.ptr.Value()
	if p == nil {
		return nil
	}
	return unsafe.Slice(p, h.len)
}

type requestPayloadHolderKey struct{}
type requestPayloadSnapshotKey struct{}

// WithRequestPayload attaches the client request body of an execution attempt via a weak
// holder so usage plugins can inspect it if the request is flagged, while allowing the body
// to be collected by GC once the caller's request execution finishes.
// It always attaches a new holder and shadows any outer payload or snapshot, ensuring that
// an outer or previous turn's payload is never inherited even if capture is toggled.
// The slice is shared, not copied; callers must not modify it afterwards.
func WithRequestPayload(ctx context.Context, payload []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	h := &requestPayloadHolder{}
	if requestPayloadCapture.Load() && len(payload) > 0 {
		h.ptr = weak.Make(&payload[0])
		h.len = len(payload)
	}
	ctx = context.WithValue(ctx, requestPayloadSnapshotKey{}, []byte(nil))
	return context.WithValue(ctx, requestPayloadHolderKey{}, h)
}

// RequestPayloadFromContext returns the client request body attached by WithRequestPayload
// or snapshotted by Manager.Publish. The returned slice must be treated as read-only.
func RequestPayloadFromContext(ctx context.Context) []byte {
	if ctx == nil {
		return nil
	}
	if snapshot, ok := ctx.Value(requestPayloadSnapshotKey{}).([]byte); ok && len(snapshot) > 0 {
		return snapshot
	}
	if h, ok := ctx.Value(requestPayloadHolderKey{}).(*requestPayloadHolder); ok && h != nil {
		return h.bytes()
	}
	return nil
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
