package searchengine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/searchengine"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness (style of board/harness_test.go).
func newTestAPI(t *testing.T) (*db.DB, *httptest.Server, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)

	r := trpc.New()
	searchengine.Register(r, d, searchengine.Options{})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			req = req.WithContext(ctxauth.WithSession(req.Context(), sessions[name]))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv, sessions
}

func seedUser(t *testing.T, d *db.DB, id, name string, admin bool) *ctxauth.Session {
	t.Helper()
	if err := d.CreateUser(context.Background(), db.User{ID: id, Name: name, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user %s: %v", name, err)
	}
	return &ctxauth.Session{ID: "session-" + id, UserID: id, Name: name, IsAdministrator: admin}
}

// seedGroupPermission puts users into a group carrying the given global
// permissions (no default groups are seeded: seed.ts:307-320).
func seedGroupPermission(t *testing.T, d *db.DB, userID string, permissions ...string) {
	t.Helper()
	ctx := context.Background()
	groupID := "group-perms-" + userID
	if _, err := d.ExecContext(ctx, `INSERT INTO "group" (id, name, position) VALUES (?, ?, 0)`, groupID, "perms-"+userID); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, groupID, userID); err != nil {
		t.Fatalf("seed group member: %v", err)
	}
	for _, permission := range permissions {
		if _, err := d.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`, groupID, permission); err != nil {
			t.Fatalf("seed group permission: %v", err)
		}
	}
}

// seedIntegration inserts an integration row to link engines against.
func seedIntegration(t *testing.T, d *db.DB, id, kind string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO "integration" (id, name, url, kind) VALUES (?, ?, ?, ?)`,
		id, "Integration "+id, "http://localhost:8080", kind); err != nil {
		t.Fatalf("seed integration: %v", err)
	}
}

