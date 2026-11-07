package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// handlerHarness wires the Auth service and its REST handler on an
// httptest.Server so Set-Cookie handling runs for real.
type handlerHarness struct {
	server *httptest.Server
	auth   *Auth
	db     *db.DB
	now    *time.Time
	client *http.Client
}

func newHandlerHarness(t *testing.T) *handlerHarness {
	t.Helper()
	a, d, now := newTestAuth(t)
	server := httptest.NewServer(a.Handler())
	t.Cleanup(server.Close)
	return &handlerHarness{
		server: server,
		auth:   a,
		db:     d,
		now:    now,
		// Never follow redirects: the tests assert the raw wire behavior.
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// get issues a GET carrying the given cookies.
func (h *handlerHarness) get(t *testing.T, path string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// postForm issues a POST with a form-encoded body, the way next-auth/react
// does (Content-Type application/x-www-form-urlencoded + X-Auth-Return-
// Redirect: 1).
func (h *handlerHarness) postForm(t *testing.T, path string, fields url.Values, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Return-Redirect", "1")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func cookieByName(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// fetchCSRF performs GET /csrf and returns the token plus the csrf cookie.
func (h *handlerHarness) fetchCSRF(t *testing.T) (string, *http.Cookie) {
	t.Helper()
	res := h.get(t, "/api/auth/csrf")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET csrf status = %d, want 200", res.StatusCode)
	}
	var body struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode csrf body: %v", err)
	}
	cookie := cookieByName(res, h.auth.csrfCookieName())
	if cookie == nil {
		t.Fatalf("csrf cookie %q not set", h.auth.csrfCookieName())
	}
	if body.CSRFToken == "" {
		t.Fatal("empty csrfToken in response")
	}
	// Cookie value = "token|sha256hex(token+secret)".
	want := h.auth.csrfCookieValue(body.CSRFToken)
	if cookie.Value != want {
		t.Errorf("csrf cookie value = %q, want %q", cookie.Value, want)
	}
	if cookie.Path != "/" || !cookie.HttpOnly {
		t.Errorf("csrf cookie flags: path=%q httpOnly=%v", cookie.Path, cookie.HttpOnly)
	}
	return body.CSRFToken, cookie
}

func (h *handlerHarness) login(t *testing.T, name, password string) (*http.Response, *http.Cookie) {
	t.Helper()
	token, csrfCookie := h.fetchCSRF(t)
	fields := url.Values{
		"csrfToken":   {token},
		"callbackUrl": {h.server.URL + "/"},
		"name":        {name},
		"password":    {password},
	}
	res := h.postForm(t, "/api/auth/callback/credentials", fields, csrfCookie)
	return res, cookieByName(res, h.auth.sessionCookieName())
}

func TestHandlerLoginFlow(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "s3cret", false)

	res, sessionCookie := h.login(t, "admin", "s3cret")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", res.StatusCode)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if body.URL != h.server.URL+"/" {
		t.Errorf("login url = %q, want callbackUrl %q", body.URL, h.server.URL+"/")
	}
	if sessionCookie == nil {
		t.Fatal("session cookie not set on login success")
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" {
		t.Errorf("session cookie flags: %+v", sessionCookie)
	}
	if !sessionCookie.Expires.After(*h.now) {
		t.Errorf("session cookie expires %v not in the future", sessionCookie.Expires)
	}
	// The color-scheme cookie of the signIn event is also set.
	if cs := cookieByName(res, colorSchemeCookieKey); cs == nil || cs.Value != "dark" {
		t.Errorf("color-scheme cookie = %+v, want dark", cs)
	}
	// And the user joined the "everyone" group when it exists (it does not in
	// the bare schema; the signIn event must not fail the login either way).

	// GET /session with the session cookie renders the session.
	sess := h.get(t, "/api/auth/session", sessionCookie)
	if sess.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want 200", sess.StatusCode)
	}
	var sessionBody struct {
		User struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Email       string   `json:"email"`
			Image       string   `json:"image"`
			ColorScheme string   `json:"colorScheme"`
			Permissions []string `json:"permissions"`
		} `json:"user"`
		Expires string `json:"expires"`
	}
	if err := json.NewDecoder(sess.Body).Decode(&sessionBody); err != nil {
		t.Fatalf("decode session body: %v", err)
	}
	if sessionBody.User.ID != "u1" || sessionBody.User.Name != "admin" ||
		sessionBody.User.Email != "admin@example.com" || sessionBody.User.ColorScheme != "dark" {
		t.Errorf("session user mismatch: %+v", sessionBody.User)
	}
	if len(sessionBody.User.Permissions) != 0 {
		t.Errorf("permissions = %v, want empty", sessionBody.User.Permissions)
	}
	if expires, err := time.Parse(time.RFC3339Nano, sessionBody.Expires); err != nil {
		t.Fatalf("session expires %q not RFC3339: %v", sessionBody.Expires, err)
	} else if !expires.Equal(sessionCookie.Expires.UTC()) {
		t.Errorf("session expires = %v, want %v", expires, sessionCookie.Expires.UTC())
	}

	// POST /signout clears the cookie and deletes the row.
	token, csrfCookie := h.fetchCSRF(t)
	signout := h.postForm(t, "/api/auth/signout", url.Values{
		"csrfToken":   {token},
		"callbackUrl": {h.server.URL + "/"},
	}, csrfCookie, sessionCookie)
	if signout.StatusCode != http.StatusOK {
		t.Fatalf("signout status = %d, want 200", signout.StatusCode)
	}
	cleared := cookieByName(signout, h.auth.sessionCookieName())
	// Note: net/http parses "Max-Age=0" back as MaxAge=-1 ("delete now").
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("signout cookie = %+v, want empty value with Max-Age=0", cleared)
	}

	// The session is gone: the endpoint answers null...
	after := h.get(t, "/api/auth/session", sessionCookie)
	var nullBody any
	if err := json.NewDecoder(after.Body).Decode(&nullBody); err != nil || nullBody != nil {
		t.Errorf("session after signout = (%v, %v), want null", nullBody, err)
	}
	// ...and the middleware no longer resolves the caller.
	probe, _ := runMiddleware(h.auth, sessionCookie, "")
	if probe.session != nil {
		t.Errorf("middleware still resolves deleted session: %+v", probe.session)
	}
}

