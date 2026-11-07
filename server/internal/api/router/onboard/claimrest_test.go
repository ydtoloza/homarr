package onboard_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/router/onboard"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// claimTestAPI mounts ClaimHandler behind a middleware that resolves a test
// session from the X-Test-User header, mirroring the auth.Middleware seam
// (the real mount is httpserver's: router.Handle("/api/onboarding/claim",
// auth.Middleware()(onboard.ClaimHandler(database)))).
type claimTestAPI struct {
	db        *db.DB
	srv       *httptest.Server
	sessions  map[string]*ctxauth.Session
	client    *http.Client // does not follow redirects and keeps no cookie jar
	jarCookie string       // claim cookie value replayed by claimPost
}

func newClaimTestAPI(t *testing.T) *claimTestAPI {
	t.Helper()
	d := db.OpenTest(t)
	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			req = req.WithContext(ctxauth.WithSession(req.Context(), sessions[name]))
		}
		onboard.ClaimHandler(d).ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &claimTestAPI{db: d, srv: srv, sessions: sessions}
}

func (a *claimTestAPI) seedOnboarding(t *testing.T, step string) {
	t.Helper()
	if _, err := a.db.Exec(`INSERT INTO "onboarding" (id, step) VALUES ('onboarding-1', ?)`, step); err != nil {
		t.Fatalf("seed onboarding: %v", err)
	}
}

