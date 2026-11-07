package info_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/info"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness: a real trpc.Router over httptest with header-driven session
// injection standing in for the auth middleware (see board/harness_test.go).
func newTestAPI(t *testing.T, opts info.Options) (*httptest.Server, *ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	info.Register(r, d, opts)

	session := &ctxauth.Session{ID: "session-1", UserID: "user-1", Name: "alice", IsAdministrator: true}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test-User") != "" {
			req = req.WithContext(ctxauth.WithSession(req.Context(), session))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, session
}

func call(t *testing.T, srv *httptest.Server, user, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
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

func TestInfoRouter(t *testing.T) {
	srv, _ := newTestAPI(t, info.Options{})

	// info.getInfo: protectedProcedure — anonymous callers get UNAUTHORIZED.
	status, body := call(t, srv, "", "/api/trpc/info.getInfo")
	if status != http.StatusUnauthorized {
		t.Fatalf("getInfo anonymous status = %d, want 401 (%s)", status, body)
	}
	if !strings.Contains(body, `"code":"UNAUTHORIZED"`) {
		t.Fatalf("getInfo anonymous body = %s, want UNAUTHORIZED envelope", body)
	}

	// Signed-in callers get the version.
	status, body = call(t, srv, "alice", "/api/trpc/info.getInfo")
	if status != http.StatusOK {
		t.Fatalf("getInfo status = %d (%s)", status, body)
	}
	want := `{"result":{"data":{"json":{"version":"2.11.1"}}}}`
	if body != want {
		t.Fatalf("getInfo body = %s, want %s", body, want)
	}

	// Demo flags are plain booleans (public procedures).
	status, body = call(t, srv, "", "/api/trpc/info.isDemoMode")
	if status != http.StatusOK || body != `{"result":{"data":{"json":false}}}` {
		t.Fatalf("isDemoMode = %d %s", status, body)
	}
	status, body = call(t, srv, "", "/api/trpc/info.isDemoReadOnly")
	if status != http.StatusOK || body != `{"result":{"data":{"json":false}}}` {
		t.Fatalf("isDemoReadOnly = %d %s", status, body)
	}
}

func TestInfoOptionsOverride(t *testing.T) {
	srv, _ := newTestAPI(t, info.Options{Version: "9.9.9", DemoMode: true, DemoReadOnly: true})
	_, body := call(t, srv, "alice", "/api/trpc/info.getInfo")
	if !strings.Contains(body, `"version":"9.9.9"`) {
		t.Fatalf("getInfo body = %s, want overridden version", body)
	}
	// isDemoReadOnly = DemoMode && DemoReadOnly.
	_, body = call(t, srv, "", "/api/trpc/info.isDemoReadOnly")
	if body != `{"result":{"data":{"json":true}}}` {
		t.Fatalf("isDemoReadOnly = %s", body)
	}
}
