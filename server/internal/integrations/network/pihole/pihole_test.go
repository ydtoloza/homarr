package pihole_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
	"github.com/ydtoloza/homarr/server/internal/integrations/network/pihole"
)

// fakePiHole answers the endpoints of BOTH versions and records every
// request (method + path + query + headers + body) in order.
type fakePiHole struct {
	t        *testing.T
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

func newFakePiHole(t *testing.T, handler func(f *fakePiHole, w http.ResponseWriter, r *http.Request)) (*fakePiHole, *httptest.Server) {
	t.Helper()
	f := &fakePiHole{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			body = string(raw[:n])
		}
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: body,
		})
		handler(f, w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-1", Kind: "piHole", Name: "Pi-hole", URL: url, ExternalURL: ""}
}

func countRequests(f *fakePiHole, path string) int {
	n := 0
	for _, req := range f.requests {
		if req.Path == path {
			n++
		}
	}
	return n
}

// --- version probe ---

// TestProbeSelectsV5On404 pins createPiHoleIntegrationAsync: a 404 on
// /api/info/login selects the v5 client.
func TestProbeSelectsV5On404(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/admin/api.php" {
			f.t.Fatalf("v5 must call /admin/api.php, got %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"enabled","domains_being_blocked":100001,"ads_blocked_today":205,"dns_queries_today":5023,"ads_percentage_today":4.1}`))
	})
	client, err := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Name: "Pi-hole", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	if err != nil {
		t.Fatalf("NewFromRef: %v", err)
	}
	if v := versionOf(client); v != 5 {
		t.Fatalf("version = %d, want 5", v)
	}
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary.Status == nil || *summary.Status != "enabled" ||
		summary.DomainsBeingBlocked != 100001 || summary.AdsBlockedToday != 205 ||
		summary.AdsBlockedTodayPercentage != 4.1 || summary.DNSQueriesToday != 5023 {
		t.Fatalf("summary = %+v", summary)
	}
}

// TestV5SummaryQueryAndAuth pins the v5 request: the apiKey travels as the
// "auth" QUERY parameter next to summaryRaw.
func TestV5SummaryQueryAndAuth(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusNotFound)
		case "/admin/api.php":
			if got := r.URL.Query().Get("auth"); got != "pw" {
				f.t.Fatalf("auth = %q", got)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	if _, err := client.GetSummary(context.Background()); err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	var last *recordedRequest
	for i := range f.requests {
		if f.requests[i].Path == "/admin/api.php" {
			last = &f.requests[i]
		}
	}
	if last == nil || last.Query != "auth=pw&summaryRaw=" {
		t.Fatalf("query = %v, want summaryRaw+auth (URL.searchParams serialization)", f.requests)
	}
}

// versionOf reads the detected version through the concrete type.
func versionOf(client network.Client) int {
	return client.(*pihole.Integration).Version()
}

// TestV5CheckWrongKeyReturns401 pins testingAsync v5: a JSON ARRAY body (the
// wrong-key answer) is the 401 result.
func TestV5CheckWrongKeyReturns401(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "bad"}})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized || res.Error == "" {
		t.Fatalf("Check result = %+v", res)
	}
}

// TestV5EnableDisable pins the v5 mutations: GET enable / disable[=seconds]
// with the auth parameter, and the exact wire messages on failure.
func TestV5EnableDisable(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusNotFound)
		case "/admin/api.php":
			if r.URL.Query().Get("auth") != "pw" {
				f.t.Fatalf("auth = %q", r.URL.Query().Get("auth"))
			}
			if strings.HasPrefix(r.URL.RawQuery, "disable") || strings.HasPrefix(r.URL.RawQuery, "enable") {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Name: "Pi-hole", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	ctx := context.Background()
	if err := client.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	seconds := network.DisableDurationSeconds(300)
	if err := client.Disable(ctx, &seconds); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := client.Disable(ctx, nil); err != nil {
		t.Fatalf("Disable permanent: %v", err)
	}
	queries := make([]string, 0)
	for _, req := range f.requests {
		if req.Path != "/admin/api.php" {
			continue
		}
		queries = append(queries, req.Query)
	}
	want := []string{"auth=pw&enable=", "auth=pw&disable=300", "auth=pw&disable="}
	if strings.Join(queries, "|") != strings.Join(want, "|") {
		t.Fatalf("queries = %v, want %v", queries, want)
	}
}

