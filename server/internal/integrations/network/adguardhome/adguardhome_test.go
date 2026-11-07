package adguardhome_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
	"github.com/ydtoloza/homarr/server/internal/integrations/network/adguardhome"
)

// fakeAdguard records every request and answers per path.
type fakeAdguard struct {
	t    *testing.T
	mu   sync.Mutex
	seen map[string]int
	last struct {
		path   string
		header http.Header
		body   string
	}
}

func newFakeAdguard(t *testing.T, handler func(f *fakeAdguard, w http.ResponseWriter, r *http.Request)) (*fakeAdguard, *httptest.Server) {
	t.Helper()
	f := &fakeAdguard{t: t, seen: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			body = string(raw[:n])
		}
		f.mu.Lock()
		f.seen[r.URL.Path]++
		f.last.path = r.URL.Path
		f.last.header = r.Header.Clone()
		f.last.body = body
		f.mu.Unlock()
		handler(f, w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-1", Kind: "adGuardHome", Name: "AdGuard Home", URL: url, ExternalURL: ""}
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestNewFromRefMissingSecret(t *testing.T) {
	if _, err := adguardhome.NewFromRef(network.IntegrationRef{ID: "int-1", Kind: "adGuardHome"}); err == nil ||
		err.Error() != "No secret of kind username was found" {
		t.Fatalf("error = %v", err)
	}
}

// TestCheckOnline pins testingAsync: GET /control/status with Basic auth.
func TestCheckOnline(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control/status" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != basicAuth("admin", "pass") {
			f.t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"protection_enabled":true}`))
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("Check result = %+v", res)
	}
}

func TestCheckNonObjectBodyIsParseResult(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[1,2]`))
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != mediaserverMsgParse() {
		t.Fatalf("error = %q, want the pinned parse message", res.Error)
	}
}

func TestCheckBadStatusIsDefinitive(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized || res.Error != mediaserverMsgResponse() {
		t.Fatalf("Check result = %+v", res)
	}
}

// TestSummaryDays pins the time_units "days" math (last entry), the enabled
// filters sum and the percentage, over the THREE parallel GETs.
func TestSummaryDays(t *testing.T) {
	f, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/stats":
			_, _ = w.Write([]byte(`{"time_units":"days","dns_queries":[100,200,300],"blocked_filtering":[10,20,50],"top_queried_domains":[],"top_clients":[],"num_dns_queries":600}`))
		case "/control/status":
			_, _ = w.Write([]byte(`{"protection_enabled":true,"version":"v0.107"}`))
		case "/control/filtering/status":
			_, _ = w.Write([]byte(`{"filters":[{"url":"a","name":"A","id":1,"rules_count":1000,"enabled":true},{"url":"b","name":"B","id":2,"rules_count":500,"enabled":false},{"url":"c","name":"C","id":3,"rules_count":250,"enabled":true}]}`))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary.Status == nil || *summary.Status != "enabled" {
		t.Fatalf("status = %v", summary.Status)
	}
	if summary.DNSQueriesToday != 300 || summary.AdsBlockedToday != 50 {
		t.Fatalf("today values = %+v (days use the LAST entry)", summary)
	}
	if summary.DomainsBeingBlocked != 1250 {
		t.Fatalf("domains = %v (only ENABLED filters count)", summary.DomainsBeingBlocked)
	}
	// Runtime float math (like JS): (blocked/queries)*100.
	blocked, queries := 50.0, 300.0
	if got := (blocked / queries) * 100; summary.AdsBlockedTodayPercentage != got {
		t.Fatalf("percentage = %v, want %v", summary.AdsBlockedTodayPercentage, got)
	}
	for _, path := range []string{"/control/stats", "/control/status", "/control/filtering/status"} {
		if f.seen[path] != 1 {
			t.Fatalf("%s calls = %d, want 1", path, f.seen[path])
		}
	}
}

