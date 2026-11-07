// Package auth is the Go port of the Homarr authentication core
// (packages/auth): bcrypt credential verification, database-backed sessions
// compatible with Auth.js v5 (next-auth 5.0.0-beta.32) cookies, the
// Auth.js-compatible REST surface the SPA talks to, and a middleware that
// resolves the caller from a session cookie or an API key.
//
// The exact upstream behavior (wire formats, cookie names, field names, with
// upstream file:line citations) is documented in AUTH.md next to this file.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Defaults mirroring the upstream environment (packages/auth/env.ts:29-35 and
// scripts/run.sh:65).
const (
	// DefaultCookiePrefix is AUTH_COOKIE_PREFIX's default. All auth cookies
	// are prefixed so Homarr's cookie jar is isolated from other Auth.js apps
	// on the same hostname (packages/auth/session.ts:11,
	// packages/auth/configuration.ts:27-39).
	DefaultCookiePrefix = "homarr"
	// DefaultSessionTTL is AUTH_SESSION_EXPIRY_TIME's default ("30d",
	// packages/auth/env.ts:29).
	DefaultSessionTTL = 30 * 24 * time.Hour
	// basePath is the URL prefix the REST handler serves; upstream mounts
	// next-auth at /api/auth (apps/nextjs/src/app/api/auth/[...nextauth]/route.ts).
	basePath = "/api/auth"
	// apiKeyHeader is the header API keys travel in
	// (packages/auth/api-key/constants.ts:1).
	apiKeyHeader = "ApiKey"
	// GroupEveryone is the group every signing-in user joins
	// (packages/definitions/src/group.ts:1, packages/auth/events.ts:47).
	GroupEveryone = "everyone"
	// colorSchemeCookieKey is set at sign-in so the SPA knows the preferred
	// color scheme before localStorage loads (packages/auth/events.ts:88-91).
	colorSchemeCookieKey = "homarr.color-scheme"
)

// Options configure the Auth service. Zero values fall back to the upstream
// defaults listed above.
type Options struct {
	// CookiePrefix overrides AUTH_COOKIE_PREFIX (default "homarr").
	CookiePrefix string
	// Secret is the AUTH_SECRET equivalent, used to HMAC-less-hash the CSRF
	// cookie (upstream: sha256(csrfToken + secret), packages/core/src/lib/
	// actions/callback/oauth/csrf-token.ts). Upstream generates it at container
	// start (scripts/run.sh:65); it only needs to be stable for the lifetime
	// of the csrf cookie.
	Secret string
	// Secure enables Secure cookies and the __Host- CSRF cookie prefix. Upstream
	// derives this from the request URL protocol being https
	// (apps/nextjs/src/app/api/auth/[...nextauth]/route.ts:25-28).
	Secure bool
	// SessionTTL is how long a session row stays valid
	// (AUTH_SESSION_EXPIRY_TIME, default 30 days). It is fixed at session
	// creation; upstream never refreshes it (packages/auth/session.ts:44).
	SessionTTL time.Duration
	// Now overrides the clock for tests. nil means time.Now.
	Now func() time.Time
}

func (o *Options) cookiePrefix() string {
	if o.CookiePrefix != "" {
		return o.CookiePrefix
	}
	return DefaultCookiePrefix
}

