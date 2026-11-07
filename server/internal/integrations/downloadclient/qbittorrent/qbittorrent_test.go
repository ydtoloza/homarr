package qbittorrent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/qbittorrent"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// fakeQbit emulates the qBittorrent WebUI v2 API surface the client touches:
// auth/login (Set-Cookie SID), app/version (TEXT), torrents/info, transfer/info
// and the mutation endpoints. version selects the v4/v5 endpoint behavior.
type fakeQbit struct {
	t        *testing.T
	version  string // e.g. "v5.0.2" or "v4.6.0"
	apiKey   string // when set, bearer auth is expected and login is refused
	loginOK  bool
	requests []capturedRequest
	torrents func(f *fakeQbit, r *http.Request) string
}

type capturedRequest struct {
	method, path string
	query        urlValues
	form         string
	header       http.Header
	body         string
}

type urlValues map[string]string

func (f *fakeQbit) capture(r *http.Request, form string) capturedRequest {
	q := urlValues{}
	for k := range r.URL.Query() {
		q[k] = r.URL.Query().Get(k)
	}
	body := form
	if body == "" {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
	}
	c := capturedRequest{method: r.Method, path: r.URL.Path, query: q, form: form, header: r.Header.Clone(), body: body}
	f.requests = append(f.requests, c)
	return c
}

func (f *fakeQbit) requireAuth(r *http.Request) {
	if f.apiKey != "" {
		if got := r.Header.Get("Authorization"); got != "Bearer "+f.apiKey {
			f.t.Fatalf("Authorization = %q, want bearer %q", got, "Bearer "+f.apiKey)
		}
		return
	}
	if got := r.Header.Get("Cookie"); got != "SID=sid-123" {
		f.t.Fatalf("Cookie = %q, want SID=sid-123", got)
	}
}

func newFakeQbit(t *testing.T, version string) (*fakeQbit, *httptest.Server) {
	t.Helper()
	f := &fakeQbit{t: t, version: version, loginOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			f.t.Fatalf("login parse form: %v", err)
		}
		f.capture(r, r.Form.Encode())
		if !f.loginOK || f.apiKey != "" {
			// qBittorrent answers 200 "Fails." without a session cookie.
			_, _ = io.WriteString(w, "Fails.")
			return
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
			f.t.Fatalf("login content-type = %q", got)
		}
		want := "password=secret&username=admin" // Go sorts form keys; the order is not significant for the server
		if r.Form.Encode() != want {
			f.t.Fatalf("login body = %q, want %q", r.Form.Encode(), want)
		}
		// Set-Cookie without a Domain attr exercises the first-pair parse.
		w.Header().Add("Set-Cookie", "SID=sid-123; path=/; HttpOnly")
		_, _ = io.WriteString(w, "Ok.")
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		f.capture(r, "")
		f.requireAuth(r)
		_, _ = io.WriteString(w, f.version) // TEXT response
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		f.capture(r, "")
		f.requireAuth(r)
		if f.torrents != nil {
			_, _ = io.WriteString(w, f.torrents(f, r))
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/api/v2/transfer/info", func(w http.ResponseWriter, r *http.Request) {
		f.capture(r, "")
		f.requireAuth(r)
		_, _ = io.WriteString(w, `{"dl_info_speed": 1200, "up_info_speed": 340}`)
	})
	for _, endpoint := range []string{"/torrents/pause", "/torrents/resume", "/torrents/stop", "/torrents/start", "/torrents/delete"} {
		ep := endpoint
		mux.HandleFunc("/api/v2"+ep, func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				f.t.Fatalf("parse form: %v", err)
			}
			f.capture(r, r.Form.Encode())
			f.requireAuth(r)
			w.WriteHeader(http.StatusOK)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-q", Kind: "qBittorrent", Name: "qBittorrent", URL: url}
}

func loginSecrets() qbittorrent.Secrets {
	return qbittorrent.Secrets{Username: "admin", Password: "secret"}
}

func lastRequest(f *fakeQbit) capturedRequest {
	return f.requests[len(f.requests)-1]
}

