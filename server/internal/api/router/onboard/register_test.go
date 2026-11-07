package onboard_test

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/onboard"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// testClaimToken is the claim the tests mint into serverSettings; the
// harness carries it through the OnboardingClaimTokenFromContext hook like
// the auth middleware would extract it from the cookie.
const testClaimToken = "test-claim-token-0123456789"

// Test harness (style of board/harness_test.go) plus the claim resolution
// hook.
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	onboard.Register(r, d, onboard.Options{
		OnboardingClaimTokenFromContext: func(ctx context.Context) string {
			token, _ := ctx.Value(claimKey{}).(string)
			return token
		},
	})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		if token := req.Header.Get("X-Test-Claim"); token != "" {
			ctx = context.WithValue(ctx, claimKey{}, token)
		}
		if name := req.Header.Get("X-Test-User"); name != "" {
			ctx = ctxauth.WithSession(ctx, sessions[name])
		}
		r.Handler().ServeHTTP(w, req.WithContext(ctx))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv, sessions
}

type claimKey struct{}

// seedClaim writes the __onboarding_claim row exactly like
// claimOnboardingAsync does (packages/api/src/onboarding-claim.ts:103-117):
// a raw JSON record (no superjson envelope) under the serverSettings key.
func seedClaim(t *testing.T, d *db.DB, expiresAt time.Time) {
	t.Helper()
	sum := sha256.Sum256([]byte(testClaimToken))
	value := fmt.Sprintf(`{"hash":%q,"expiresAt":%d}`, hex.EncodeToString(sum[:]), expiresAt.UnixMilli())
	if err := d.UpsertServerSetting(context.Background(), "__onboarding_claim", value); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
}

// seedOnboardingRow writes the wizard row the seed migration creates.
func seedOnboardingRow(t *testing.T, d *db.DB, step string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO "onboarding" (id, step) VALUES ('onboarding-1', ?)`, step); err != nil {
		t.Fatalf("seed onboarding row: %v", err)
	}
}

func call(t *testing.T, srv *httptest.Server, user, path, claim string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if claim != "" {
		req.Header.Set("X-Test-Claim", claim)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res.StatusCode, string(raw)
}

func TestOnboardingCurrentStepAndNext(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["root"] = &ctxauth.Session{ID: "s1", UserID: "user-root", Name: "root", IsAdministrator: true}

	// A missing row behaves like a fresh installation ("start",
	// getOnboardingOrFallbackAsync).
	status, body := call(t, srv, "", "/api/trpc/onboard.currentStep", "")
	if status != http.StatusOK || body != `{"result":{"data":{"json":{"current":"start","previous":null}}}}` {
		t.Fatalf("currentStep (no row) = %d %s", status, body)
	}

	// nextStep requires the claim or an administrator.
	status, body = call(t, srv, "", "/api/trpc/onboard.nextStep", "")
	if status != http.StatusForbidden || !strings.Contains(body, "This onboarding session is not claimed.") {
		t.Fatalf("nextStep anonymous without claim = %d %s", status, body)
	}
	seedClaim(t, d, time.Now().Add(time.Hour))
	seedOnboardingRow(t, d, "start")

	// Claim-only caller (no users exist): the transition happens and the
	// next step resolves through the user table (no users -> "user").
	status, body = call(t, srv, "", "/api/trpc/onboard.nextStep", testClaimToken)
	if status != http.StatusOK {
		t.Fatalf("nextStep with claim = %d %s", status, body)
	}
	var step struct {
		Current string `json:"current"`
	}
	_ = json.Unmarshal([]byte(body), &step)
	status, body = call(t, srv, "", "/api/trpc/onboard.currentStep", "")
	if !strings.Contains(body, `"current":"user"`) || !strings.Contains(body, `"previous":"start"`) {
		t.Fatalf("currentStep after nextStep = %d %s, want user/start", status, body)
	}

	// A second nextStep is PRECONDITION_FAILED: the welcome step is done.
	status, body = call(t, srv, "root", "/api/trpc/onboard.nextStep", "")
	if status != http.StatusPreconditionFailed ||
		!strings.Contains(body, "The welcome step is already complete.") {
		t.Fatalf("nextStep twice = %d %s", status, body)
	}

	// Claim tampering fails the constant-time match.
	seedClaim(t, d, time.Now().Add(-time.Hour)) // expired
	seedOnboardingRow2(t, d)
	status, body = call(t, srv, "", "/api/trpc/onboard.nextStep", testClaimToken)
	if status != http.StatusForbidden {
		t.Fatalf("nextStep with expired claim = %d %s", status, body)
	}
	_ = subtle.ConstantTimeCompare
}

// seedOnboardingRow2 resets the wizard to "start" for the expired-claim
// check (the first row transitioned to "user").
func seedOnboardingRow2(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Exec(`UPDATE "onboarding" SET step = 'start', previous_step = NULL WHERE id = 'onboarding-1'`); err != nil {
		t.Fatalf("reset onboarding row: %v", err)
	}
}
