package analytics_test

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

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/analytics"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness: the analytics router against a fake PostHog /batch/ endpoint,
// sessions injected through headers.

type testAPI struct {
	db    *db.DB
	srv   *httptest.Server
	hog   *fakeHog
	clock atomic.Value // time.Time
}

type fakeHog struct {
	server   *httptest.Server
	requests atomic.Int64
	status   atomic.Int64
	lastBody atomic.Value // string
}

func (f *fakeHog) last() map[string]any {
	raw, _ := f.lastBody.Load().(string)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func newTestAPI(t *testing.T, now time.Time) *testAPI {
	t.Helper()
	d := db.OpenTest(t)

	hog := &fakeHog{}
	hog.status.Store(200)
	hog.lastBody.Store("")
	hog.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hog.requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		hog.lastBody.Store(string(raw))
		w.WriteHeader(int(hog.status.Load()))
	}))
	t.Cleanup(hog.server.Close)

	r := trpc.New()
	analytics.Register(r, d, analytics.Options{PostHogURL: hog.server.URL, Now: func() time.Time { return now }})

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test-Admin") == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u1", IsAdministrator: true}))
		} else if req.Header.Get("X-Test-User") == "1" {
			req = req.WithContext(ctxauth.WithSession(req.Context(),
				&ctxauth.Session{ID: "s", UserID: "u1"}))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{db: d, srv: srv, hog: hog}
}

func (a *testAPI) send(t *testing.T, admin bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/api/trpc/analytics.sendAnalytics", strings.NewReader(`null`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("X-Test-Admin", "1")
	}
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// seedAnalytics writes the analytics setting row.
func (a *testAPI) seedAnalytics(t *testing.T, enableGeneral bool, instanceID, lastSnapshotAt string) {
	t.Helper()
	value := admin.WrapSuperJSON(map[string]any{
		"enableGeneral":            enableGeneral,
		"instanceId":               instanceID,
		"lastSuccessfulSnapshotAt": lastSnapshotAt,
	})
	_, err := a.db.ExecContext(context.Background(),
		`INSERT INTO "serverSetting" (setting_key, value) VALUES ('analytics', ?)`, value)
	if err != nil {
		t.Fatalf("seed analytics: %v", err)
	}
}

// The disabled path: enableGeneral false answers "disabled" without a capture.
func TestSendAnalyticsDisabled(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)
	a.seedAnalytics(t, false, "inst-1", "")

	status, body := a.send(t, true)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if body != `{"result":{"data":{"json":{"status":"disabled"}}}}` {
		t.Errorf("body = %s, want disabled", body)
	}
	if n := a.hog.requests.Load(); n != 0 {
		t.Errorf("captures = %d, want 0", n)
	}
}

// The skipped path: a recent successful snapshot answers "skipped".
func TestSendAnalyticsSkipped(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)
	a.seedAnalytics(t, true, "inst-1", "2026-11-05T00:00:00.000Z")

	_, body := a.send(t, true)
	if body != `{"result":{"data":{"json":{"status":"skipped"}}}}` {
		t.Errorf("body = %s, want skipped", body)
	}
	if n := a.hog.requests.Load(); n != 0 {
		t.Errorf("captures = %d, want 0", n)
	}
}

// The sent path: due snapshot captured, success timestamp advanced, event
// shape correct.
func TestSendAnalyticsSent(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)
	a.seedAnalytics(t, true, "inst-1", "")
	a.db.ExecContext(context.Background(), `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b1', 'b', 1, '#f', '#f')`)

	_, body := a.send(t, true)
	if body != `{"result":{"data":{"json":{"status":"sent"}}}}` {
		t.Fatalf("body = %s, want sent", body)
	}
	if n := a.hog.requests.Load(); n != 1 {
		t.Fatalf("captures = %d, want 1", n)
	}
	hog := a.hog.last()
	if hog["api_key"] == "" {
		t.Errorf("api_key missing: %v", hog)
	}
	batch, _ := hog["batch"].([]any)
	if len(batch) != 1 {
		t.Fatalf("batch = %v", hog["batch"])
	}
	event := batch[0].(map[string]any)
	if event["event"] != "server-analytics" || event["distinct_id"] != "inst-1" {
		t.Errorf("event = %v", event)
	}
	props := event["properties"].(map[string]any)
	if props["homarrVersion"] != "2.11.1" || props["databaseType"] != "sqlite" {
		t.Errorf("properties = %v", props)
	}
	if props["countBoards"] != float64(1) {
		t.Errorf("countBoards = %v, want 1", props["countBoards"])
	}
	if _, ok := props["$geoip_disable"]; !ok {
		t.Errorf("$geoip_disable missing: %v", props)
	}

	// The success stamp: lastSuccessfulSnapshotAt = now, so the immediate
	// retry is "skipped".
	_, body = a.send(t, true)
	if body != `{"result":{"data":{"json":{"status":"skipped"}}}}` {
		t.Errorf("retry body = %s, want skipped", body)
	}

	// The stamp is visible in the row (superjson wrapped).
	row, err := a.db.GetServerSetting(context.Background(), "analytics")
	if err != nil {
		t.Fatalf("read analytics row: %v", err)
	}
	if !strings.Contains(row.Value, "2026-11-07T12:00:00.000Z") {
		t.Errorf("row = %s, want the success stamp", row.Value)
	}
}

// The failed path: a PostHog outage degrades to "failed" (the upstream catch)
// and the success timestamp does not advance.
func TestSendAnalyticsFailed(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)
	a.seedAnalytics(t, true, "inst-1", "")
	a.hog.status.Store(500)

	_, body := a.send(t, true)
	if body != `{"result":{"data":{"json":{"status":"failed"}}}}` {
		t.Errorf("body = %s, want failed", body)
	}
	row, err := a.db.GetServerSetting(context.Background(), "analytics")
	if err != nil {
		t.Fatalf("read analytics row: %v", err)
	}
	if strings.Contains(row.Value, "2026-11-07T12:00:00.000Z") {
		t.Errorf("row = %s, the failure must not stamp success", row.Value)
	}
}

// A missing analytics setting row fails the CAS update ("Analytics server
// settings are missing" upstream) — the status degrades to "failed".
func TestSendAnalyticsMissingRow(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)

	_, body := a.send(t, true)
	if body != `{"result":{"data":{"json":{"status":"failed"}}}}` {
		t.Errorf("body = %s, want failed", body)
	}
}

// Gates: the mutation is admin-only and wrapped with the read-only guard.
func TestSendAnalyticsGates(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := newTestAPI(t, now)

	status, body := a.send(t, false)
	if status != http.StatusUnauthorized || !strings.Contains(body, "UNAUTHORIZED") {
		t.Errorf("anonymous = %s (status %d), want UNAUTHORIZED", body, status)
	}
}

// The uuid derivation: version-5-shaped, deterministic per instance+period
// (getSnapshotUuid).
func TestSnapshotUUID(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	u1 := analytics.SnapshotUUID("instance-a", now)
	u2 := analytics.SnapshotUUID("instance-a", now)
	u3 := analytics.SnapshotUUID("instance-b", now)
	if u1 != u2 {
		t.Errorf("uuid not deterministic: %s vs %s", u1, u2)
	}
	if u1 == u3 {
		t.Error("uuid identical across instances")
	}
	if len(u1) != 36 || u1[14] != '5' || u1[19] != 'a' {
		t.Errorf("uuid shape = %s, want the 8-4-5a-… v5 shape", u1)
	}
}