// TestLoginCookieFlow pins the cookie auth: POST form login, SID cookie on
// subsequent requests, version probe before torrents/info.
func TestLoginCookieFlow(t *testing.T) {
	f, srv := newFakeQbit(t, "v4.6.0")
	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50}); err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	paths := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		paths = append(paths, r.path)
	}
	want := []string{"/api/v2/auth/login", "/api/v2/app/version", "/api/v2/torrents/info"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
	for _, r := range f.requests[1:] {
		if got := r.header.Get("Cookie"); got != "SID=sid-123" {
			t.Fatalf("cookie = %q", got)
		}
	}
}

// TestAPIKeyBearer pins the apiKey mode: Bearer header, no login, no cookie.
func TestAPIKeyBearer(t *testing.T) {
	f, srv := newFakeQbit(t, "v5.0.2")
	f.apiKey = "token-9"
	client := qbittorrent.New(testInput(srv.URL), qbittorrent.Secrets{APIKey: "token-9"})
	if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50}); err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	for _, r := range f.requests {
		if r.path == "/api/v2/auth/login" {
			t.Fatal("apiKey mode must not log in")
		}
		if got := r.header.Get("Authorization"); got != "Bearer token-9" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.header.Get("Cookie"); got != "" {
			t.Fatalf("apiKey mode must not send a cookie, got %q", got)
		}
	}
}

func TestCheckRejectedLogin(t *testing.T) {
	f, srv := newFakeQbit(t, "v4.6.0")
	f.loginOK = false
	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error == "" {
		t.Fatalf("expected a definitive error, got %+v", res)
	}
}

func TestCheckTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client := qbittorrent.New(testInput(url), loginSecrets())
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != mediaorganizer.MsgRequest {
		t.Fatalf("error = %q", res.Error)
	}
}

// TestV4VersusV5MutationEndpoints pins the version-dependent endpoints:
// v4 pause/resume, v5 stop/start (qbittorrent.js:646-670).
func TestV4VersusV5MutationEndpoints(t *testing.T) {
	cases := []struct {
		version      string
		pause, start string
	}{
		{"v4.6.0", "/api/v2/torrents/pause", "/api/v2/torrents/resume"},
		{"v5.0.2", "/api/v2/torrents/stop", "/api/v2/torrents/start"},
	}
	for _, tc := range cases {
		f, srv := newFakeQbit(t, tc.version)
		client := qbittorrent.New(testInput(srv.URL), loginSecrets())
		item := downloadclient.Item{ID: "abc", Progress: 0.5}
		if err := client.PauseQueue(context.Background()); err != nil {
			t.Fatalf("%s PauseQueue: %v", tc.version, err)
		}
		if got := lastRequest(f); got.path != tc.pause || got.form != "hashes=all" {
			t.Fatalf("%s pause: %s %s", tc.version, got.path, got.form)
		}
		if err := client.ResumeQueue(context.Background()); err != nil {
			t.Fatalf("%s ResumeQueue: %v", tc.version, err)
		}
		if got := lastRequest(f); got.path != tc.start || got.form != "hashes=all" {
			t.Fatalf("%s resume: %s %s", tc.version, got.path, got.form)
		}
		if err := client.PauseItem(context.Background(), item); err != nil {
			t.Fatalf("%s PauseItem: %v", tc.version, err)
		}
		if got := lastRequest(f); got.form != "hashes=abc" {
			t.Fatalf("%s pause item form = %q", tc.version, got.form)
		}
		if err := client.ResumeItem(context.Background(), item); err != nil {
			t.Fatalf("%s ResumeItem: %v", tc.version, err)
		}
		if err := client.DeleteItem(context.Background(), item, true); err != nil {
			t.Fatalf("%s DeleteItem: %v", tc.version, err)
		}
		if got := lastRequest(f); got.path != "/api/v2/torrents/delete" ||
			got.form != "deleteFiles=true&hashes=abc" {
			t.Fatalf("%s delete: %s %q", tc.version, got.path, got.form)
		}
	}
}