func TestHandlerLoginEveryoneGroupSideEffect(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "pw", false)
	// Seed the "everyone" group like the upstream migration seed does.
	if _, err := h.db.ExecContext(context.Background(),
		`INSERT INTO "group" (id, name, owner_id, position) VALUES ('g-everyone', 'everyone', NULL, -1)`); err != nil {
		t.Fatalf("insert everyone group: %v", err)
	}

	res, _ := h.login(t, "admin", "pw")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", res.StatusCode)
	}
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM "groupMember" WHERE user_id = 'u1' AND group_id = 'g-everyone'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("everyone group membership = (%d, %v), want (1, nil)", n, err)
	}
}

func TestHandlerLoginWrongPassword(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "s3cret", false)

	for _, tc := range []struct{ name, password string }{
		{"admin", "wrong"},
		{"ghost", "s3cret"}, // unknown user
		{"", "s3cret"},      // empty name
		{"admin", ""},       // empty password
	} {
		res, sessionCookie := h.login(t, tc.name, tc.password)
		if res.StatusCode != http.StatusOK {
			t.Errorf("login(%s) status = %d, want 200", tc.name, res.StatusCode)
			continue
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Errorf("decode login error body: %v", err)
			continue
		}
		want := h.server.URL + "/auth/login?error=CredentialsSignin&code=credentials"
		if body.URL != want {
			t.Errorf("login(%s) url = %q, want %q", tc.name, body.URL, want)
		}
		if sessionCookie != nil {
			t.Errorf("login(%s) set a session cookie %+v, want none", tc.name, sessionCookie)
		}
	}
}

func TestHandlerLoginLowercasesName(t *testing.T) {
	h := newHandlerHarness(t)
	// Upstream stores user names lowercased and looks them up lowercased
	// (basic-authorization.ts:19).
	seedUser(t, h.db, "u1", "john", "pw", false)
	res, cookie := h.login(t, "JOHN", "pw")
	if res.StatusCode != http.StatusOK || cookie == nil {
		t.Fatalf("login status=%d cookie=%+v, want 200 with session cookie", res.StatusCode, cookie)
	}
	var body struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body.URL != h.server.URL+"/" {
		t.Errorf("login url = %q, want callback", body.URL)
	}
}

