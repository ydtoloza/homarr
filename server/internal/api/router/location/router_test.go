package location_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/router/location"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// harness with a fake Open-Meteo endpoint.
type testAPI struct {
	srv     *httptest.Server
	geocode *fakeGeocoder
}

type fakeGeocoder struct {
	server  *httptest.Server
	body    string
	status  int
	queries []string
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	d := db.OpenTest(t)
	g := &fakeGeocoder{status: 200, body: `{"results":[]}`}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil {
			g.queries = append(g.queries, r.Form.Get("name"))
		}
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	t.Cleanup(g.server.Close)

	r := trpc.New()
	location.Register(r, d, location.Options{GeocodingURL: g.server.URL})
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return &testAPI{srv: srv, geocode: g}
}

func (a *testAPI) searchCity(t *testing.T, query string) (int, string) {
	t.Helper()
	input := url.QueryEscape(`{"json":{"query":` + jsonString(query) + `}}`)
	res, err := a.srv.Client().Get(a.srv.URL + "/api/trpc/location.searchCity?input=" + input)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// The happy path: required keys kept, optionals omitted when absent, unknown
// API keys stripped, the name parameter carried as the query.
func TestSearchCityResults(t *testing.T) {
	a := newTestAPI(t)
	a.geocode.body = `{"results":[
		{"id":2950159,"name":"Berlin","latitude":52.52,"longitude":13.405,
		 "country":"Germany","country_code":"DE","population":3426354,
		 "elevation":74,"timezone":"Europe/Berlin","admin1":"Berlin"},
		{"id":123,"name":"Nowhere","latitude":-1.5,"longitude":2.5}
	]}`

	status, body := a.searchCity(t, "ber lin")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	want := `{"result":{"data":{"json":{"results":[` +
		`{"id":2950159,"name":"Berlin","country":"Germany","country_code":"DE","latitude":52.52,"longitude":13.405,"population":3426354},` +
		`{"id":123,"name":"Nowhere","latitude":-1.5,"longitude":2.5}` +
		`]}}}}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	if len(a.geocode.queries) != 1 || a.geocode.queries[0] != "ber lin" {
		t.Errorf("queries = %v, want the input query", a.geocode.queries)
	}
}

// The generationtime fallback: a bare generationtime_ms object becomes an
// empty results list.
func TestSearchCityGenerationtimeFallback(t *testing.T) {
	a := newTestAPI(t)
	a.geocode.body = `{"generationtime_ms":0.123}`

	status, body := a.searchCity(t, "zzz")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if body != `{"result":{"data":{"json":{"results":[]}}}}` {
		t.Errorf("body = %s", body)
	}
}

// Shapes neither branch accepts fail the output schema
// (INTERNAL_SERVER_ERROR): keys beside a lone generationtime_ms fail the
// refine of the right branch (the left branch needs a results key), and
// an invalid city (missing required keys) fails the schema.
func TestSearchCityInvalidOutput(t *testing.T) {
	a := newTestAPI(t)

	a.geocode.body = `{"generationtime_ms":1.0,"foo":2}`
	status, body := a.searchCity(t, "x")
	if status != http.StatusInternalServerError || !strings.Contains(body, "INTERNAL_SERVER_ERROR") {
		t.Errorf("extra-keys body = %s (status %d), want 500", body, status)
	}

	a.geocode.body = `{"results":[{"id":1,"name":"x"}]}`
	status, body = a.searchCity(t, "x")
	if status != http.StatusInternalServerError || !strings.Contains(body, "INTERNAL_SERVER_ERROR") {
		t.Errorf("missing-keys body = %s (status %d), want 500", body, status)
	}

	a.geocode.body = `{"error":true,"reason":"bad request"}`
	status, body = a.searchCity(t, "x")
	if status != http.StatusInternalServerError || !strings.Contains(body, "INTERNAL_SERVER_ERROR") {
		t.Errorf("api-error body = %s (status %d), want 500", body, status)
	}
}

// Input validation: query is required.
func TestSearchCityInputValidation(t *testing.T) {
	a := newTestAPI(t)

	res, err := a.srv.Client().Get(a.srv.URL + "/api/trpc/location.searchCity?input=%7B%22json%22%3A%7B%7D%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "BAD_REQUEST") {
		t.Errorf("missing query = %s (status %d), want 400", raw, res.StatusCode)
	}
}