func (o *Options) sessionTTL() time.Duration {
	if o.SessionTTL > 0 {
		return o.SessionTTL
	}
	return DefaultSessionTTL
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Auth provides session management, credential verification and the
// Auth.js-compatible REST handler. It carries no mutable state beyond the
// options; it is safe for concurrent use.
type Auth struct {
	db   *db.DB
	opts Options
}

// New builds an Auth service on top of the shared database handle (db.DB
// embeds *sql.DB, so no extra accessor is needed).
func New(database *db.DB, opts Options) *Auth {
	return &Auth{db: database, opts: opts}
}

// Session-token cookie name: AUTH_COOKIE_PREFIX + ".session-token"
// (packages/auth/session.ts:11). Note the name deliberately has no __Secure-
// prefix even on https, unlike the other auth cookies
// (packages/auth/configuration.ts:27-39).
func (a *Auth) sessionCookieName() string { return a.opts.cookiePrefix() + ".session-token" }

// CSRF cookie name: [__Host-]prefix.csrf-token (packages/auth/configuration.ts:33).
func (a *Auth) csrfCookieName() string {
	hostPrefix := ""
	if a.opts.Secure {
		hostPrefix = "__Host-"
	}
	return hostPrefix + a.opts.cookiePrefix() + ".csrf-token"
}

// GenerateSessionToken ports generateSessionToken
// (packages/auth/session.ts:17-19): generateSecureRandomToken(48) = 48 random
// bytes hex-encoded = 96 characters (packages/common/src/security.ts:8-10).
func GenerateSessionToken() (string, error) {
	return randomHex(48)
}

// randomHex returns size random bytes hex-encoded.
func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateSession inserts a session row for the user and returns the token
// that must travel in the session cookie. Expiry is fixed at creation time
// (now + SessionTTL), mirroring the upstream signIn callback
// (packages/auth/configuration.ts:86-92) and the adapter's createSession.
func (a *Auth) CreateSession(ctx context.Context, userID string) (Session, error) {
	token, err := GenerateSessionToken()
	if err != nil {
		return Session{}, err
	}
	s := Session{Token: token, UserID: userID, Expires: a.opts.now().Add(a.opts.sessionTTL())}
	if err := a.db.CreateSession(ctx, db.Session{Token: s.Token, UserID: s.UserID, Expires: s.Expires}); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Session is the auth-layer view of a validated session.
type Session struct {
	Token   string
	UserID  string
	Expires time.Time
}

// sessionCookie builds the session-token cookie as upstream sets it in the
// signIn callback (packages/auth/configuration.ts:94-100): path=/, expires at
// session expiry, httpOnly, SameSite Lax, Secure when https.
func (a *Auth) sessionCookie(s Session) *http.Cookie {
	return &http.Cookie{
		Name:     a.sessionCookieName(),
		Value:    s.Token,
		Path:     "/",
		Expires:  s.Expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.opts.Secure,
	}
}

// clearSessionCookie builds the cookie that deletes the session cookie on the
// browser, the way SessionStore.clean does upstream (value "" + maxAge=0,
// packages/core/src/lib/utils/cookie.ts).
func (a *Auth) clearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     a.sessionCookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1, // serialized as "Max-Age=0"
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.opts.Secure,
	}
}

// ValidateSession resolves a session token to a session row, enforcing
// expiry the way upstream reads sessions (packages/auth/session.ts:44:
// expires <= now means invalid). Expired tokens return ErrNotFound so callers
// treat them like unknown tokens; upstream additionally removes the stale
// row only from the cookie jar, the DB row is left to expire naturally.
func (a *Auth) ValidateSession(ctx context.Context, token string) (Session, error) {
	s, err := a.db.GetSession(ctx, token)
	if err != nil {
		return Session{}, err
	}
	if !s.Session.Expires.After(a.opts.now()) {
		return Session{}, db.ErrNotFound
	}
	return Session{Token: s.Session.Token, UserID: s.Session.UserID, Expires: s.Session.Expires}, nil
}

// DeleteSession removes the session row (sign-out; upstream adapter
// deleteSession called from the signOut action, packages/core/src/lib/actions/
// signout.ts).
func (a *Auth) DeleteSession(ctx context.Context, token string) error {
	return a.db.DeleteSession(ctx, token)
}

