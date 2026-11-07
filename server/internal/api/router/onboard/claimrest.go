package onboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// ClaimHandler is the Go port of the onboarding claim minting endpoint
// POST /api/onboarding/claim
// (apps/nextjs/src/app/api/onboarding/claim/route.ts). It is a plain REST
// route, NOT tRPC: the SPA calls it during the first-run wizard and carries
// the returned token as the "homarr-onboarding-claim" cookie, which the
// onboarding-gated tRPC procedures verify through
// Options.OnboardingClaimTokenFromContext.
//
// Mount it behind the auth middleware so the caller session is resolved:
//
//	router.Handle("/api/onboarding/claim",
//	    auth.Middleware()(onboard.ClaimHandler(database)))
//
// Status matrix (route.ts:13-53, statuses from admin.ClaimOnboarding):
//
//	sec-fetch-site not same-origin/none -> 403 {"code":"cross_site"}
//	finished                            -> 409 {"code":"finished"} + cookie delete
//	forbidden                           -> 403 {"code":"administrator_required"}
//	locked                              -> 423 {"code":"locked","expiresAt"} + no-store
//	issued / active                     -> 200 {"status","expiresAt"} + no-store + Set-Cookie
//	(unreachable union member)          -> 409 {"code":"unavailable"}
//
// The issued/active Set-Cookie is HttpOnly, SameSite=Strict, Path=/, with
// Expires = the claim expiry; Secure follows x-forwarded-proto (first value)
// or the request being TLS, like route.ts:39-40.
//
// Deviation note: the administrator override reads ctxauth.Session.
// IsAdministrator, the fork-wide mapping of upstream
// session.user.permissions.includes("admin") (the auth middleware derives it
// from the "admin" group permission, AUTH.md §5). The session-cookie delete
// on "finished" carries Path=/ so it actually clears the Path=/ claim cookie
// (Next's cookies.delete emits no Path and would not match it).
func ClaimHandler(database *db.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// route.ts exports POST only; Next answers 405 with an empty body to
		// any other method on a route handler.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Cross-site browser requests are rejected before anything else
		// (route.ts:14-17); non-browser callers without the header pass.
		if fetchSite := r.Header.Get("Sec-Fetch-Site"); fetchSite != "" &&
			fetchSite != "same-origin" && fetchSite != "none" {
			writeClaimJSON(w, http.StatusForbidden, claimCodeBody{Code: "cross_site"})
			return
		}

		// force = session?.user.permissions.includes("admin") (route.ts:22).
		force := false
		if session := ctxauth.FromContext(r.Context()); session != nil {
			force = session.IsAdministrator
		}

		result, err := admin.ClaimOnboarding(r.Context(), database, admin.ClaimOnboardingOptions{
			CurrentToken: admin.OnboardingClaimTokenFromCookieHeader(r.Header.Get("Cookie")),
			Force:        force,
		})
		if err != nil {
			// Upstream lets the throw bubble into a 500 (Next route handler
			// error page); the fork answers a plain JSON 500.
			writeClaimJSON(w, http.StatusInternalServerError, claimCodeBody{Code: "internal_error"})
			return
		}

		switch result.Status {
		case admin.ClaimStatusFinished:
			// route.ts:24-28: 409 plus the claim cookie removal.
			http.SetCookie(w, &http.Cookie{
				Name:     admin.OnboardingClaimCookieName,
				Value:    "",
				Path:     "/",
				Expires:  time.Unix(0, 0),
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			writeClaimJSON(w, http.StatusConflict, claimCodeBody{Code: "finished"})
			return
		case admin.ClaimStatusForbidden:
			writeClaimJSON(w, http.StatusForbidden, claimCodeBody{Code: "administrator_required"})
			return
		case admin.ClaimStatusLocked:
			setNoStore(w)
			writeClaimJSON(w, http.StatusLocked, claimCodeExpiryBody{Code: "locked", ExpiresAt: result.ExpiresAt})
			return
		case admin.ClaimStatusIssued, admin.ClaimStatusActive:
			// x-forwarded-proto first value wins (route.ts:39-40).
			secure := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Proto"), ",", 2)[0]) == "https" ||
				r.URL.Scheme == "https" || r.TLS != nil
			setNoStore(w)
			http.SetCookie(w, &http.Cookie{
				Name:     admin.OnboardingClaimCookieName,
				Value:    result.Token,
				Path:     "/",
				Expires:  time.UnixMilli(result.ExpiresAt),
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				Secure:   secure,
			})
			writeClaimJSON(w, http.StatusOK, claimStatusExpiryBody{Status: result.Status, ExpiresAt: result.ExpiresAt})
			return
		default:
			// Unreachable with the closed status union; route.ts:35-37 keeps a
			// catch-all 409, so the port does too.
			writeClaimJSON(w, http.StatusConflict, claimCodeBody{Code: "unavailable"})
			return
		}
	})
}

// claimCodeBody is the {"code": ...} body family of route.ts (cross_site,
// finished, administrator_required, unavailable).
type claimCodeBody struct {
	Code string `json:"code"`
}

// claimCodeExpiryBody is {"code": "locked", "expiresAt": epochMs}
// (route.ts:33).
type claimCodeExpiryBody struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

// claimStatusExpiryBody is the issued/active success body
// {"status": "issued"|"active", "expiresAt": epochMs} (route.ts:41-44): the
// member is "status" here but "code" in the error bodies, so the shapes stay
// byte-exact.
type claimStatusExpiryBody struct {
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expiresAt"`
}

func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func writeClaimJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(body)
}
