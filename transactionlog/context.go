package transactionlog

import "context"

type identityContextKey struct{}
type requestHeaderContextKey struct{}

type requestIdentity struct {
	userID    string
	username  string
	sessionID string
}

// WithIdentity carries authenticated user fields to outgoing HTTP calls so
// their log records can be joined to the inbound transaction by request ID.
func WithIdentity(ctx context.Context, userID, username, sessionID string) context.Context {
	return context.WithValue(ctx, identityContextKey{}, requestIdentity{userID, username, sessionID})
}

// IdentityFromContext returns user ID and username attached by accesslog middleware.
func IdentityFromContext(ctx context.Context) (string, string) {
	identity, _ := ctx.Value(identityContextKey{}).(requestIdentity)
	return identity.userID, identity.username
}

// SessionIDFromContext returns the session ID attached by accesslog middleware.
func SessionIDFromContext(ctx context.Context) string {
	identity, _ := ctx.Value(identityContextKey{}).(requestIdentity)
	return identity.sessionID
}

// WithRequestHeader carries client metadata to downstream log records without
// forwarding those values to the called service.
func WithRequestHeader(ctx context.Context, header RequestHeader) context.Context {
	return context.WithValue(ctx, requestHeaderContextKey{}, header)
}

// RequestHeaderFromContext returns client metadata attached by accesslog.
func RequestHeaderFromContext(ctx context.Context) RequestHeader {
	header, _ := ctx.Value(requestHeaderContextKey{}).(RequestHeader)
	return header
}
