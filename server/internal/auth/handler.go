package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// Handler serves the subset of the Auth.js v5 (next-auth 5.0.0-beta.32) REST
// surface the Homarr SPA calls (see AUTH.md §4 for the wire contract and
// upstream citations). The integrator mounts it at /api/auth/*:
//
//	router.Handle("/api/auth/*", a.Handler())
//
// Endpoints:
//
//	GET  /api/auth/csrf                  -> {csrfToken} + Set-Cookie
//	GET  /api/auth/providers             -> {credentials: {...}}
//	GET  /api/auth/session               -> {user, expires} | null
//	POST /api/auth/callback/credentials  -> {url} (login) + Set-Cookie
//	POST /api/auth/signout               -> {url} (logout) + cookie cleared
//
// Unknown auth actions (or known paths with the wrong method) answer 400
// "Bad request." like @auth/core does for unparseable requests
// (packages/core/src/index.ts: toInternalRequest falsy -> Response.json(
// `Bad request.`, { status: 400 })). The SPA only uses the five endpoints
// above; signin/<provider> for oidc/ldap and the error/verify-request pages
// are out of scope for M1.
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/csrf", a.handleCSRF)
	mux.HandleFunc("GET /api/auth/providers", a.handleProviders)
	mux.HandleFunc("GET /api/auth/session", a.handleSession)
	mux.HandleFunc("POST /api/auth/callback/credentials", a.handleCredentialsCallback)
	mux.HandleFunc("POST /api/auth/signout", a.handleSignout)
	// Catch-all for unknown actions and wrong methods (more specific patterns
	// above win over this one).
	mux.HandleFunc("/api/auth/", func(w http.ResponseWriter, r *http.Request) {
		httpJSON(w, http.StatusBadRequest, "Bad request.")
	})
	return mux
}

func httpJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers are already sent; nothing else can be done on the wire.
		_ = err
	}
}

// noCacheHeaders mirrors the "private, no-cache, no-store" trio the session
// and csrf endpoints send (packages/core/src/lib/pages/index.ts renderPage,
// packages/core/src/lib/actions/session.ts).
func noCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-cache, no-store")
	w.Header().Set("Expires", "0")
	w.Header().Set("Pragma", "no-cache")
}

// returnRedirectHeader is the header next-auth/react always sends to ask for
// a JSON response instead of an HTTP redirect (packages/next-auth/src/
// react.tsx signIn/signOut fetch headers).
const returnRedirectHeader = "X-Auth-Return-Redirect"

// respondRedirect mirrors @auth/core's redirect rendering: with
// X-Auth-Return-Redirect the response is JSON {url} (Set-Cookie headers
// added before this call are preserved); without it, a 302 redirect.
func respondRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get(returnRedirectHeader) != "" {
		httpJSON(w, http.StatusOK, map[string]string{"url": target})
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleCSRF mints a fresh token: JSON {csrfToken} plus the csrf cookie
// (upstream render.csrf in packages/core/src/lib/pages/index.ts: body
// {csrfToken}, no-cache headers; the cookie is set by init() with the
// default httpOnly/sameSite/path options).
func (a *Auth) handleCSRF(w http.ResponseWriter, r *http.Request) {
	token, cookieValue, err := a.newCSRFToken()
	if err != nil {
		http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     a.csrfCookieName(),
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.opts.Secure,
	})
	noCacheHeaders(w)
	httpJSON(w, http.StatusOK, map[string]string{"csrfToken": token})
}

// providerJSON is the public provider shape the GET providers endpoint
// returns (packages/core/src/lib/pages/index.ts render.providers whitelist:
// id, name, type, signinUrl, callbackUrl).
type providerJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	SigninURL   string `json:"signinUrl"`
	CallbackURL string `json:"callbackUrl"`
}

// handleProviders lists the enabled providers. M1 supports credentials only
// (upstream filterProviders adds ldap and oidc depending on env).
func (a *Auth) handleProviders(w http.ResponseWriter, r *http.Request) {
	origin := a.origin(r)
	httpJSON(w, http.StatusOK, map[string]providerJSON{
		"credentials": {
			ID:          "credentials",
			Name:        "Credentials",
			Type:        "credentials",
			SigninURL:   origin + basePath + "/signin/credentials",
			CallbackURL: origin + basePath + "/callback/credentials",
		},
	})
}

