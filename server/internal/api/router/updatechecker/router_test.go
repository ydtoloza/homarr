package updatechecker_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/updatechecker"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness: a real trpc.Router over httptest with header-driven session
// injection and a fake GitHub releases endpoint.

type testAPI struct {
	db     *db.DB
	srv    *httptest.Server
	github *fakeGitHub
}

type fakeGitHub struct {
	server   *httptest.Server
	requests atomic.Int64
	payload  atomic.Value // string
	status   atomic.Int64
}

func (f *fakeGitHub) setReleases(status int, body string) {
	f.status.Store(int64(status))
	f.payload.Store(body)
}

func (f *fakeGitHub) releasesJSON(releases ...map[string]any) string {
	raw, err := json.Marshal(releases)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	d := db.OpenTest(t)

	gh := &fakeGitHub{}
	gh.status.Store(200)
	gh.payload.Store(`[]`)
	gh.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.requests.Add(1)
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("GitHub request without User-Agent")
		}
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/vnd.github+json") {
			t.Errorf("Accept = %q, want the octokit header", accept)
		}
		w.WriteHeader(int(gh.status.Load()))
		_, _ = w.Write([]byte(gh.payload.Load().(string)))
	}))
	t.Cleanup(gh.server.Close)

	r := trpc.New()
	updatechecker.Register(r, d, updatechecker.Options{
		Version:     "2.11.1",
		ReleasesURL: gh.server.URL,
		CacheTTL:    30 * time.Millisecond,
	})

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-Admin"); name == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u1", IsAdministrator: true}))
		} else if name := req.Header.Get("X-Test-User"); name == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u1"}))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{db: d, srv: srv, github: gh}
}

func (a *testAPI) get(t *testing.T, admin bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.srv.URL+"/api/trpc/updateChecker.getAvailableUpdates", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if admin {
		req.Header.Set("X-Test-Admin", "1")
	}
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func release(name, tag string, prerelease bool, bodyHTML string) map[string]any {
	m := map[string]any{
		"name":       name,
		"html_url":   "https://github.com/homarr-labs/homarr/releases/tag/" + tag,
		"tag_name":   tag,
		"prerelease": prerelease,
	}
	if bodyHTML != "" {
		m["body_html"] = bodyHTML
	}
	return m
}

// Gates of permissionRequiredProcedure.requiresPermission("admin").
func TestUpdateCheckerGates(t *testing.T) {
	a := newTestAPI(t)

	// A single erroring op answers the classic envelope with its own status.
	status, body := a.get(t, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous status %d, want 401", status)
	}
	if !strings.Contains(body, `"code":"UNAUTHORIZED"`) {
		t.Errorf("anonymous body = %s", body)
	}

	status, body = a.get(t, true)
	if status != http.StatusOK || strings.Contains(body, "error") {
		t.Errorf("admin body = %s (status %d)", body, status)
	}
}

// The release pipeline: non-semantic tags skipped, prereleases hidden for a
// stable current version, strictly-newer releases only, newest first.
func TestUpdateCheckerPipeline(t *testing.T) {
	a := newTestAPI(t)
	a.github.setReleases(200, a.github.releasesJSON(
		release("v3", "3.0.0", false, "<p>big</p>"),
		release("v2.12", "v2.12.0", false, ""),
		release("beta", "2.12.0-beta.1", true, ""),
		release("same", "2.11.1", false, ""),
		release("older", "2.10.0", false, ""),
		release("weird", "not-a-version", false, ""),
		release("edge", "2.11.1+build.1", false, ""), // build metadata == equal precedence
	))

	status, body := a.get(t, true)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	want := `{"result":{"data":{"json":[` +
		`{"name":"v3","contentHtml":"<p>big</p>","url":"https://github.com/homarr-labs/homarr/releases/tag/3.0.0","tagName":"3.0.0","isPrerelease":false},` +
		`{"name":"v2.12","url":"https://github.com/homarr-labs/homarr/releases/tag/v2.12.0","tagName":"v2.12.0","isPrerelease":false}` +
		`]}}}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
}

// Cache: one fetch serves the fresh window; after expiry it refetches.
func TestUpdateCheckerCacheTTL(t *testing.T) {
	a := newTestAPI(t)
	a.github.setReleases(200, a.github.releasesJSON(release("v3", "3.0.0", false, "")))

	a.get(t, true)
	a.get(t, true)
	if n := a.github.requests.Load(); n != 1 {
		t.Fatalf("requests = %d, want 1 within the TTL", n)
	}
	time.Sleep(40 * time.Millisecond)
	a.get(t, true)
	if n := a.github.requests.Load(); n != 2 {
		t.Fatalf("requests = %d, want 2 after expiry", n)
	}
}

// Stale-if-error: a failing check serves the last payload and suppresses
// retries for the TTL window; the empty first failure hides the indicator.
func TestUpdateCheckerStaleOnError(t *testing.T) {
	a := newTestAPI(t)
	a.github.setReleases(200, a.github.releasesJSON(release("v3", "3.0.0", false, "")))
	a.get(t, true)

	// GitHub goes down; the fresh window still serves the payload.
	a.github.setReleases(500, `{"message":"boom"}`)
	status, body := a.get(t, true)
	if !strings.Contains(body, "3.0.0") {
		t.Errorf("fresh window broken: %s", body)
	}

	time.Sleep(40 * time.Millisecond)
	// Expired: the fetch fails → stale payload cached as fresh for another
	// window ("suppressing retries for 24 hours").
	status, body = a.get(t, true)
	if !strings.Contains(body, "3.0.0") {
		t.Errorf("stale fallback broken: %s (status %d)", body, status)
	}
	n := a.github.requests.Load()
	a.get(t, true)
	if n2 := a.github.requests.Load(); n2 != n {
		t.Errorf("retries not suppressed: %d then %d requests", n, n2)
	}

}

// A cold failure (no stale yet) yields an empty list, hiding the indicator.
func TestUpdateCheckerColdFailure(t *testing.T) {
	d := db.OpenTest(t)
	r := trpc.New()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("nope"))
	}))
	defer gh.Close()
	updatechecker.Register(r, d, updatechecker.Options{ReleasesURL: gh.URL})
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test-Admin") == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u", IsAdministrator: true}))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/trpc/updateChecker.getAvailableUpdates", nil)
	req.Header.Set("X-Test-Admin", "1")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("cold failure GET: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if strings.TrimSpace(string(raw)) != `{"result":{"data":{"json":[]}}}` {
		t.Errorf("cold failure body = %s, want an empty list", raw)
	}
}

// Single-flight: concurrent misses behind one fetch.
func TestUpdateCheckerSingleFlight(t *testing.T) {
	a := newTestAPI(t)
	// A slow GitHub response widens the race window.
	a.github.setReleases(200, a.github.releasesJSON(release("v3", "3.0.0", false, "")))

	done := make(chan string, 4)
	for i := 0; i < 4; i++ {
		go func() {
			status, body := a.get(t, true)
			done <- fmt.Sprintf("%d:%s", status, body)
		}()
	}
	for i := 0; i < 4; i++ {
		out := <-done
		if !strings.Contains(out, "3.0.0") {
			t.Errorf("concurrent response = %s", out)
		}
	}
	if n := a.github.requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 (single-flight)", n)
	}
}
