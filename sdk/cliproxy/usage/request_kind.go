package usage

import "context"

// RequestKindEmbeddings marks usage records produced by embedding endpoints.
const RequestKindEmbeddings = "embeddings"

type requestKindContextKey struct{}

// WithRequestKind tags the request so usage sinks can tell endpoint families
// apart, for example to leave embeddings out of usage statistics.
func WithRequestKind(ctx context.Context, kind string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestKindContextKey{}, kind)
}

// RequestKindFromContext returns the tag set by WithRequestKind, or "".
func RequestKindFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	kind, _ := ctx.Value(requestKindContextKey{}).(string)
	return kind
}
