package bangs_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/bangs"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// catalogFixture mirrors a slice of the real bang.js payload: unsorted,
// mixed-case tokens, optional fields present/absent, an unknown key the zod
// schema strips and an empty-token entry the filter drops.
const catalogFixture = `[
	{"t":"!YouTube","s":"YouTube","u":"https://youtube.com/results?q={{{s}}}","r":1300,"d":"youtube.com","c":"Multimedia","x":"ignored"},
	{"t":"","s":"Empty token dropped","u":"https://example.com/?q={{{s}}}"},
	{"t":"yt","s":"YouTube","u":"https://www.youtube.com/results?search_query={{{s}}}"},
	{"t":"gh","s":"GitHub","u":"https://github.com/search?q={{{s}}}","d":"github.com"},
	{"t":"gi","s":"Google Images","u":"https://images.google.com/","c":"Multimedia"}
]`

// fakeUpstream serves bodies from a mutable slice of responses; every fetch
// consumes the next entry (the last one repeats) and counts the hits.
type fakeUpstream struct {
	srv        *httptest.Server
	responses  []fakeResponse
	hits       int64
	lastAccept atomic.Value
}

type fakeResponse struct {
	status int
	body   string
}

func newFakeUpstream(t *testing.T, responses ...fakeResponse) *fakeUpstream {
	t.Helper()
	if len(responses) == 0 {
		responses = []fakeResponse{{http.StatusOK, catalogFixture}}
	}
	f := &fakeUpstream{responses: responses}
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&f.hits, 1)
		f.lastAccept.Store(r.Header.Get("accept"))
		idx := int(n) - 1
		if idx >= len(f.responses) {
			idx = len(f.responses) - 1
		}
		w.WriteHeader(f.responses[idx].status)
		_, _ = io.WriteString(w, f.responses[idx].body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) hitCount() int64 { return atomic.LoadInt64(&f.hits) }

func (f *fakeUpstream) acceptHeader() string {
	v, _ := f.lastAccept.Load().(string)
	return v
}

// newTestRouter mounts the router against the fake upstream. The fake URL
// travels through Options.CatalogURL and the upstream 10s TTL is shrunk to
// 30ms so expiry tests stay fast.
func newTestRouter(t *testing.T, f *fakeUpstream) *trpc.Router {
	t.Helper()
	d := db.OpenTest(t)
	r := trpc.New()
	bangs.Register(r, d, bangs.Options{CatalogURL: f.srv.URL, CacheTTL: 30 * time.Millisecond})
	return r
}