// TestSummaryHours pins the time_units "hours" math (SUM) and the null
// filters list.
func TestSummaryHours(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/stats":
			_, _ = w.Write([]byte(`{"time_units":"hours","dns_queries":[100,200],"blocked_filtering":[10,20]}`))
		case "/control/status":
			_, _ = w.Write([]byte(`{"protection_enabled":false}`))
		case "/control/filtering/status":
			_, _ = w.Write([]byte(`{"filters":null}`))
		}
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary.DNSQueriesToday != 300 || summary.AdsBlockedToday != 30 || summary.DomainsBeingBlocked != 0 {
		t.Fatalf("summary = %+v (hours are summed, null filters contribute none)", summary)
	}
	if summary.Status == nil || *summary.Status != "disabled" {
		t.Fatalf("status = %v", summary.Status)
	}
	if summary.AdsBlockedTodayPercentage != 10 {
		t.Fatalf("percentage = %v", summary.AdsBlockedTodayPercentage)
	}
}

// TestSummaryZeroQueries pins the percentage guard.
func TestSummaryZeroQueries(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/stats":
			_, _ = w.Write([]byte(`{"time_units":"days","dns_queries":[],"blocked_filtering":[]}`))
		case "/control/status":
			_, _ = w.Write([]byte(`{}`))
		case "/control/filtering/status":
			_, _ = w.Write([]byte(`{"filters":[]}`))
		}
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	summary, err := client.GetSummary(context.Background())
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if summary.AdsBlockedTodayPercentage != 0 || summary.DNSQueriesToday != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

// TestSummaryFetchFailureMessage pins the per-resource fetch message.
func TestSummaryFetchFailureMessage(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/control/stats" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	_, err := client.GetSummary(context.Background())
	if err == nil || err.Error() != "Failed to fetch stats for AdGuard Home (int-1): Bad Gateway" {
		t.Fatalf("error = %v", err)
	}
}

// TestEnableDisable pins the POST bodies (duration seconds -> milliseconds)
// and the exact wire messages.
func TestEnableDisable(t *testing.T) {
	f, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control/protection" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != basicAuth("admin", "pass") {
			f.t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			f.t.Fatalf("Content-Type = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last.body != `{"enabled":false,"duration":0}` {
		t.Fatalf("last body = %q (nil duration = 0 seconds)", f.last.body)
	}
}

// TestEnableFailureMessage pins the exact mutation error message.
func TestEnableFailureMessage(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	err := client.Enable(context.Background())
	if err == nil || err.Error() != "Failed to enable AdGuard Home for AdGuard Home (int-1): Internal Server Error" {
		t.Fatalf("error = %v", err)
	}
}

// TestDisableFailureMessage pins the disable variant.
func TestDisableFailureMessage(t *testing.T) {
	_, srv := newFakeAdguard(t, func(f *fakeAdguard, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	client := adguardhome.New(testInput(srv.URL), adguardhome.Secrets{Username: "admin", Password: "pass"})
	err := client.Disable(context.Background(), nil)
	if err == nil || err.Error() != "Failed to disable AdGuard Home for AdGuard Home (int-1): Forbidden" {
		t.Fatalf("error = %v", err)
	}
}

// TestJSONEncoding sanity: the summary marshals with the upstream member
// names.
func TestJSONEncoding(t *testing.T) {
	summary := network.DnsHoleSummary{
		Status:                    network.Ptr("enabled"),
		DomainsBeingBlocked:       100001,
		AdsBlockedToday:           205,
		AdsBlockedTodayPercentage: 4.1,
		DNSQueriesToday:           5023,
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"status":"enabled","domainsBeingBlocked":100001,"adsBlockedToday":205,"adsBlockedTodayPercentage":4.1,"dnsQueriesToday":5023}`
	if string(raw) != want {
		t.Fatalf("json = %s, want %s", raw, want)
	}
	// A nil status drops the member (v6 failed/unknown).
	summary.Status = nil
	raw, _ = json.Marshal(summary)
	if strings.Contains(string(raw), `"status"`) {
		t.Fatalf("status must be dropped, got %s", raw)
	}
}

// mediaserverMsgParse/mediaserverMsgResponse keep the pinned taxonomy
// messages local to the test binary.
func mediaserverMsgParse() string    { return "Failed to parse integration data" }
func mediaserverMsgResponse() string { return "Response from integration did not indicate success" }