// TestFilterTranslation pins the v4/v5 filter renaming of torrents/info
// (qbittorrent.js:218-234): the "running" filter of the selection path
// becomes "resumed" on v4 and stays "running" on v5.
func TestFilterTranslation(t *testing.T) {
	for _, tc := range []struct {
		version, wantFilter string
	}{{"v4.6.0", "resumed"}, {"v5.0.2", "running"}} {
		f, srv := newFakeQbit(t, tc.version)
		client := qbittorrent.New(testInput(srv.URL), loginSecrets())
		input := downloadclient.JobsInput{Limit: 50, Selection: &downloadclient.Selection{Sort: "name", CategoryFilter: []string{"tv"}, FilterIsWhitelist: true}}
		if _, err := client.GetJobsAndStatus(context.Background(), input); err != nil {
			t.Fatalf("%s GetJobsAndStatus: %v", tc.version, err)
		}
		var running string
		for _, r := range f.requests {
			if r.path == "/api/v2/torrents/info" && r.query["filter"] != "" {
				running = r.query["filter"]
			}
		}
		if running != tc.wantFilter {
			t.Fatalf("%s running filter = %q, want %q", tc.version, running, tc.wantFilter)
		}
	}
}

// TestItemsShapeAndStatus pins the item mapping and the status computation
// without selection (:33-70): rates summed over the listed torrents, paused
// when every state maps to paused, eta 8640000 -> time 0, completion time
// negative.
func TestItemsShapeAndStatus(t *testing.T) {
	f, srv := newFakeQbit(t, "v4.6.0")
	f.torrents = func(f *fakeQbit, r *http.Request) string {
		return `[
			{"hash": "h1", "name": "One", "state": "downloading", "size": 1000, "uploaded": 100,
			 "progress": 0.5, "dlspeed": 300, "upspeed": 40, "eta": 120, "added_on": 1700000000,
			 "completion_on": 1700000000, "priority": 3, "category": "movies"},
			{"hash": "h2", "name": "Two", "state": "stalledUP", "size": 500, "uploaded": 900,
			 "progress": 1, "dlspeed": 0, "upspeed": 60, "eta": 8640000, "added_on": 1700000001,
			 "completion_on": 1700000100, "priority": 0, "category": ""},
			{"hash": "h3", "name": "Three", "state": "pausedDL", "size": 10, "uploaded": 0,
			 "progress": 0.2, "dlspeed": 0, "upspeed": 0, "eta": 0, "added_on": 1700000002,
			 "completion_on": 0, "priority": -1}
		]`
	}
	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	// fixed "now": 1700000200s, after every completion_on of the fixture
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	if jobs.Status.Paused {
		t.Fatal("paused must be false (h1 is leeching)")
	}
	if jobs.Status.Rates.Down != 300 || *jobs.Status.Rates.Up != 100 {
		t.Fatalf("rates = %+v", jobs.Status.Rates)
	}
	if len(jobs.Status.Types) != 1 || jobs.Status.Types[0] != "torrent" {
		t.Fatalf("types = %v", jobs.Status.Types)
	}
	raw, err := json.Marshal(jobs.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[` +
		`{"id":"h1","index":3,"name":"One","type":"torrent","size":1000,"sent":100,"downSpeed":300,"upSpeed":40,"time":120000,"added":1700000000000,"state":"leeching","progress":0.5,"category":"movies"},` +
		`{"id":"h2","index":0,"name":"Two","type":"torrent","size":500,"sent":900,"upSpeed":60,"time":-100000,"added":1700000001000,"state":"seeding","progress":1,"category":""},` +
		`{"id":"h3","index":-1,"name":"Three","type":"torrent","size":10,"sent":0,"downSpeed":0,"upSpeed":0,"time":0,"added":1700000002000,"state":"paused","progress":0.2}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}

}

// TestSelectionPagingAndRatioItems pins the selection path: paged
// torrents/info with offset stepping, transfer/info status, running filter
// and ratioItems (:72-85, 113-173).
func TestSelectionPagingAndRatioItems(t *testing.T) {
	f, srv := newFakeQbit(t, "v5.0.2")
	// Page 0 is a FULL page (100 torrents) so the loop continues to the
	// second page; page 1 is empty and stops it (:169-170).
	page := 0
	f.torrents = func(f *fakeQbit, r *http.Request) string {
		if r.URL.Query().Get("sort") == "" {
			return `[{"hash": "h1", "name": "One", "state": "downloading", "size": 1000, "uploaded": 100, "progress": 0.5, "dlspeed": 300, "upspeed": 40, "eta": 5, "added_on": 1, "completion_on": 1, "priority": 1, "category": "movies"}]`
		}
		page++
		offset := r.URL.Query().Get("offset")
		if offset == "0" {
			var torrents []string
			for i := 0; i < 100; i++ {
				torrents = append(torrents, `{"hash": "p0-`+strconv.Itoa(i)+`", "name": "N", "state": "downloading", "size": 1, "uploaded": 1, "progress": 0.5, "dlspeed": 1, "upspeed": 1, "eta": 1, "added_on": 1, "completion_on": 1, "priority": 1, "category": "movies"}`)
			}
			return `[` + strings.Join(torrents, ",") + `]`
		}
		if offset == "100" {
			return `[]` // empty second page stops the loop
		}
		f.t.Fatalf("unexpected offset %q", offset)
		return `[]`
	}
	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	input := downloadclient.JobsInput{
		// 150 > pageSize(100): the loop must continue to the second page.
		Limit: 150,
		Selection: &downloadclient.Selection{
			Sort: "name", Descending: false,
			CategoryFilter:         []string{"movies"},
			FilterIsWhitelist:      true,
			ShowCompletedTorrent:   true,
			ActiveTorrentThreshold: 0,
		},
	}
	jobs, err := client.GetJobsAndStatus(context.Background(), input)
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	if page != 2 {
		t.Fatalf("selection pages visited = %d, want 2", page)
	}
	// Status recomputed from transfer/info + running filter.
	if jobs.Status.Paused {
		t.Fatal("paused must be false when a torrent is running")
	}
	if jobs.Status.Rates.Down != 1200 || *jobs.Status.Rates.Up != 340 {
		t.Fatalf("rates = %+v", jobs.Status.Rates)
	}
	// Page 0 contributes its 100 torrents; the loop stops on the empty
	// second page.
	if len(jobs.Items) != 100 || jobs.Items[0].ID != "p0-0" {
		t.Fatalf("items = %d entries, first %v", len(jobs.Items), jobs.Items[0].ID)
	}
	if len(jobs.RatioItems) != 1 || jobs.RatioItems[0].Size != 1000 {
		t.Fatalf("ratioItems = %+v", jobs.RatioItems)
	}
	raw, _ := json.Marshal(jobs.RatioItems)
	want := `[{"sent":100,"size":1000,"progress":0.5,"category":"movies"}]`
	if string(raw) != want {
		t.Fatalf("ratioItems mismatch: got %s want %s", raw, want)
	}
	// torrents/info paging must have used pageSize=100 (filters active) and
	// the category whitelist.
	var infoRequests int
	for _, r := range f.requests {
		if r.path != "/api/v2/torrents/info" {
			continue
		}
		if r.query["filter"] == "running" {
			continue // the status probe uses limit=1 (listTorrents {filter:"running", limit:1})
		}
		if _, isSelectionPage := r.query["sort"]; !isSelectionPage {
			continue // the ratioItems paging uses min(limit, 100), not the selection pageSize
		}
		infoRequests++
		if r.query["limit"] != "100" {
			t.Fatalf("limit = %q, want 100 when filters are active", r.query["limit"])
		}
		if r.query["category"] != "movies" {
			t.Fatalf("category = %q", r.query["category"])
		}
		if r.query["reverse"] != "false" {
			t.Fatalf("reverse = %q, want \"false\" (JSON.stringify)", r.query["reverse"])
		}
	}
	if infoRequests != 2 {
		t.Fatalf("expected two selection pages, got %d", infoRequests)
	}
	_ = strconv.Itoa
}

// TestReloginOn401 pins the one-shot re-login of requestWithAuth
// (qbittorrentSession.js:148-160): the first /app/version attempt is rejected
// with 403 (an expired session server-side), the client re-logs in once and
// the retried request succeeds.
func TestReloginOn401(t *testing.T) {
	logins := 0
	versionAttempts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins++
		w.Header().Add("Set-Cookie", "SID=sid-"+strconv.Itoa(logins)+"; path=/")
		_, _ = io.WriteString(w, "Ok.")
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		versionAttempts++
		if versionAttempts == 1 {
			// The session the client just obtained is treated as expired.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "v4.6.0")
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "SID=sid-2" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := qbittorrent.New(testInput(srv.URL), loginSecrets())
	if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 1}); err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	if logins != 2 {
		t.Fatalf("logins = %d, want the initial login plus the re-login", logins)
	}
}