// ResolveAPIKey validates an "id.token" ApiKey header value and returns the
// caller identity. It mirrors getSessionFromApiKeyAsync
// (packages/auth/api-key/get-api-key-session.ts:22-82): split on the dot,
// look up the row by id, bcrypt-compare the token. Malformed headers, unknown
// ids and wrong tokens all yield (nil, nil), which the middleware turns into
// the plain 401 {"error":"UNAUTHORIZED"} of the upstream route handler. Like
// upstream's createSessionAsync (packages/auth/callbacks.ts:23-36), which
// builds an in-memory session with expires = now+1d, no session row is
// written.
//
// Seam: upstream also attaches apiKey.readOnly to the session so tRPC
// ResolveAPIKey validates an "ApiKey" header value ("<id>.<token>") and
// builds the caller session for it. The key's readOnly flag is propagated
// into ctxauth.Session.ReadOnly; write-procedure enforcement is pending
// (see AUTH.md §6).
func (a *Auth) ResolveAPIKey(ctx context.Context, headerValue string) (*ctxauth.Session, error) {
	id, token, ok := strings.Cut(headerValue, ".")
	if !ok || id == "" || token == "" || strings.Contains(token, ".") {
		return nil, nil // malformed: treated like an invalid key upstream
	}
	key, err := a.db.GetAPIKeyByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if !ComparePassword(token, key.Hash) {
		return nil, nil
	}
	sess, err := a.callerSession(ctx, key.UserID)
	if err != nil {
		return nil, err
	}
	sess.ReadOnly = key.ReadOnly
	return sess, nil
}

// callerSession builds the ctxauth.Session for a user, including the
// administrator flag derived from the "admin" group permission.
func (a *Auth) callerSession(ctx context.Context, userID string) (*ctxauth.Session, error) {
	isAdmin, err := a.db.UserHasPermission(ctx, userID, permissionAdmin)
	if err != nil {
		return nil, err
	}
	return &ctxauth.Session{UserID: userID, IsAdministrator: isAdmin}, nil
}

// Middleware resolves the caller before the next handler runs and stores it
// in the request context via ctxauth.WithSession. Resolution order mirrors
// the upstream tRPC route handler (apps/nextjs/src/app/api/trpc/[trpc]/
// route.ts:34-46):
//
//   - an "ApiKey" header that fails validation answers 401 with the plain
//     JSON body {"error":"UNAUTHORIZED"} (route.ts:41-45) — never fall
//     through to other mechanisms in that case;
//   - otherwise the session cookie decides; anonymous requests pass through
//     with no session in the context, procedures enforce their own authz
//     (upstream isSignedIn/isAdministrator are tRPC middlewares, see AUTH.md
//     §5).
//
// Mount it on the API surfaces that need caller resolution (tRPC), not on
// /api/auth/* itself.
func (a *Auth) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiKey := r.Header.Get(apiKeyHeader); apiKey != "" {
				sess, err := a.ResolveAPIKey(r.Context(), apiKey)
				if err != nil {
					http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
					return
				}
				if sess == nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"UNAUTHORIZED"}`))
					return
				}
				next.ServeHTTP(w, r.WithContext(ctxauth.WithSession(r.Context(), sess)))
				return
			}

			if token := cookieValue(r, a.sessionCookieName()); token != "" {
				s, err := a.ValidateSession(r.Context(), token)
				if err != nil && !errors.Is(err, db.ErrNotFound) {
					http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
					return
				}
				if err == nil {
					sess, err := a.callerSession(r.Context(), s.UserID)
					if err != nil {
						http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
						return
					}
					next.ServeHTTP(w, r.WithContext(ctxauth.WithSession(r.Context(), sess)))
					return
				}
			}

			// Anonymous: no session in the context; procedures decide whether
			// that is an error, mirroring upstream's publicProcedure.
			next.ServeHTTP(w, r)
		})
	}
}

// cookieValue returns the named cookie value from the request, "" when
// absent. (Cookie chunking, which Auth.js applies above ~3.9 KiB
// (packages/core/src/lib/utils/cookie.ts SessionStore), never triggers for a
// 96-char session token.)
func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