func call(t *testing.T, srv *httptest.Server, user, path, payload string) (int, string) {
	t.Helper()
	method := http.MethodGet
	if payload != "" {
		method = http.MethodPost
	}
	var reader io.Reader
	body := ""
	if payload != "" {
		body = `{"json":` + payload + `}`
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
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

func unwrapData(t *testing.T, body string) json.RawMessage {
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
		t.Fatalf("expected success, got %s: %s", envelope.Error.JSON.Data.Code, body)
	}
	return envelope.Result.Data.JSON
}

func unwrapError(t *testing.T, body string) (string, string) {
	t.Helper()
	var envelope struct {
		Error *struct {
			JSON struct {
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error == nil {
		t.Fatalf("expected error envelope, got %s", body)
	}
	return envelope.Error.JSON.Data.Code, envelope.Error.JSON.Message
}

func TestSearchEngineCRUD(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["creator"] = seedUser(t, d, "user-creator", "creator", false)
	seedGroupPermission(t, d, "user-creator", "search-engine-create", "search-engine-modify-all", "search-engine-full-all")
	sessions["viewer"] = seedUser(t, d, "user-viewer", "viewer", false)

	// Guards: anonymous -> UNAUTHORIZED; user without the permission ->
	// FORBIDDEN ("Permission denied").
	status, body := call(t, srv, "", "/api/trpc/searchEngine.create", genericCreateInput)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d (%s)", status, body)
	}
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.create", genericCreateInput)
	if status != http.StatusForbidden {
		code, _ := unwrapError(t, body)
		t.Fatalf("unpermitted create = %d %s", status, code)
	}

	// create (generic): void output.
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.create", genericCreateInput)
	if status != http.StatusOK {
		t.Fatalf("create status = %d (%s)", status, body)
	}
	// create is a mutation without an output schema: void renders as a
	// data member without a "json" payload ({"result":{"data":{}}}).
	if got := string(unwrapData(t, body)); got != "" {
		t.Fatalf("create output = %s, want void (no json member)", got)
	}

	// The create generated the id server-side (createId, upstream
	// cuid2): read it back for the roundtrip assertions.
	var engineID string
	if err := d.QueryRow(`SELECT id FROM "search_engine" WHERE short = 'e1'`).Scan(&engineID); err != nil {
		t.Fatalf("read created engine id: %v", err)
	}

	// byId: the full row shape with nulls preserved.
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.byId", fmt.Sprintf(`{"id":%q}`, engineID))
	if status != http.StatusOK {
		t.Fatalf("byId status = %d (%s)", status, body)
	}
	if got := string(unwrapData(t, body)); !strings.Contains(got, `"name":"Engine One"`) ||
		!strings.Contains(got, `"short":"e1"`) ||
		!strings.Contains(got, `"type":"generic"`) ||
		!strings.Contains(got, `"urlTemplate":"http://example.com/?q=%s"`) ||
		!strings.Contains(got, `"description":null`) ||
		!strings.Contains(got, `"integrationId":null`) {
		t.Fatalf("byId = %s", got)
	}

	// Negative input validation: urlTemplate without %s and a too-long
	// name both reject.
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.create",
		`{"type":"generic","urlTemplate":"http://example.com/?q=","name":"Broken","short":"b1","iconUrl":"http://icon","description":null}`)
	if status != http.StatusBadRequest {
		t.Fatalf("urlTemplate without %%s = %d (%s)", status, body)
	}
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.create",
		`{"type":"generic","urlTemplate":"http://example.com/?q=%s","name":"`+strings.Repeat("x", 65)+`","short":"b2","iconUrl":"http://icon","description":null}`)
	if status != http.StatusBadRequest {
		t.Fatalf("long name = %d (%s)", status, body)
	}

	// update (modify-all): the short column is not part of the edit
	// schema; NOT_FOUND for unknown ids.
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.update",
		fmt.Sprintf(`{"id":%q,"type":"generic","urlTemplate":"http://changed.com/?q=%%s","name":"Renamed","iconUrl":"http://icon2","description":"d"}`, engineID))
	if status != http.StatusOK {
		t.Fatalf("update status = %d (%s)", status, body)
	}
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.byId", fmt.Sprintf(`{"id":%q}`, engineID))
	if !strings.Contains(string(unwrapData(t, body)), `"name":"Renamed"`) ||
		strings.Contains(string(unwrapData(t, body)), `"short":"renamed"`) {
		t.Fatalf("byId after update = %s", unwrapData(t, body))
	}
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.update",
		`{"id":"missing","type":"generic","urlTemplate":"http://x.com/?q=%s","name":"N","iconUrl":"http://i","description":null}`)
	if status != http.StatusNotFound {
		code, message := unwrapError(t, body)
		t.Fatalf("update unknown = %d %s (%s)", status, code, message)
	}

	// fromIntegration create with an integration reference.
	seedIntegration(t, d, "integration-1", "pihole")
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.create",
		`{"type":"fromIntegration","integrationId":"integration-1","name":"PiHole","short":"pi","iconUrl":"http://icon","description":null}`)
	if status != http.StatusOK {
		t.Fatalf("create fromIntegration status = %d (%s)", status, body)
	}

	// catalog: signed-in callers see everything with the integration
	// attached; anonymous callers only generic engines.
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.catalog", "")
	if status != http.StatusOK {
		t.Fatalf("catalog status = %d (%s)", status, body)
	}
	var catalog []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &catalog); err != nil {
		t.Fatalf("decode catalog: %v (%s)", err, body)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog rows = %d, want 2", len(catalog))
	}
	status, body = call(t, srv, "", "/api/trpc/searchEngine.catalog", "")
	if err := json.Unmarshal(unwrapData(t, body), &catalog); err != nil {
		t.Fatalf("decode anonymous catalog: %v", err)
	}
	if len(catalog) != 1 || catalog[0]["type"] != "generic" {
		t.Fatalf("anonymous catalog = %v, want generic only", catalog)
	}

	// search: prefix match on the lowercased short column.
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.search?input="+superjson(`{"query":"E1","limit":10}`), "")
	if status != http.StatusOK {
		t.Fatalf("search status = %d (%s)", status, body)
	}
	var results []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode search: %v (%s)", err, body)
	}
	if len(results) != 1 || results[0]["short"] != "e1" {
		t.Fatalf("search results = %v", results)
	}

	// getPaginated: protectedProcedure with the page record.
	status, body = call(t, srv, "", "/api/trpc/searchEngine.getPaginated", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous getPaginated = %d (%s)", status, body)
	}
	status, body = call(t, srv, "viewer", "/api/trpc/searchEngine.getPaginated", `{"search":"amed","pageSize":10,"page":1}`)
	if status != http.StatusOK {
		t.Fatalf("getPaginated status = %d (%s)", status, body)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		TotalCount int64            `json:"totalCount"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &page); err != nil {
		t.Fatalf("decode getPaginated: %v (%s)", err, body)
	}
	if page.TotalCount != 1 || len(page.Items) != 1 {
		t.Fatalf("getPaginated = %+v", page)
	}

	// delete (full-all): user defaults detach first.
	if _, err := d.Exec(`UPDATE "user" SET default_search_engine_id = ? WHERE id = 'user-viewer'`, engineID); err != nil {
		t.Fatalf("seed default engine: %v", err)
	}
	status, body = call(t, srv, "creator", "/api/trpc/searchEngine.delete", fmt.Sprintf(`{"id":%q}`, engineID))
	if status != http.StatusOK {
		t.Fatalf("delete status = %d (%s)", status, body)
	}
	var defaultID *string
	if err := d.QueryRow(`SELECT default_search_engine_id FROM "user" WHERE id = 'user-viewer'`).Scan(&defaultID); err != nil {
		t.Fatalf("read default engine: %v", err)
	}
	if defaultID != nil {
		t.Fatalf("default engine not cleared: %q", *defaultID)
	}
	if _, err := d.GetSearchEngineByID(context.Background(), engineID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("engine still present after delete: %v", err)
	}

	// Guards on the mutation ladder: a user with only search-engine-create
	// cannot update or delete.
	sessions["minimal"] = seedUser(t, d, "user-minimal", "minimal", false)
	seedGroupPermission(t, d, "user-minimal", "search-engine-create")
	status, body = call(t, srv, "minimal", "/api/trpc/searchEngine.update",
		`{"id":"engine-pi","type":"fromIntegration","integrationId":"integration-1","name":"X","iconUrl":"http://i","description":null}`)
	if status != http.StatusForbidden {
		t.Fatalf("minimal update = %d (%s)", status, body)
	}
	status, body = call(t, srv, "minimal", "/api/trpc/searchEngine.delete", `{"id":"engine-pi"}`)
	if status != http.StatusForbidden {
		t.Fatalf("minimal delete = %d (%s)", status, body)
	}
}

func TestGetDefaultSearchEngine(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, "user-alice", "alice", false)

	// Seed a generic engine and point the server default at it.
	ctx := context.Background()
	if _, err := d.Exec(`INSERT INTO "search_engine" (id, icon_url, name, short, type) VALUES ('engine-gen', 'http://i', 'Generic', 'gen', 'generic')`); err != nil {
		t.Fatalf("seed engine: %v", err)
	}
	if err := d.UpsertServerSetting(ctx, "search", `{"json":{"defaultSearchEngineId":"engine-gen"}}`); err != nil {
		t.Fatalf("seed search settings: %v", err)
	}

	// Anonymous caller: the server default (generic) resolves.
	status, body := call(t, srv, "", "/api/trpc/searchEngine.getDefaultSearchEngine", "")
	if status != http.StatusOK {
		t.Fatalf("getDefaultSearchEngine status = %d (%s)", status, body)
	}
	if got := string(unwrapData(t, body)); !strings.Contains(got, `"short":"gen"`) {
		t.Fatalf("server default = %s", got)
	}

	// A signed-in caller with a personal default gets it.
	if _, err := d.Exec(`UPDATE "user" SET default_search_engine_id = 'engine-gen' WHERE id = 'user-alice'`); err != nil {
		t.Fatalf("seed user default: %v", err)
	}
	status, body = call(t, srv, "alice", "/api/trpc/searchEngine.getDefaultSearchEngine", "")
	if !strings.Contains(string(unwrapData(t, body)), `"short":"gen"`) {
		t.Fatalf("user default = %s", unwrapData(t, body))
	}

	// A stale server default is cleared on read (search-engine-router.ts:
	// 122-133) once no user default is set.
	if _, err := d.Exec(`UPDATE "user" SET default_search_engine_id = NULL WHERE id = 'user-alice'`); err != nil {
		t.Fatalf("clear user default: %v", err)
	}
	if err := d.UpsertServerSetting(ctx, "search", `{"json":{"defaultSearchEngineId":"engine-gone"}}`); err != nil {
		t.Fatalf("seed stale settings: %v", err)
	}
	status, body = call(t, srv, "alice", "/api/trpc/searchEngine.getDefaultSearchEngine", "")
	if status != http.StatusOK {
		t.Fatalf("stale default status = %d (%s)", status, body)
	}
	if got := string(unwrapData(t, body)); got != "null" {
		t.Fatalf("stale default = %s, want null", got)
	}
	setting, err := d.GetServerSetting(ctx, "search")
	if err != nil {
		t.Fatalf("read search settings: %v", err)
	}
	if !strings.Contains(setting.Value, `"defaultSearchEngineId":null`) {
		t.Fatalf("stale default not cleared: %s", setting.Value)
	}
	_ = fmt.Sprint
}

func superjson(payload string) string {
	if payload == "" {
		payload = "null"
	}
	// GET inputs travel as the superjson envelope in the ?input= query
	// (http.NewRequest escapes it again).
	return url.QueryEscape(`{"json":` + payload + `}`)
}

const genericCreateInput = `{"type":"generic","urlTemplate":"http://example.com/?q=%s","name":"Engine One","short":"E1","iconUrl":"http://icon","description":null}`