func (a *claimTestAPI) seedUser(t *testing.T, id, name string, isAdmin bool) {
	t.Helper()
	if err := a.db.CreateUser(context.Background(), db.User{ID: id, Name: name, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	a.sessions[id] = &ctxauth.Session{ID: "session-" + id, UserID: id, Name: name, IsAdministrator: isAdmin}
}

// claimPost POSTs the endpoint; when cookieValue is non-empty it travels as
// the claim cookie in the Cookie header (the way the SPA replays it).
func (a *claimTestAPI) claimPost(cookieValue string, headers map[string]string) (*http.Response, string) {
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/onboarding/claim", nil)
	if err != nil {
		panic(err)
	}
	if cookieValue != "" {
		req.Header.Set("Cookie", admin.OnboardingClaimCookieName+"="+cookieValue)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := a.srv.Client().Do(req)
	if err != nil {
		panic(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		panic(err)
	}
	return res, string(raw)
}

// decodeBody reads {"code": "..."} / {"status": "...", "expiresAt": n} style
// bodies into a generic map.
func decodeBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return m
}

// TestClaimHandlerIssuedActiveLocked covers the happy path of the status
// matrix through the REST surface: issuance stores only a hash and hands the
// token back as a strict cookie, the claiming browser re-claims (active),
// another browser is locked until the claim expires, and expiry frees
// minting while invalidating the old token (onboarding-claim.spec.ts:16-60).
func TestClaimHandlerIssuedActiveLocked(t *testing.T) {
	a := newClaimTestAPI(t)
	a.seedOnboarding(t, "start")

	res, body := a.claimPost("", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first claim status = %d, want 200 (body %s)", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("issued Cache-Control = %q, want no-store", cc)
	}
	parsed := decodeBody(t, body)
	if parsed["status"] != "issued" {
		t.Fatalf("issued body = %s, want status issued", body)
	}
	expiresAt, ok := parsed["expiresAt"].(float64)
	if !ok {
		t.Fatalf("issued body = %s, want numeric expiresAt", body)
	}
	if want := time.Now().Add(admin.OnboardingClaimTTL).UnixMilli(); abs64(int64(expiresAt)-want) > 5000 {
		t.Fatalf("issued expiresAt = %d, want ~%d (TTL 4h)", int64(expiresAt), want)
	}

	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != admin.OnboardingClaimCookieName {
		t.Fatalf("issued cookies = %v, want the claim cookie", cookies)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("claim cookie attributes = %+v, want HttpOnly SameSite=Strict Path=/", cookie)
	}
	if cookie.Secure {
		t.Fatalf("claim cookie over http must not be Secure: %+v", cookie)
	}
	if abs64(cookie.Expires.UnixMilli()-int64(expiresAt)) > 2000 {
		t.Fatalf("claim cookie Expires = %v, want the claim expiry %d", cookie.Expires, int64(expiresAt))
	}

	// Only the hash is stored: the token never touches the database.
	var stored string
	if err := a.db.QueryRow(`SELECT value FROM "serverSetting" WHERE setting_key = ?`,
		admin.OnboardingClaimSettingKey).Scan(&stored); err != nil {
		t.Fatalf("read claim row: %v", err)
	}
	if strings.Contains(stored, cookie.Value) {
		t.Fatalf("stored claim %q must not contain the raw token", stored)
	}
	valid, err := admin.IsOnboardingClaimValid(context.Background(), a.db, cookie.Value, time.Now())
	if err != nil || !valid {
		t.Fatalf("stored claim does not validate the issued token (valid=%v err=%v)", valid, err)
	}

	// Same browser: active re-claim, same token and expiry.
	res2, body2 := a.claimPost(cookie.Value, nil)
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("re-claim status = %d (body %s)", res2.StatusCode, body2)
	}
	parsed2 := decodeBody(t, body2)
	if parsed2["status"] != "active" {
		t.Fatalf("re-claim body = %s, want status active", body2)
	}
	if int64(parsed2["expiresAt"].(float64)) != int64(expiresAt) {
		t.Fatalf("re-claim expiresAt = %s, want the original %d", body2, int64(expiresAt))
	}
	if got := res2.Cookies(); len(got) != 1 || got[0].Value != cookie.Value {
		t.Fatalf("re-claim must replay the same token, got %v", got)
	}

	// Another browser without the token: locked, with the lock expiry.
	res3, body3 := a.claimPost("", nil)
	if res3.StatusCode != http.StatusLocked {
		t.Fatalf("locked status = %d (body %s)", res3.StatusCode, body3)
	}
	if cc := res3.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("locked Cache-Control = %q, want no-store", cc)
	}
	parsed3 := decodeBody(t, body3)
	if parsed3["code"] != "locked" || int64(parsed3["expiresAt"].(float64)) != int64(expiresAt) {
		t.Fatalf("locked body = %s, want {code: locked, expiresAt: %d}", body3, int64(expiresAt))
	}
	if got := res3.Cookies(); len(got) != 0 {
		t.Fatalf("locked response must not set cookies, got %v", got)
	}

	// Admin force rotates the live claim even while it is locked.
	a.seedUser(t, "admin-1", "admin", true)
	res4, body4 := a.claimPost("", map[string]string{"X-Test-User": "admin-1"})
	if res4.StatusCode != http.StatusOK {
		t.Fatalf("forced claim status = %d (body %s)", res4.StatusCode, body4)
	}
	parsed4 := decodeBody(t, body4)
	if parsed4["status"] != "issued" {
		t.Fatalf("forced claim body = %s, want status issued", body4)
	}
	if int64(parsed4["expiresAt"].(float64)) == int64(expiresAt) {
		t.Fatalf("forced claim must mint a fresh expiry, got %s", body4)
	}
	// The previous browser's token is dead, the new one validates.
	if valid, _ := admin.IsOnboardingClaimValid(context.Background(), a.db, cookie.Value, time.Now()); valid {
		t.Fatalf("rotated claim must invalidate the previous token")
	}
	if valid, _ := admin.IsOnboardingClaimValid(context.Background(), a.db, res4.Cookies()[0].Value, time.Now()); !valid {
		t.Fatalf("rotated claim token does not validate")
	}
}

// TestClaimHandlerFinishedForbidden covers the refusing branches of the
// matrix: a finished wizard answers 409 + cookie delete, and a non-admin
// caller after the first user exists is forbidden unless the override runs.
func TestClaimHandlerFinishedForbidden(t *testing.T) {
	a := newClaimTestAPI(t)
	a.seedOnboarding(t, "setup")

	// First claim works while no user exists, then a user appears.
	res, body := a.claimPost("", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d (body %s)", res.StatusCode, body)
	}
	token := res.Cookies()[0].Value
	a.seedUser(t, "u1", "owner", false)

	res2, body2 := a.claimPost("", nil)
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("post-user claim status = %d (body %s)", res2.StatusCode, body2)
	}
	if got := decodeBody(t, body2)["code"]; got != "administrator_required" {
		t.Fatalf("post-user body = %s, want code administrator_required", body2)
	}

	// The claiming browser is forbidden too once a user exists: upstream
	// checks existingUser before the active-token match
	// (onboarding-claim.ts:84-85 precedes :90-92).
	res3, body3 := a.claimPost(token, nil)
	if res3.StatusCode != http.StatusForbidden {
		t.Fatalf("claim after user status = %d (body %s)", res3.StatusCode, body3)
	}
	if got := decodeBody(t, body3)["code"]; got != "administrator_required" {
		t.Fatalf("claim after user body = %s, want code administrator_required", body3)
	}

	// Wizard finished: 409 finished plus the cookie removal.
	if _, err := a.db.Exec(`UPDATE "onboarding" SET step = 'finish'`); err != nil {
		t.Fatalf("finish onboarding: %v", err)
	}
	res4, body4 := a.claimPost(token, nil)
	if res4.StatusCode != http.StatusConflict {
		t.Fatalf("finished status = %d (body %s)", res4.StatusCode, body4)
	}
	if got := decodeBody(t, body4)["code"]; got != "finished" {
		t.Fatalf("finished body = %s, want code finished", body4)
	}
	deleted := res4.Cookies()
	if len(deleted) != 1 || deleted[0].Value != "" || !deleted[0].Expires.Before(time.Now()) {
		t.Fatalf("finished cookies = %v, want the claim cookie cleared with a past expiry", deleted)
	}
}

