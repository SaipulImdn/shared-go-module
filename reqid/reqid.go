// Package reqid provides a single, shared convention for the distributed
// tracing ID that flows through every service: axe-gateway-pixora
// generates or forwards it as the "X-Request-ID" header on every proxied
// request; each backend service should read it (or generate one if it's
// missing, e.g. for a direct/internal call that didn't come through the
// gateway), log it on every request, and forward it on any outbound call it
// makes to another service — so a single ID can be grepped across
// every service's logs to reconstruct one request's full path.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// Header is the HTTP header name carrying the request ID between services.
const Header = "X-Request-ID"

// idLen is the byte length of the random portion (16 bytes = 32 hex chars).
const idLen = 16

// Generate produces a random 32-character hex request ID.
func Generate() string {
	buf := make([]byte, idLen)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// FromHeaderOrGenerate returns headerValue if non-empty, otherwise a freshly
// generated ID. Use this at the point a request enters a service (HTTP
// middleware, a queue consumer, a cron job) to decide the ID for that unit
// of work.
func FromHeaderOrGenerate(headerValue string) string {
	if headerValue != "" {
		return headerValue
	}
	return Generate()
}

// contextKey is unexported so it can never collide with keys from other packages.
type contextKey struct{}

// WithID returns a context carrying the request ID.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext extracts the request ID from context, or "" if none is set.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