// TestV5EnableFailureMessage pins the exact mutation error message.
func TestV5EnableFailureMessage(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Name: "Pi-hole", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	err := client.Enable(context.Background())
	if err == nil || err.Error() != "Failed to enable PiHole for Pi-hole (int-1): Internal Server Error" {
		t.Fatalf("error = %v", err)
	}
}

// TestV5StatusDroppedOnUnknown pins the v5 status enum: an unknown status
// would fail the zod enum upstream; the port drops the member.
func TestV5StatusDroppedOnUnknown(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	raw, _ := json.Marshal(summary)
	if strings.Contains(string(raw), `"status"`) {
		t.Fatalf("status must be dropped, got %s", raw)
	}
}

// --- v6 ---

// TestProbeDefaultsToV6OnTransportError pins the factory fallback: an
// unreachable probe endpoint selects v6.
func TestProbeDefaultsToV6OnTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: url})
	if v := versionOf(client); v != 6 {
		t.Fatalf("version = %d, want 6 (fallback)", v)
	}
}

// TestV6LoginAndSummary pins the v6 flow: POST /api/auth with the password
// and the Homarr User-Agent, then the summary GETs with the sid header in the
// pinned order (stats first, blocking second).
func TestV6LoginAndSummary(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			if f.requests[len(f.requests)-1].Body != `{"password":"pw"}` {
				f.t.Fatalf("login body = %q", f.requests[len(f.requests)-1].Body)
			}
			if got := r.Header.Get("User-Agent"); got != "Homarr" {
				f.t.Fatalf("User-Agent = %q", got)
			}
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"sid-1","message":null}}`))
		case "/api/stats/summary":
			if got := r.Header.Get("sid"); got != "sid-1" {
				f.t.Fatalf("sid = %q", got)
			}
			_, _ = w.Write([]byte(`{"queries":{"total":5023,"blocked":205,"percent_blocked":4.1},"gravity":{"domains_being_blocked":100001}}`))
		case "/api/dns/blocking":
			_, _ = w.Write([]byte(`{"blocking":"enabled","timer":null}`))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary.Status == nil || *summary.Status != "enabled" ||
		summary.DNSQueriesToday != 5023 || summary.AdsBlockedToday != 205 ||
		summary.AdsBlockedTodayPercentage != 4.1 || summary.DomainsBeingBlocked != 100001 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(f.requests) < 4 {
		t.Fatalf("requests = %d, want probe+login+2", len(f.requests))
	}
	// stats runs before blocking (sequential awaits).
	var statsIdx, blockingIdx int
	for i, req := range f.requests {
		switch req.Path {
		case "/api/stats/summary":
			statsIdx = i
		case "/api/dns/blocking":
			blockingIdx = i
		}
	}
	if statsIdx > blockingIdx {
		t.Fatalf("stats must run before blocking (stats %d, blocking %d)", statsIdx, blockingIdx)
	}
}

// TestV6SessionReuseAndReLogin pins withAuthAsync: the stored session is
// reused (one login for two calls) and a 401 triggers exactly one re-login.
func TestV6SessionReuseAndReLogin(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"sid-2","message":null}}`))
		case "/api/stats/summary":
			if countRequests(f, "/api/stats/summary") == 2 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"queries":{"total":1,"blocked":0,"percent_blocked":0},"gravity":{"domains_being_blocked":1}}`))
		case "/api/dns/blocking":
			_, _ = w.Write([]byte(`{"blocking":"disabled","timer":null}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	ctx := context.Background()
	if _, err := client.GetSummary(ctx); err != nil {
		t.Fatalf("GetSummary 1: %v", err)
	}
	if _, err := client.GetSummary(ctx); err != nil {
		t.Fatalf("GetSummary 2: %v", err)
	}
	logins := countRequests(f, "/api/auth")
	if logins != 2 {
		t.Fatalf("logins = %d, want 2 (first + one re-login after the 401)", logins)
	}
}

