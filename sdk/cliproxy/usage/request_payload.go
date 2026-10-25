package usage

import "context"

type requestPayloadContextKey struct{}

// WithRequestPayload attaches the client request body of an execution attempt so usage
// plugins can inspect it (for example to keep the prompt of a request the upstream flagged).
// The slice is shared, not copied; callers must not modify it afterwards.
func WithRequestPayload(ctx context.Context, payload []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(payload) == 0 {
		return ctx
	}
	return context.WithValue(ctx, requestPayloadContextKey{}, payload)
}

// RequestPayloadFromContext returns the client request body attached by WithRequestPayload.
// The returned slice must be treated as read-only.
func RequestPayloadFromContext(ctx context.Context) []byte {
	if ctx == nil {
		return nil
	}
	payload, _ := ctx.Value(requestPayloadContextKey{}).([]byte)
	return payload
}