// callSearch invokes bangs.search with the given superjson input and returns
// status + body.
func callSearch(t *testing.T, r *trpc.Router, input string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/trpc/bangs.search",
		strings.NewReader(`{"json":`+input+`}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("bangs.search: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res.StatusCode, string(raw)
}

// extractData unwraps {"result":{"data":{"json":...}}} or fails on an error
// envelope.
func extractData(t *testing.T, body string) json.RawMessage {
	t.Helper()
	var envelope struct {
		Result struct {
			Data struct {
				JSON json.RawMessage `json:"json"`
			} `json:"data"`
		} `json:"result"`
		Error *struct {
			JSON struct {
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("unexpected error envelope: %s", body)
	}
	return envelope.Result.Data.JSON
}

// TestBangsSearchShapeAndOrder pins the response contract of
// searchDuckDuckGoBangsAsync: prefix matches over the normalized token,
// sorted, with the exact bang shape (t/s/d/u/c/sc/r; optional members
// omitted when absent; unknown upstream keys stripped).
func TestBangsSearchShapeAndOrder(t *testing.T) {
	f := newFakeUpstream(t)
	r := newTestRouter(t, f)

	// Prefix "g" matches gh + gi in byte order, with the fixture fields kept
	// and the unknown "x" key stripped.
	status, body := callSearch(t, r, `{"query":"g","limit":10}`)
	if status != http.StatusOK {
		t.Fatalf("search status = %d (body %s)", status, body)
	}
	if got, want := string(extractData(t, body)),
		`[{"t":"gh","s":"GitHub","d":"github.com","u":"https://github.com/search?q={{{s}}}"},`+
			`{"t":"gi","s":"Google Images","u":"https://images.google.com/","c":"Multimedia"}]`; got != want {
		t.Fatalf("search g =\n%s\nwant\n%s", got, want)
	}

	// Mixed case and surrounding spaces normalize like upstream
	// (normalizeBangToken = toLowerCase().trim()).
	status, body = callSearch(t, r, `{"query":"  YT  ","limit":10}`)
	if status != http.StatusOK {
		t.Fatalf("search status = %d (body %s)", status, body)
	}
	if got, want := string(extractData(t, body)),
		`[{"t":"yt","s":"YouTube","u":"https://www.youtube.com/results?search_query={{{s}}}"}]`; got != want {
		t.Fatalf("search yt = %s, want %s", got, want)
	}

	// Bang tokens may carry symbols; the full optional field set survives.
	status, body = callSearch(t, r, `{"query":"!","limit":10}`)
	if status != http.StatusOK {
		t.Fatalf("search status = %d (body %s)", status, body)
	}
	if got, want := string(extractData(t, body)),
		`[{"t":"!youtube","s":"YouTube","d":"youtube.com","u":"https://youtube.com/results?q={{{s}}}","c":"Multimedia","r":1300}]`; got != want {
		t.Fatalf("search ! = %s, want %s", got, want)
	}

	// Empty/blank query -> []; no match -> [].
	for _, q := range []string{`{"query":"  ","limit":10}`, `{"query":"zzz","limit":10}`} {
		status, body = callSearch(t, r, q)
		if status != http.StatusOK || string(extractData(t, body)) != "[]" {
			t.Fatalf("search %s = %d %s, want 200 []", q, status, body)
		}
	}

	// limit bounds the output (limit 1 over "g" -> only gh).
	status, body = callSearch(t, r, `{"query":"g","limit":1}`)
	if status != http.StatusOK {
		t.Fatalf("search status = %d (body %s)", status, body)
	}
	if n := strings.Count(string(extractData(t, body)), `"t":`); n != 1 {
		t.Fatalf("search g limit 1 returned %d entries, want 1 (%s)", n, body)
	}

	// All of the above ran on the single fetch the cache allowed.
	if n := f.hitCount(); n != 1 {
		t.Fatalf("upstream fetches = %d, want 1", n)
	}
	if got, want := f.acceptHeader(), "application/json,text/plain,*/*"; got != want {
		t.Fatalf("upstream accept header = %q, want %q", got, want)
	}
}

// TestBangsSearchInputValidation pins the zod input schema of bangs-router.ts
// (query required string; limit int 1-50 default 20).
func TestBangsSearchInputValidation(t *testing.T) {
	f := newFakeUpstream(t)
	r := newTestRouter(t, f)

	cases := []struct {
		input string
		ok    bool
	}{
		{`{"query":"g"}`, true},               // default limit 20
		{`{"query":"g","limit":50}`, true},    // upper bound
		{`{"query":"g","limit":1}`, true},     // lower bound
		{`{"query":"g","limit":0}`, false},    // below min
		{`{"query":"g","limit":51}`, false},   // above max
		{`{"query":"g","limit":1.5}`, false},  // not an integer
		{`{"limit":10}`, false},               // query required
		{`{"query":"g","limit":"10"}`, false}, // not a number
	}
	for _, tc := range cases {
		status, body := callSearch(t, r, tc.input)
		if tc.ok && status != http.StatusOK {
			t.Fatalf("search %s = %d %s, want 200", tc.input, status, body)
		}
		if !tc.ok && status != http.StatusBadRequest {
			t.Fatalf("search %s = %d %s, want 400", tc.input, status, body)
		}
	}
}

// TestBangsSearchCache pins the request-handler cache semantics: searches
// inside the TTL hit the memory cache (one upstream fetch) and expiry
// triggers a re-fetch (upstream serves no stale data:
// fallbackToStaleOnError is unset).
func TestBangsSearchCache(t *testing.T) {
	f := newFakeUpstream(t)
	r := newTestRouter(t, f)

	if _, body := callSearch(t, r, `{"query":"g","limit":10}`); !strings.Contains(body, "GitHub") {
		t.Fatalf("first search = %s", body)
	}
	// Cache hit: no second fetch inside the TTL window.
	if _, body := callSearch(t, r, `{"query":"yt","limit":10}`); !strings.Contains(body, "youtube") {
		t.Fatalf("second search = %s", body)
	}
	if n := f.hitCount(); n != 1 {
		t.Fatalf("upstream hits after cache hit = %d, want 1", n)
	}
	// After expiry the catalog is fetched again.
	time.Sleep(60 * time.Millisecond)
	if _, body := callSearch(t, r, `{"query":"gh","limit":10}`); !strings.Contains(body, "GitHub") {
		t.Fatalf("third search = %s", body)
	}
	if n := f.hitCount(); n != 2 {
		t.Fatalf("upstream hits after expiry = %d, want 2", n)
	}
}

// TestBangsSearchUpstreamError pins the error behavior: a non-2xx catalog
// answer surfaces as INTERNAL_SERVER_ERROR with the exact upstream
// ResponseError message ("Response did not indicate success",
// packages/common/src/errors/http/response-error.ts:13), the failure is not
// cached, the next call retries, and a recovered upstream is served.
func TestBangsSearchUpstreamError(t *testing.T) {
	f := newFakeUpstream(t,
		fakeResponse{http.StatusInternalServerError, `{"error":"boom"}`},
		fakeResponse{http.StatusInternalServerError, `{"error":"boom"}`},
		fakeResponse{http.StatusOK, catalogFixture},
	)
	r := newTestRouter(t, f)

	status, body := callSearch(t, r, `{"query":"g","limit":10}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("search over failing upstream = %d %s, want 500", status, body)
	}
	if !strings.Contains(body, `"message":"Response did not indicate success"`) ||
		!strings.Contains(body, `"code":"INTERNAL_SERVER_ERROR"`) {
		t.Fatalf("error body = %s, want the exact upstream ResponseError envelope", body)
	}
	status, _ = callSearch(t, r, `{"query":"g","limit":10}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("retry = %d, want 500 (errors are not cached)", status)
	}
	status, body = callSearch(t, r, `{"query":"g","limit":10}`)
	if status != http.StatusOK || !strings.Contains(body, "GitHub") {
		t.Fatalf("recovered search = %d %s, want 200", status, body)
	}
	if n := f.hitCount(); n != 3 {
		t.Fatalf("upstream hits = %d, want 3", n)
	}
}

// TestBangsSearchInvalidCatalog pins the schema behavior: a catalog entry
// missing a required key (or with a wrong type) fails the whole parse with
// INTERNAL_SERVER_ERROR, like duckDuckGoBangsResponseSchema rejecting the
// array wholesale (duckduckgo-bangs.ts:33).
func TestBangsSearchInvalidCatalog(t *testing.T) {
	for name, catalog := range map[string]string{
		"missing token": `[{"s":"YouTube","u":"https://youtube.com"}]`,
		"missing name":  `[{"t":"yt","u":"https://youtube.com"}]`,
		"missing url":   `[{"t":"yt","s":"YouTube"}]`,
		"wrong types":   `[{"t":1,"s":"YouTube","u":"https://youtube.com"}]`,
		"not an array":  `{"t":"yt","s":"YouTube","u":"https://youtube.com"}`,
		"invalid json":  `[{"t":"yt"`,
	} {
		f := newFakeUpstream(t, fakeResponse{http.StatusOK, catalog})
		r := newTestRouter(t, f)
		status, got := callSearch(t, r, `{"query":"g","limit":10}`)
		if status != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d %s, want 500", name, status, got)
		}
		if !strings.Contains(got, `"code":"INTERNAL_SERVER_ERROR"`) {
			t.Fatalf("%s: body = %s, want INTERNAL_SERVER_ERROR envelope", name, got)
		}
	}
}

// TestBangsSearchDefaultsToRealUpstream pins that the zero Options proxies
// the real DuckDuckGo endpoint (DUCKDUCKGO_BANGS_URL,
// duckduckgo-bangs.ts:11); the endpoint itself is never contacted in tests.
func TestBangsSearchDefaultsToRealUpstream(t *testing.T) {
	if got, want := bangs.DefaultCatalogURL, "https://duckduckgo.com/bang.js"; got != want {
		t.Fatalf("DefaultCatalogURL = %q, want %q", got, want)
	}
	// The zero Options registers cleanly (no fetch happens until a call).
	d := db.OpenTest(t)
	r := trpc.New()
	bangs.Register(r, d, bangs.Options{})
	_ = context.Background
}