func TestHandlerLoginRequiresCSRF(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "s3cret", false)

	// No csrf cookie at all.
	res := h.postForm(t, "/api/auth/callback/credentials", url.Values{
		"csrfToken": {"whatever"}, "name": {"admin"}, "password": {"s3cret"},
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status without csrf = %d, want 400", res.StatusCode)
	}

	// Cookie present but body token mismatched (double-submit check).
	token, csrfCookie := h.fetchCSRF(t)
	res = h.postForm(t, "/api/auth/callback/credentials", url.Values{
		"csrfToken": {"not-" + token}, "name": {"admin"}, "password": {"s3cret"},
	}, csrfCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status with mismatched csrf = %d, want 400", res.StatusCode)
	}

	// Cookie tampered: valid token, hash re-signed with a different secret ->
	// the double-submit pair matches but the cookie is not ours -> invalid.
	_, csrfCookie = h.fetchCSRF(t)
	tamperedToken, _, _ := strings.Cut(csrfCookie.Value, "|")
	evil := sha256.Sum256([]byte(tamperedToken + "evil-secret"))
	tampered := h.auth.csrfCookieName() + "=" + tamperedToken + "|" + hex.EncodeToString(evil[:])
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/auth/callback/credentials",
		strings.NewReader(url.Values{"csrfToken": {tamperedToken}, "name": {"admin"}, "password": {"s3cret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Return-Redirect", "1")
	req.Header.Set("Cookie", tampered)
	tres, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("tampered csrf request: %v", err)
	}
	_ = tres.Body.Close()
	if tres.StatusCode != http.StatusBadRequest {
		t.Errorf("status with tampered csrf = %d, want 400", tres.StatusCode)
	}
}

func TestHandlerSessionAnonymousNull(t *testing.T) {
	h := newHandlerHarness(t)
	res := h.get(t, "/api/auth/session")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body != nil {
		t.Errorf("body = (%v, %v), want JSON null", body, err)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "private, no-cache, no-store" {
		t.Errorf("cache-control = %q, want private, no-cache, no-store", cc)
	}
}

func TestHandlerSessionExpiredClearsCookie(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "", false)
	ctx := context.Background()
	s, err := h.auth.CreateSession(ctx, "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	*h.now = h.now.Add(DefaultSessionTTL + time.Second) // make the session expire

	cookie := &http.Cookie{Name: h.auth.sessionCookieName(), Value: s.Token}
	res := h.get(t, "/api/auth/session", cookie)
	var body any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body != nil {
		t.Errorf("expired session body = (%v, %v), want null", body, err)
	}
	cleared := cookieByName(res, h.auth.sessionCookieName())
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cleared cookie = %+v, want empty with Max-Age=0", cleared)
	}
}

func TestHandlerSessionAdminPermissions(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "", true)
	s, err := h.auth.CreateSession(context.Background(), "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	cookie := &http.Cookie{Name: h.auth.sessionCookieName(), Value: s.Token}
	res := h.get(t, "/api/auth/session", cookie)
	var body struct {
		User struct {
			Permissions []string `json:"permissions"`
		} `json:"user"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// admin expands into every *-full-all level and its children
	// (packages/definitions/src/permissions.ts:53-66).
	want := []string{
		"admin",
		"app-create", "app-full-all", "app-modify-all", "app-use-all",
		"board-create", "board-full-all", "board-modify-all", "board-view-all",
		"integration-create", "integration-full-all", "integration-interact-all", "integration-use-all",
		"media-full-all", "media-upload", "media-view-all",
		"other-view-logs",
		"search-engine-create", "search-engine-full-all", "search-engine-modify-all",
	}
	if len(body.User.Permissions) != len(want) {
		t.Errorf("permissions = %v, want %v", body.User.Permissions, want)
	}
	for i, p := range want {
		if i >= len(body.User.Permissions) || body.User.Permissions[i] != p {
			t.Errorf("permissions[%d] = %q, want %q", i, body.User.Permissions[i], p)
		}
	}
}

func TestHandlerCSRFCookieAndNoCache(t *testing.T) {
	h := newHandlerHarness(t)
	token, _ := h.fetchCSRF(t)
	if len(token) != 32 {
		t.Errorf("csrf token length = %d, want 32", len(token))
	}
	res := h.get(t, "/api/auth/csrf")
	if cc := res.Header.Get("Cache-Control"); cc != "private, no-cache, no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	if res.Header.Get("Pragma") != "no-cache" || res.Header.Get("Expires") != "0" {
		t.Error("missing Pragma/Expires no-cache headers")
	}
}

func TestHandlerProviders(t *testing.T) {
	h := newHandlerHarness(t)
	res := h.get(t, "/api/auth/providers")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]providerJSON
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	p, ok := body["credentials"]
	if !ok || p.ID != "credentials" || p.Name != "Credentials" || p.Type != "credentials" {
		t.Errorf("credentials provider = %+v", p)
	}
	if !strings.HasSuffix(p.SigninURL, "/api/auth/signin/credentials") ||
		!strings.HasSuffix(p.CallbackURL, "/api/auth/callback/credentials") {
		t.Errorf("provider urls = %q / %q", p.SigninURL, p.CallbackURL)
	}
}

func TestHandlerUnknownAction(t *testing.T) {
	h := newHandlerHarness(t)
	for _, path := range []string{"/api/auth/unknown", "/api/auth/signin/credentials", "/api/auth/callback/ldap"} {
		res := h.get(t, path)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, res.StatusCode)
		}
		var body string
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body != "Bad request." {
			t.Errorf("GET %s body = (%q, %v), want \"Bad request.\"", path, body, err)
		}
	}
	// Wrong method on a known action is also a bad request (upstream would
	// dispatch POST-only actions through UnknownAction handling).
	res := h.postForm(t, "/api/auth/csrf", url.Values{})
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("POST csrf status = %d, want 400", res.StatusCode)
	}
}

func TestHandlerRedirectWithoutHeader(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "pw", false)
	token, csrfCookie := h.fetchCSRF(t)

	// Without X-Auth-Return-Redirect the server answers a real HTTP redirect
	// (@auth/core Response.redirect -> 302).
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/auth/callback/credentials",
		strings.NewReader(url.Values{"csrfToken": {token}, "callbackUrl": {h.server.URL + "/"}, "name": {"admin"}, "password": {"pw"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("redirect login: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Errorf("status without return-redirect header = %d, want 302", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != h.server.URL+"/" {
		t.Errorf("location = %q, want %q", loc, h.server.URL+"/")
	}
}

func TestHandlerCrossOriginCallbackRejected(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "pw", false)
	token, csrfCookie := h.fetchCSRF(t)
	fields := url.Values{
		"csrfToken":   {token},
		"callbackUrl": {"https://evil.example.com/steal"},
		"name":        {"admin"},
		"password":    {"pw"},
	}
	res := h.postForm(t, "/api/auth/callback/credentials", fields, csrfCookie)
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.URL != h.server.URL {
		t.Errorf("url = %q, want the same-origin fallback %q", body.URL, h.server.URL)
	}
}

func TestHandlerSignoutRequiresCSRF(t *testing.T) {
	h := newHandlerHarness(t)
	seedUser(t, h.db, "u1", "admin", "", false)
	ctx := context.Background()
	s, _ := h.auth.CreateSession(ctx, "u1")
	cookie := &http.Cookie{Name: h.auth.sessionCookieName(), Value: s.Token}

	// signout without csrf pair still requires verification (upstream
	// validateCSRF runs unconditionally for POST signout).
	res := h.postForm(t, "/api/auth/signout", url.Values{"csrfToken": {"x"}}, cookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("signout without csrf status = %d, want 400", res.StatusCode)
	}

	token, csrfCookie := h.fetchCSRF(t)
	res = h.postForm(t, "/api/auth/signout", url.Values{"csrfToken": {token}}, csrfCookie, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("signout status = %d, want 200", res.StatusCode)
	}
	if _, err := h.auth.ValidateSession(ctx, s.Token); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("session still valid after signout: %v", err)
	}
}

func TestExpandPermissionsAdmin(t *testing.T) {
	got := expandPermissions([]string{"admin"})
	want := map[string]bool{
		"admin": true, "board-full-all": true, "board-modify-all": true, "board-view-all": true,
		"board-create": true, "app-full-all": true, "app-modify-all": true, "app-create": true,
		"app-use-all": true, "integration-full-all": true, "integration-interact-all": true,
		"integration-use-all": true, "integration-create": true, "search-engine-full-all": true,
		"search-engine-modify-all": true, "search-engine-create": true, "media-full-all": true,
		"media-upload": true, "media-view-all": true, "other-view-logs": true,
	}
	if len(got) != len(want) {
		t.Errorf("expandPermissions(admin) = %v (%d), want %d items", got, len(got), len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected permission %q", p)
		}
	}
	// Sorted for stable output.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("not sorted at %d: %q >= %q", i, got[i-1], got[i])
		}
	}
	// An empty input stays empty; a leaf permission stays a single item.
	if got := expandPermissions(nil); len(got) != 0 {
		t.Errorf("expandPermissions(nil) = %v, want empty", got)
	}
	if got := expandPermissions([]string{"board-view-all"}); len(got) != 1 || got[0] != "board-view-all" {
		t.Errorf("expandPermissions(leaf) = %v, want [board-view-all]", got)
	}
}
