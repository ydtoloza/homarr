// Package ctxauth carries the authenticated caller through request contexts.
//
// internal/auth populates it (session cookie or API key); tRPC procedures and
// REST handlers read it. It is a separate leaf package so routers can depend
// on the contract without importing the auth implementation, and so tests can
// inject sessions directly.
package ctxauth

import "context"

// Session is the minimal authenticated-caller identity shared across the API.
// Field names mirror the upstream user/session tables.
type Session struct {
	// ID is the upstream session table id.
	ID string
	// UserID is the upstream user table id.
	UserID string
	// Name is the user display name (may be empty).
	Name string
	// IsAdministrator mirrors the user table's isAdministrator column.
	IsAdministrator bool
	// ReadOnly is true when the caller authenticated with a read-only API
	// key (upstream rejects write procedures with apiKey.readOnly,
	// packages/api/src/trpc.ts:130-135). Session-cookie callers are always
	// false.
	ReadOnly bool
}

type ctxKey struct{}

// WithSession returns ctx carrying s.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the session stored by the auth middleware, or nil when
// the caller is anonymous; procedures decide whether that is an error.
func FromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}
