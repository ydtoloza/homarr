package home_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/home"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Test harness over httptest with header-driven session injection, following
// the board/ADMIN router conventions.

type testAPI struct {
	db       *db.DB
	srv      *httptest.Server
	sessions map[string]*ctxauth.Session
}

func newTestAPI(t *testing.T) (*testAPI, map[string]*ctxauth.Session) {
	t.Helper()
	d := db.OpenTest(t)
	r := trpc.New()
	home.Register(r, d, home.Options{})

	sessions := map[string]*ctxauth.Session{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			// Unknown names would index nil; the tests register what they use.
			if session, ok := sessions[name]; ok {
				req = req.WithContext(ctxauth.WithSession(req.Context(), session))
			}
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{db: d, srv: srv, sessions: sessions}, sessions
}

func (a *testAPI) seedUser(t *testing.T, id string, admin bool) *ctxauth.Session {
	t.Helper()
	if err := a.db.CreateUser(context.Background(), db.User{ID: id, Name: id, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	session := &ctxauth.Session{ID: "s-" + id, UserID: id, Name: id, IsAdministrator: admin}
	a.sessions[id] = session
	return session
}

func (a *testAPI) seedGroup(t *testing.T, id, name, memberUserID, permission string) {
	t.Helper()
	ctx := context.Background()
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "group" (id, name, position) VALUES (?, ?, 1)`, id, name); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, id, memberUserID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`, id, permission); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
}

func (a *testAPI) getStats(t *testing.T, name string) (int, []map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.srv.URL+"/api/trpc/home.getStats", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if name != "" {
		// The middleware would resolve the caller server-side; the test
		// injects the session through the header like the other harnesses.
		req.Header.Set("X-Test-User", name)
	}
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	// Unwrap the tRPC envelope {"result":{"data":{"json":[...]}}}.
	var envelope struct {
		Result struct {
			Data struct {
				JSON []map[string]any `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return res.StatusCode, envelope.Result.Data.JSON, string(raw)
}

func seedRow(t *testing.T, a *testAPI, query string, args ...any) {
	t.Helper()
	if _, err := a.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("seed %s: %v", query, err)
	}
}

// The full shape for an administrator: every card in the upstream order.
func TestGetStatsAdministrator(t *testing.T) {
	a, _ := newTestAPI(t)
	a.seedUser(t, "root", true)

	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b1', 'pub', 1, '#fa5252', '#fd7e14')`)
	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b2', 'priv', 0, '#fa5252', '#fd7e14')`)
	seedRow(t, a, `INSERT INTO "app" (id, name, icon_url) VALUES ('a1', 'app', 'https://icon')`)
	seedRow(t, a, `INSERT INTO "search_engine" (id, icon_url, name, short, url_template) VALUES ('se1', 'https://icon', 'engine', 'g', 'https://g/?q=%s')`)
	seedRow(t, a, `INSERT INTO "media" (id, name, content, content_type, size, creator_id) VALUES ('m1', 'img', x'00', 'image/png', 1, 'root')`)
	seedRow(t, a, `INSERT INTO "integration" (id, name, url, kind) VALUES ('i1', 'int', 'http://x', 'sabnzbd')`)

	status, stats, raw := a.getStats(t, "root")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	type card struct {
		title, subtitle, path string
		count                 float64
	}
	var got []card
	for _, s := range stats {
		got = append(got, card{
			title:    s["titleKey"].(string),
			subtitle: s["subtitleKey"].(string),
			path:     s["path"].(string),
			count:    s["count"].(float64),
		})
	}
	want := []card{
		{"board", "boards", "/manage/boards", 2},
		{"user", "authentication", "/manage/users", 1},
		{"invite", "authentication", "/manage/users/invites", 0},
		{"integration", "resources", "/manage/integrations", 1},
		{"app", "resources", "/manage/apps", 1},
		{"group", "authorization", "/manage/users/groups", 0},
		{"searchEngine", "resources", "/manage/search-engines", 1},
		{"media", "resources", "/manage/medias", 1},
	}
	if len(got) != len(want) {
		t.Fatalf("cards = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("card %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Anonymous callers: the public-boards card only.
func TestGetStatsAnonymous(t *testing.T) {
	a, _ := newTestAPI(t)
	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b1', 'pub', 1, '#fa5252', '#fd7e14')`)
	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b2', 'priv', 0, '#fa5252', '#fd7e14')`)

	status, stats, raw := a.getStats(t, "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	if len(stats) != 1 {
		t.Fatalf("cards = %s, want one", raw)
	}
	if stats[0]["titleKey"] != "board" || stats[0]["count"].(float64) != 1 {
		t.Errorf("card = %s, want public boards only", raw)
	}
}

// A signed-in user without board-view-all counts public + owned + permitted
// boards; the app card needs app-create or app-modify-all; media counts only
// the caller's rows without media-view-all.
func TestGetStatsScopedUser(t *testing.T) {
	a, _ := newTestAPI(t)
	a.seedUser(t, "alice", false)

	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color) VALUES ('b1', 'pub', 1, '#f', '#f')`)
	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color, creator_id) VALUES ('b2', 'own', 0, '#f', '#f', 'alice')`)
	seedRow(t, a, `INSERT INTO "board" (id, name, is_public, primary_color, secondary_color, creator_id) VALUES ('b3', 'other', 0, '#f', '#f', 'bob')`)

	// Alice's group carries the app-create and media-upload keys.
	a.seedGroup(t, "g1", "creators", "alice", "app-create")
	seedRow(t, a, `INSERT INTO "groupPermission" (group_id, permission) VALUES ('g1', 'media-upload')`)

	// A board explicitly permitted to the group.
	seedRow(t, a, `INSERT INTO "boardGroupPermission" (board_id, group_id, permission) VALUES ('b3', 'g1', 'view')`)

	seedRow(t, a, `INSERT INTO "media" (id, name, content, content_type, size, creator_id) VALUES ('m1', 'mine', x'00', 'image/png', 1, 'alice')`)
	seedRow(t, a, `INSERT INTO "media" (id, name, content, content_type, size, creator_id) VALUES ('m2', 'theirs', x'00', 'image/png', 1, 'bob')`)

	status, stats, raw := a.getStats(t, "alice")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	counts := map[string]float64{}
	for _, s := range stats {
		counts[s["titleKey"].(string)] = s["count"].(float64)
	}
	// board: public (b1) + owned (b2) + group-permitted (b3) = 3.
	if counts["board"] != 3 {
		t.Errorf("board count = %v (%s), want 3", counts["board"], raw)
	}
	// app: create permission shows the card with count 0 (not manage-all).
	if c, ok := counts["app"]; !ok || c != 0 {
		t.Errorf("app card = %v (%s), want present with 0", c, raw)
	}
	// media: only alice's rows.
	if counts["media"] != 1 {
		t.Errorf("media count = %v (%s), want 1", counts["media"], raw)
	}
	if strings.Contains(raw, "integration") {
		t.Errorf("integration card must be absent for alice: %s", raw)
	}
}

// Delegated full access: one integration with a group "full" permission row
// unlocks the integration card with the delegated count.
func TestGetStatsDelegatedIntegrationAccess(t *testing.T) {
	a, _ := newTestAPI(t)
	a.seedUser(t, "alice", false)
	a.seedGroup(t, "g1", "int-full", "alice", "integration-create")
	seedRow(t, a, `INSERT INTO "integrationGroupPermissions" (integration_id, group_id, permission) VALUES ('i1', 'g1', 'full')`)
	seedRow(t, a, `INSERT INTO "integration" (id, name, url, kind) VALUES ('i1', 'int', 'http://x', 'sabnzbd')`)

	status, stats, raw := a.getStats(t, "alice")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	for _, s := range stats {
		if s["titleKey"] == "integration" && s["count"].(float64) != 1 {
			t.Errorf("integration count = %s, want 1 (delegated)", raw)
		}
	}
}