// TestClaimHandlerCrossSiteAndMethod pins the sec-fetch-site rejection
// (route.ts:14-17), the header values that pass, the https detection via
// x-forwarded-proto (route.ts:39-40) and the POST-only surface.
func TestClaimHandlerCrossSiteAndMethod(t *testing.T) {
	a := newClaimTestAPI(t)
	a.seedOnboarding(t, "start")

	for header := range map[string]bool{
		"cross-site": false,
		"same-site":  false,
	} {
		res, body := a.claimPost("", map[string]string{"Sec-Fetch-Site": header})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site %s status = %d, want 403", header, res.StatusCode)
		}
		if got := decodeBody(t, body)["code"]; got != "cross_site" {
			t.Fatalf("cross-site body = %s, want code cross_site", body)
		}
	}
	for header := range map[string]bool{
		"same-origin": true,
		"none":        true,
	} {
		// A fresh installation per header: an allowed claim mints a lock that
		// would fail the next case.
		allowed := newClaimTestAPI(t)
		allowed.seedOnboarding(t, "start")
		res, body := allowed.claimPost("", map[string]string{"Sec-Fetch-Site": header})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("Sec-Fetch-Site %s status = %d, want 200 (body %s)", header, res.StatusCode, body)
		}
	}

	// https via x-forwarded-proto flips the cookie Secure attribute; the
	// first value of a comma list wins. Fresh installations per case: an
	// issued claim would lock the next one.
	httpsAPI := newClaimTestAPI(t)
	httpsAPI.seedOnboarding(t, "start")
	res, _ := httpsAPI.claimPost("", map[string]string{"X-Forwarded-Proto": "https, http"})
	if res.StatusCode != http.StatusOK || len(res.Cookies()) != 1 {
		t.Fatalf("https claim status = %d cookies %v, want 200 + cookie", res.StatusCode, res.Cookies())
	}
	if c := res.Cookies()[0]; !c.Secure {
		t.Fatalf("claim cookie behind https proxy must be Secure: %+v", c)
	}
	httpAPI := newClaimTestAPI(t)
	httpAPI.seedOnboarding(t, "start")
	res, _ = httpAPI.claimPost("", map[string]string{"X-Forwarded-Proto": "http"})
	if res.StatusCode != http.StatusOK || len(res.Cookies()) != 1 {
		t.Fatalf("http claim status = %d cookies %v, want 200 + cookie", res.StatusCode, res.Cookies())
	}
	if c := res.Cookies()[0]; c.Secure {
		t.Fatalf("claim cookie behind http proxy must not be Secure: %+v", c)
	}

	// Non-POST methods are rejected like a Next route handler without the
	// export (405).
	res2, err := http.Get(a.srv.URL + "/api/onboarding/claim")
	if err != nil {
		t.Fatalf("GET claim: %v", err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET claim status = %d, want 405", res2.StatusCode)
	}
}

