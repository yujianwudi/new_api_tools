package requestmeta

import "context"

type contextKey string

const requestIDKey contextKey = "request_id"

// WithRequestID stores a validated request identifier in a context so it can
// be propagated through database, Tool Store, and NewAPI adapter calls.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestIDKey, requestID)
}

// RequestID returns the request identifier stored by the HTTP middleware.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}