// TestV6FailedBlockingDropsStatus pins the dnsBlockingGetSchema transform:
// "failed"/"unknown" drop the status member.
func TestV6FailedBlockingDropsStatus(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"s","message":null}}`))
		case "/api/stats/summary":
			_, _ = w.Write([]byte(`{"queries":{"total":1,"blocked":0,"percent_blocked":0},"gravity":{"domains_being_blocked":1}}`))
		case "/api/dns/blocking":
			_, _ = w.Write([]byte(`{"blocking":"failed","timer":null}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	raw, _ := json.Marshal(summary)
	if strings.Contains(string(raw), `"status"`) {
		t.Fatalf("status must be dropped for failed, got %s", raw)
	}
}

// TestV6MutationsAndWireMessages pins the POST bodies and the exact
// "Response did not indicate success" wire message.
func TestV6MutationsAndWireMessages(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"s","message":null}}`))
		case "/api/dns/blocking":
			if r.Method == http.MethodPost {
				last := f.requests[len(f.requests)-1]
				if last.Header.Get("Content-Type") != "application/json" {
					f.t.Fatalf("Content-Type = %q", last.Header.Get("Content-Type"))
				}
				if last.Header.Get("sid") != "s" {
					f.t.Fatalf("sid = %q", last.Header.Get("sid"))
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`{"blocking":"enabled","timer":null}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	ctx := context.Background()
	if err := client.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	seconds := network.DisableDurationSeconds(300)
	if err := client.Disable(ctx, &seconds); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := client.Disable(ctx, nil); err != nil {
		t.Fatalf("Disable permanent: %v", err)
	}
	var bodies []string
	for _, req := range f.requests {
		if req.Path == "/api/dns/blocking" && req.Method == http.MethodPost {
			bodies = append(bodies, req.Body)
		}
	}
	want := []string{`{"blocking":true}`, `{"blocking":false,"timer":300}`, `{"blocking":false}`}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("bodies = %v, want %v (timer dropped when undefined)", bodies, want)
	}

}

// TestV6MutationFailureMessage pins the exact wire message of a failed v6
// mutation (the common ResponseError, NOT the integration taxonomy).
func TestV6MutationFailureMessage(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"s","message":null}}`))
		case "/api/dns/blocking":
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"blocking":"enabled","timer":null}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	err := client.Enable(context.Background())
	if err == nil || err.Error() != "Response did not indicate success" {
		t.Fatalf("error = %v, want the pinned ResponseError message", err)
	}
}

// TestV6InvalidPassword pins the invalid-session login failure.
func TestV6InvalidPassword(t *testing.T) {
	_, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			_, _ = w.Write([]byte(`{"session":{"valid":false,"sid":null,"message":"invalid password"}}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "bad"}})
	err := client.Enable(context.Background())
	if err == nil || err.Error() != "Response did not indicate success" {
		t.Fatalf("error = %v", err)
	}
}

// TestV6CheckClearsServerSession pins testingAsync v6: login, DELETE /api/auth
// with the sid header, success.
func TestV6CheckClearsServerSession(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			if r.Method == http.MethodDelete {
				if got := r.Header.Get("sid"); got != "sid-1" {
					f.t.Fatalf("DELETE sid = %q", got)
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`{"session":{"valid":true,"sid":"sid-1","message":null}}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL, Secrets: map[string]string{"apiKey": "pw"}})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Check result = %+v", res)
	}
	if countRequests(f, "/api/auth") < 2 {
		t.Fatal("Check must log in and clear the session")
	}
}

// TestV6Passwordless pins the optional secret: without an apiKey no session
// runs and the calls carry no sid header.
func TestV6Passwordless(t *testing.T) {
	f, srv := newFakePiHole(t, func(f *fakePiHole, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info/login":
			w.WriteHeader(http.StatusOK)
		case "/api/auth":
			f.t.Fatal("no login may happen without an apiKey")
		case "/api/stats/summary":
			if got := r.Header.Get("sid"); got != "" {
				f.t.Fatalf("sid = %q, want no header", got)
			}
			_, _ = w.Write([]byte(`{"queries":{"total":1,"blocked":0,"percent_blocked":0},"gravity":{"domains_being_blocked":1}}`))
		case "/api/dns/blocking":
			_, _ = w.Write([]byte(`{"blocking":"enabled","timer":null}`))
		}
	})
	client, _ := pihole.NewFromRef(context.Background(), network.IntegrationRef{ID: "int-1", Kind: "piHole", URL: srv.URL})
	if _, err := client.GetSummary(context.Background()); err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if countRequests(f, "/api/auth") != 0 {
		t.Fatal("passwordless pi-hole must not log in")
	}
}
