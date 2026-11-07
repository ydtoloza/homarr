package user_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
)

// TestReadOnlyAPIKeyEnforcement pins the read-only API key rejection of
// credentialProcedure (packages/api/src/trpc.ts:130-135): a caller
// authenticated with a read-only API key (ctxauth.Session.ReadOnly) may run
// read procedures but every mutation answers FORBIDDEN "This credential only
// permits read-only operations", while sessions without the flag are
// unaffected.
func TestReadOnlyAPIKeyEnforcement(t *testing.T) {
	d, srv, sessions := newTestAPI(t)

	readOnly := seedUser(t, d, "ro-1", "readonly", false)
	readOnly.ReadOnly = true
	sessions["ro"] = readOnly
	writable := seedUser(t, d, "wr-1", "writable", false)
	sessions["wr"] = writable

	// A read procedure succeeds with a read-only key.
	status, body := call(t, srv, "ro", "/api/trpc/user.getTourStatus", "", "")
	if status != http.StatusOK {
		t.Fatalf("read with read-only key status = %d (body %s), want 200", status, body)
	}

	// A write procedure fails with the exact upstream error.
	status, body = call(t, srv, "ro", "/api/trpc/user.changeColorScheme",
		`{"colorScheme":"dark","id":"ro-1"}`, "")
	if status != http.StatusForbidden {
		t.Fatalf("write with read-only key status = %d (body %s), want 403", status, body)
	}
	if !strings.Contains(body, `"message":"This credential only permits read-only operations"`) ||
		!strings.Contains(body, `"code":"FORBIDDEN"`) {
		t.Fatalf("write with read-only key body = %s, want the exact upstream FORBIDDEN envelope", body)
	}

	// A session without the readOnly flag is unaffected.
	status, body = call(t, srv, "wr", "/api/trpc/user.changeColorScheme",
		`{"colorScheme":"dark","id":"wr-1"}`, "")
	if status != http.StatusOK {
		t.Fatalf("write with a normal session status = %d (body %s), want 200", status, body)
	}

	// Ordering: credentialProcedure is the base middleware upstream, so the
	// rejection precedes input validation (a malformed write body still gets
	// the read-only error, not BAD_REQUEST).
	status, body = call(t, srv, "ro", "/api/trpc/user.changeColorScheme",
		`{"colorScheme":"bogus"}`, "")
	if status != http.StatusForbidden || !strings.Contains(body, admin.ReadOnlyForbiddenMessage) {
		t.Fatalf("malformed write with read-only key = %d %s, want 403 %s", status, body, admin.ReadOnlyForbiddenMessage)
	}

	// The other protectedProcedure write family (tours) is covered too.
	status, body = call(t, srv, "ro", "/api/trpc/user.completeTour", `{"tour":"board"}`, "")
	if status != http.StatusForbidden || !strings.Contains(body, admin.ReadOnlyForbiddenMessage) {
		t.Fatalf("completeTour with read-only key = %d %s, want the read-only FORBIDDEN", status, body)
	}
}