// handleSession renders the session the way the SPA's useSession consumes it:
// the createSessionCallback shape (packages/auth/callbacks.ts:38-62) or the
// JSON literal null when unauthenticated (packages/core/src/lib/actions/
// session.ts: body stays null without a session token). An expired/unknown
// session additionally clears the stale cookie (sessionStore.clean()).
func (a *Auth) handleSession(w http.ResponseWriter, r *http.Request) {
	noCacheHeaders(w)
	token := cookieValue(r, a.sessionCookieName())
	if token == "" {
		httpJSON(w, http.StatusOK, nil) // JSON null, HTTP 200 (Auth.js v5)
		return
	}
	s, err := a.db.GetSession(r.Context(), token)
	if err != nil || !s.Session.Expires.After(a.opts.now()) {
		// Unknown or expired session: same null response, plus cookie cleanup.
		http.SetCookie(w, a.clearSessionCookie())
		httpJSON(w, http.StatusOK, nil)
		return
	}
	user := s.User
	permissions, err := a.db.UserPermissions(r.Context(), user.ID)
	if err != nil {
		http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
		return
	}
	httpJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id":          user.ID,
			"name":        user.Name,
			"email":       user.Email,
			"image":       user.Image,
			"colorScheme": user.ColorScheme,
			"permissions": expandPermissions(permissions),
		},
		"expires": s.Session.Expires.UTC().Format(time.RFC3339Nano),
	})
}

// handleCredentialsCallback implements POST /api/auth/callback/credentials,
// the login endpoint (packages/core/src/lib/actions/callback/index.ts
// credentials branch + the signIn callback in packages/auth/configuration.ts
// + the signIn event in packages/auth/events.ts). Steps:
//
//  1. CSRF verification (double submit; MissingCSRF -> 400 JSON null).
//  2. Parse form fields: name and password (userSignInSchema in
//     packages/validation/src/user.ts:91-94) plus callbackUrl.
//  3. authorize: user lookup (lowercased name, provider "credentials") +
//     bcrypt compare (packages/auth/providers/credentials/authorization/
//     basic-authorization.ts:13-41). Any failure redirects to the error page
//     /auth/login?error=CredentialsSignin&code=credentials.
//  4. Success: create the session row + session cookie (signIn callback in
//     configuration.ts:73-103) and run the signIn event side-effects that
//     affect the credentials flow: join the "everyone" group and set the
//     color-scheme cookie (packages/auth/events.ts:47,88-91).
//  5. Respond {url: callbackUrl} or 302 to it (same-origin enforced).
func (a *Auth) handleCredentialsCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpJSON(w, http.StatusBadRequest, nil)
		return
	}
	if !a.verifyCSRF(cookieValue(r, a.csrfCookieName()), r.PostFormValue("csrfToken")) {
		// Upstream: validateCSRF -> MissingCSRF -> the catch block returns
		// Response.json(null, { status: 400 }) (packages/core/src/index.ts).
		httpJSON(w, http.StatusBadRequest, nil)
		return
	}

	name := r.PostFormValue("name")
	password := r.PostFormValue("password")
	if name == "" || password == "" {
		// The SPA validates with the same zod schema before submitting
		// (packages/validation/src/user.ts:91-94); treat empty credentials
		// like a failed authorize.
		a.respondCredentialsError(w, r)
		return
	}

	user, ok := a.authorizeCredentials(r.Context(), name, password)
	if !ok {
		a.respondCredentialsError(w, r)
		return
	}

	s, err := a.CreateSession(r.Context(), user.ID)
	if err != nil {
		http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.sessionCookie(s))
	a.applySignInEvent(w, r, user)

	respondRedirect(w, r, a.callbackURL(r))
}