// TestClaimHandlerUnauthenticatedAdminOverrideIsNotForced pins that the
// administrator override comes only from the session in the request context:
// an anonymous caller after the first user exists is forbidden.
func TestClaimHandlerUnauthenticatedAdminOverrideIsNotForced(t *testing.T) {
	a := newClaimTestAPI(t)
	a.seedOnboarding(t, "setup")
	a.seedUser(t, "u1", "owner", false)

	res, body := a.claimPost("", nil)
	if res.StatusCode != http.StatusForbidden || decodeBody(t, body)["code"] != "administrator_required" {
		t.Fatalf("anonymous claim = %d %s, want 403 administrator_required", res.StatusCode, body)
	}
}

// TestOnboardingClaimTokenFromCookieHeader mirrors
// onboarding-claim.spec.ts:76-83: only the exact cookie matches, values are
// percent-decoded and malformed escapes behave like a missing token.
func TestOnboardingClaimTokenFromCookieHeader(t *testing.T) {
	const cookie = admin.OnboardingClaimCookieName
	cases := []struct {
		header string
		want   string
	}{
		{"theme=dark; " + cookie + "=secret-token; other=x", "secret-token"},
		{cookie + "=secret%2Dtoken", "secret-token"},
		{cookie + "=%E0%A4%A", ""},
		{cookie + "-copy=wrong", ""},
		{cookie + "=", ""},
		{"", ""},
		{"theme=dark", ""},
	}
	for _, tc := range cases {
		if got := admin.OnboardingClaimTokenFromCookieHeader(tc.header); got != tc.want {
			t.Errorf("OnboardingClaimTokenFromCookieHeader(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// TestClaimOnboardingExpiredRotation mirrors onboarding-claim.spec.ts:49-60
// at the issuance layer: after expiry a new claim is issued and the old token
// is invalidated.
func TestClaimOnboardingExpiredRotation(t *testing.T) {
	a := newClaimTestAPI(t)
	a.seedOnboarding(t, "start")

	first, err := admin.ClaimOnboarding(context.Background(), a.db, admin.ClaimOnboardingOptions{
		Now: time.UnixMilli(1000), TTL: 100 * time.Millisecond,
	})
	if err != nil || first.Status != admin.ClaimStatusIssued {
		t.Fatalf("first claim = %+v err %v, want issued", first, err)
	}
	second, err := admin.ClaimOnboarding(context.Background(), a.db, admin.ClaimOnboardingOptions{
		Now: time.UnixMilli(1101), TTL: 100 * time.Millisecond,
	})
	if err != nil || second.Status != admin.ClaimStatusIssued {
		t.Fatalf("second claim = %+v err %v, want issued", second, err)
	}
	if second.Token == first.Token {
		t.Fatalf("rotation must mint a fresh token")
	}
	if valid, _ := admin.IsOnboardingClaimValid(context.Background(), a.db, first.Token, time.UnixMilli(1102)); valid {
		t.Fatalf("expired token must not validate")
	}
	if valid, _ := admin.IsOnboardingClaimValid(context.Background(), a.db, second.Token, time.UnixMilli(1102)); !valid {
		t.Fatalf("replacement token must validate")
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
