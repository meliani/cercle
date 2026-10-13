package auth

import "context"

type ctxKey struct{}

// WithUser stores the authenticated username in ctx.
func WithUser(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, ctxKey{}, username)
}

// User returns the authenticated username ("" if none).
func User(ctx context.Context) string {
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}