// respondCredentialsError redirects to the configured error page with
// error=CredentialsSignin&code=credentials. Upstream: authorize returns null
// -> CredentialsSignin thrown -> the catch builds params {error: type} +
// {code: error.code} and redirects to pages.error = "/auth/login"
// (packages/auth/configuration.ts:114-117, packages/core/src/errors.ts:
// CredentialsSignin.code = "credentials", packages/core/src/index.ts catch).
func (a *Auth) respondCredentialsError(w http.ResponseWriter, r *http.Request) {
	respondRedirect(w, r, a.origin(r)+"/auth/login?error=CredentialsSignin&code=credentials")
}

// authorizeCredentials ports authorizeWithBasicCredentialsAsync
// (packages/auth/providers/credentials/authorization/basic-authorization.ts):
// user by lowercased name + provider "credentials", then bcrypt compare of
// the stored hash. Any failure collapses to ok=false exactly like the
// upstream null return (the wire never distinguishes unknown user from wrong
// password).
func (a *Auth) authorizeCredentials(ctx context.Context, name, password string) (db.User, bool) {
	user, err := a.db.GetUserByName(ctx, strings.ToLower(name), "credentials")
	if err != nil || user.Password == "" {
		return db.User{}, false
	}
	if !ComparePassword(password, user.Password) {
		return db.User{}, false
	}
	return user, true
}

// applySignInEvent ports the parts of the upstream signIn event
// (packages/auth/events.ts) that affect the credentials flow: add the user to
// the "everyone" group (best effort, like upstream where event handler errors
// are logged and swallowed by eventsErrorHandler) and set the color-scheme
// cookie for one year (events.ts:88-91, packages/definitions/src/cookie.ts:1).
func (a *Auth) applySignInEvent(w http.ResponseWriter, r *http.Request, user db.User) {
	if groupID, err := a.db.GroupIDByName(r.Context(), GroupEveryone); err == nil {
		_, _ = a.db.AddUserToGroup(r.Context(), user.ID, groupID)
	}
	http.SetCookie(w, &http.Cookie{
		Name:    colorSchemeCookieKey,
		Value:   user.ColorScheme,
		Path:    "/",
		Expires: a.opts.now().AddDate(1, 0, 0),
	})
}

// handleSignout implements POST /api/auth/signout (packages/core/src/lib/
// actions/signout.ts): CSRF check, delete the session row (database
// strategy: adapter.deleteSession), clear the cookie (SessionStore.clean:
// value "" + maxAge=0), respond {url: callbackUrl} / 302.
func (a *Auth) handleSignout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpJSON(w, http.StatusBadRequest, nil)
		return
	}
	if !a.verifyCSRF(cookieValue(r, a.csrfCookieName()), r.PostFormValue("csrfToken")) {
		httpJSON(w, http.StatusBadRequest, nil)
		return
	}
	if token := cookieValue(r, a.sessionCookieName()); token != "" {
		if err := a.DeleteSession(r.Context(), token); err != nil {
			http.Error(w, `{"error":"INTERNAL"}`, http.StatusInternalServerError)
			return
		}
	}
	http.SetCookie(w, a.clearSessionCookie())
	respondRedirect(w, r, a.callbackURL(r))
}

// callbackURL resolves the callbackUrl form field with the default redirect
// callback (packages/core/src/lib/init.ts default callbacks): relative URLs
// join the request origin, absolute URLs must be same-origin or fall back to
// the origin.
func (a *Auth) callbackURL(r *http.Request) string {
	origin := a.origin(r)
	raw := r.PostFormValue("callbackUrl")
	if raw == "" {
		return origin
	}
	if strings.HasPrefix(raw, "/") {
		return origin + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" && originOf(u) == origin {
		return raw
	}
	return origin
}

// origin mirrors what @auth/core sees as url.origin. trustHost is enabled
// upstream (packages/auth/configuration.ts:61), so the request Host decides;
// behind TLS-terminating proxies X-Forwarded-Proto decides the scheme
// (upstream rewrites the origin with it in apps/nextjs/src/app/api/auth/
// [...nextauth]/route.ts:57-69).
func (a *Auth) origin(r *http.Request) string {
	scheme := "http"
	if a.opts.Secure {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

func originOf(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}
