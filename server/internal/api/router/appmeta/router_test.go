package appmeta_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/appmeta"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness: a fake website (httptest) plus the shared database as the
// icon source, sessions injected through headers.

type testAPI struct {
	db   *db.DB
	srv  *httptest.Server
	site *fakeSite
}

type fakeSite struct {
	server   *httptest.Server
	requests atomic.Int64
	html     atomic.Value // string
	status   atomic.Int64
	favicon  string // body content type of /favicon.ico
}

func (f *fakeSite) url() string { return f.server.URL }

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	d := db.OpenTest(t)

	site := &fakeSite{favicon: "image/png"}
	site.html.Store("")
	site.status.Store(200)
	site.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.requests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/favicon.ico"):
			w.Header().Set("Content-Type", site.favicon)
			w.WriteHeader(200)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(int(site.status.Load()))
			_, _ = w.Write([]byte(site.html.Load().(string)))
		}
	}))
	t.Cleanup(site.server.Close)

	r := trpc.New()
	appmeta.Register(r, d, appmeta.Options{HTTPClient: site.server.Client()})

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test-Admin") == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u1", IsAdministrator: true}))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{db: d, srv: srv, site: site}
}

func (a *testAPI) detect(t *testing.T, href string) (int, string) {
	t.Helper()
	body := `{"json":{"href":` + jsonString(href) + `}}`
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/trpc/app.detectMetadata", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Admin", "1")
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// seedIconRepository satisfies the icon FK.
func (a *testAPI) seedIconRepository(t *testing.T, id, slug string) {
	t.Helper()
	if _, err := a.db.ExecContext(context.Background(),
		`INSERT INTO "iconRepository" (id, slug) VALUES (?, ?)`, id, slug); err != nil {
		t.Fatalf("seed icon repository: %v", err)
	}
}

func (a *testAPI) seedIcon(t *testing.T, id, name, url, repoID string) {
	t.Helper()
	if _, err := a.db.ExecContext(context.Background(),
		`INSERT INTO "icon" (id, name, url, checksum, icon_repository_id) VALUES (?, ?, ?, 'sha', ?)`,
		id, name, url, repoID); err != nil {
		t.Fatalf("seed icon: %v", err)
	}
}

// The happy path: title + declared icon from the page head, unknown keys and
// unsupported relations ignored.
func TestDetectMetadataTitleAndIcon(t *testing.T) {
	a := newTestAPI(t)
	a.site.html.Store(`<!DOCTYPE html><html><head>
		<link rel="preload" href="ignore-me.png" as="image">
		<link rel="icon" href="/static/icon-32.png" sizes="32x32">
		<link rel="apple-touch-icon" href="/static/icon-180.png" sizes="180x180">
		<title> My &amp; Great &#x41;pp </title>
		</head><body>hello</body></html>`)

	status, body := a.detect(t, a.site.url())
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	// The apple-touch-icon outranks the plain icon (relation score 3 > 2)
	// regardless of its larger size; entities decode; whitespace collapses
	// and trims (favicon-fetcher.ts:180-182).
	want := `{"result":{"data":{"json":{"name":"My & Great App","iconUrl":"` + a.site.url() + `/static/icon-180.png"}}}}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
}

// The relation/size ranking: same relation → the largest area wins.
func TestDetectMetadataSizeRanking(t *testing.T) {
	a := newTestAPI(t)
	a.site.html.Store(`<head><link rel="icon" href="/small.png" sizes="32x32">
		<link rel="icon" href="/big.png" sizes="180x180"></head>`)

	_, body := a.detect(t, a.site.url())
	if !strings.Contains(body, "/big.png") {
		t.Errorf("body = %s, want the largest icon", body)
	}
}

// No declared icon: the well known /favicon.ico of the origin that answered.
func TestDetectMetadataWellKnownFallback(t *testing.T) {
	a := newTestAPI(t)
	a.site.html.Store(`<head><title>Bare</title></head>`)

	_, body := a.detect(t, a.site.url()+"/deep/path")
	if !strings.Contains(body, `"iconUrl":"`+a.site.url()+`/favicon.ico"`) {
		t.Errorf("body = %s, want the well known favicon", body)
	}
}

// Title fallbacks: hostname when there is no title; the icon search against
// the icon table when the site has no icon; the default icon as the last
// resort.
func TestDetectMetadataFallbacks(t *testing.T) {
	a := newTestAPI(t)
	// The well known favicon probe must fail for the lower fallbacks to
	// kick in (an HTML content type is not an icon).
	a.site.favicon = "text/html"
	a.seedIconRepository(t, "repo-1", "dashboards")
	a.seedIcon(t, "icon-1", "Jellyseerr", "https://icons.example/jellyseerr.svg", "repo-1")

	// No title, no icon: name = hostname, icon = the LIKE search hit
	// (192.168.x contains no match → hmm, the hostname is 127.0.0.1; seed a
	// name the hostname matches).
	a.seedIcon(t, "icon-2", "127.0.0.1-special", "https://icons.example/local.svg", "repo-1")
	a.site.html.Store(`<head><link rel="stylesheet" href="x.css"></head>`)
	_, body := a.detect(t, a.site.url())
	if !strings.Contains(body, `"iconUrl":"https://icons.example/local.svg"`) {
		t.Errorf("body = %s, want the icon-search hit", body)
	}
	if !strings.Contains(body, `"name":"127.0.0.1"`) {
		t.Errorf("body = %s, want the hostname fallback", body)
	}

	// No icon row matches: the default icon.
	a.site.html.Store(`<head><title>Unmatched Title</title></head>`)
	_, body = a.detect(t, a.site.url())
	if !strings.Contains(body, `"iconUrl":"https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons@master/svg/homarr.svg"`) {
		t.Errorf("body = %s, want the default icon", body)
	}
	if !strings.Contains(body, `"name":"Unmatched Title"`) {
		t.Errorf("body = %s, want the title as name", body)
	}
}

// Site failures degrade: a non-ok page falls back to the well known probe;
// a site that answers everything with HTML keeps the hostname fallback.
func TestDetectMetadataSiteFailures(t *testing.T) {
	a := newTestAPI(t)
	a.site.favicon = "text/html"
	a.site.status.Store(500)
	_, body := a.detect(t, a.site.url())
	// The well known probe also answers 200 with the html content type →
	// not an icon → the default icon via the icon search (no rows) → default.
	if !strings.Contains(body, `"iconUrl":"https://cdn.jsdelivr.net`) {
		t.Errorf("body = %s, want the default icon", body)
	}
}

// Validation and gates: non-http schemes, unparseable hrefs and anonymous
// callers fail like upstream.
func TestDetectMetadataValidationAndGates(t *testing.T) {
	a := newTestAPI(t)

	for _, href := range []string{"javascript:alert(1)", "ftp://x", "not a url"} {
		status, body := a.detect(t, href)
		if status != http.StatusBadRequest {
			t.Errorf("href %q = %d %s, want 400", href, status, body)
		}
	}

	// Anonymous: UNAUTHORIZED.
	body := `{"json":{"href":"http://example.com"}}`
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/api/trpc/app.detectMetadata", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous = %d %s, want 401", res.StatusCode, raw)
	}
}
