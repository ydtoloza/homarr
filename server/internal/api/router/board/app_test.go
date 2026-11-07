package board_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// appRouter tests: permission gates, output shapes and the anonymous access
// control of AppAccessControl.

// seedApp creates an app through the API as root (who carries the fork
// administrator flag, expanding every permission).
func seedApp(t *testing.T, srv *httptest.Server, name, iconURL, href string) string {
	t.Helper()
	input := fmt.Sprintf(`{"name":%q,"description":"","iconUrl":%q,"href":%q,"pingUrl":""}`, name, iconURL, href)
	status, body := call(t, srv, rootName, http.MethodPost, "/api/trpc/app.create", superjsonEnvelope(input))
	if status != http.StatusOK {
		t.Fatalf("app.create status %d body %s", status, body)
	}
	var out struct {
		AppID string `json:"appId"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode app.create: %v (%s)", err, body)
	}
	if out.AppID == "" || out.AppID != out.ID {
		t.Fatalf("app.create output = %+v", out)
	}
	return out.AppID
}

func TestAppRouter(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	root := seedUser(t, d, testUser{id: "user-root", name: rootName, admin: true})
	sessions[aliceName], sessions[bobName], sessions[rootName] = alice, bob, root

	// Permission gates.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/app.create",
		superjsonEnvelope(`{"name":"X","description":"","iconUrl":"i","href":"","pingUrl":""}`), "UNAUTHORIZED")
	// Alice has no groups: no app-create.
	expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/app.create",
		superjsonEnvelope(`{"name":"X","description":"","iconUrl":"i","href":"","pingUrl":""}`), "FORBIDDEN")
	expectError(t, srv, aliceName, http.MethodGet, "/api/trpc/app.all", "", "FORBIDDEN")
	expectError(t, srv, aliceName, http.MethodGet, "/api/trpc/app.getPaginated", "", "FORBIDDEN")

	appID := seedApp(t, srv, "Sonarr", "https://example.com/sonarr.png", "https://sonarr.example.com")

	// The output carries the full selectAppSchema plus the legacy appId.
	status, body := call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, appID)), "")
	if status != http.StatusOK {
		t.Fatalf("app.byId status %d body %s", status, body)
	}
	app := unwrapData(t, body)
	var out map[string]any
	if err := json.Unmarshal(app, &out); err != nil {
		t.Fatalf("decode app: %v", err)
	}
	if got := sortedKeys(out); fmt.Sprint(got) != fmt.Sprint([]string{"description", "href", "iconUrl", "id", "name", "pingUrl"}) {
		t.Fatalf("app.byId keys = %v", got)
	}
	if out["name"] != "Sonarr" || out["description"] != nil || out["href"] != "https://sonarr.example.com" || out["pingUrl"] != nil {
		t.Fatalf("app.byId = %+v", out)
	}

	// byIds with a plain array input.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/app.byIds`+encodedInput(fmt.Sprintf(`[%q]`, appID)), "")
	if status != http.StatusOK {
		t.Fatalf("app.byIds status %d body %s", status, body)
	}
	var apps []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &apps); err != nil || len(apps) != 1 {
		t.Fatalf("app.byIds = %s", body)
	}

	// Anonymous callers only see apps placed on public boards. The app is
	// linked through an "app" item on a public board.
	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "AppsBoard", 10, true)
	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID),
			fmt.Sprintf(`[{"id":"item-app-1","kind":"app","options":{"appId":%q},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":1,"height":1}],"integrationIds":[]}]`,
				appID, baseLayoutID, rootSectionID))))
	if status != http.StatusOK {
		t.Fatalf("saveBoard app item status %d body %s", status, body)
	}

	otherID := seedApp(t, srv, "Private App", "https://example.com/x.png", "")
	status, body = call(t, srv, "", http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, appID)), "")
	if status != http.StatusOK {
		t.Fatalf("anonymous byId status %d body %s", status, body)
	}
	// The private app is invisible for anonymous callers (empty result set ->
	// NOT_FOUND via repository).
	expectError(t, srv, "", http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, otherID)), "", "NOT_FOUND")
	// Signed-in callers see every app.
	status, body = call(t, srv, bobName, http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, otherID)), "")
	if status != http.StatusOK {
		t.Fatalf("signed-in byId status %d body %s", status, body)
	}

	// app.search: app-modify-all OR board-modify-all, otherwise FORBIDDEN.
	expectError(t, srv, bobName, http.MethodGet,
		`/api/trpc/app.search`+encodedInput(`{"query":"son"}`), "", "FORBIDDEN")
	seedGroup(t, d, "group-managers", "managers", []string{"user-bob"}, []string{"board-modify-all"})
	status, body = call(t, srv, bobName, http.MethodGet,
		`/api/trpc/app.search`+encodedInput(`{"query":"son"}`), "")
	if status != http.StatusOK {
		t.Fatalf("search status %d body %s", status, body)
	}
	var found []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &found); err != nil || len(found) != 1 {
		t.Fatalf("search = %s", body)
	}

	// app.getPaginated with search + pagination.
	status, body = call(t, srv, rootName, http.MethodGet,
		`/api/trpc/app.getPaginated`+encodedInput(`{"search":"son","pageSize":10,"page":1}`), "")
	if status != http.StatusOK {
		t.Fatalf("getPaginated status %d body %s", status, body)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		TotalCount int              `json:"totalCount"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &page); err != nil {
		t.Fatalf("decode page: %v (%s)", err, body)
	}
	if page.TotalCount != 1 || len(page.Items) != 1 || page.Items[0]["name"] != "Sonarr" {
		t.Fatalf("page = %+v", page)
	}

	// app.selectable for signed-in callers.
	status, body = call(t, srv, bobName, http.MethodGet, "/api/trpc/app.selectable", "")
	if status != http.StatusOK {
		t.Fatalf("selectable status %d body %s", status, body)
	}
	if err := json.Unmarshal(unwrapData(t, body), &apps); err != nil || len(apps) != 2 {
		t.Fatalf("selectable = %s", body)
	}

	// app.update renames and clears the href; missing apps are NOT_FOUND.
	status, body = call(t, srv, rootName, http.MethodPost, "/api/trpc/app.update",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Sonarr v2","description":"downloads","iconUrl":"https://example.com/sonarr.png","href":"","pingUrl":""}`, appID)))
	if status != http.StatusOK {
		t.Fatalf("app.update status %d body %s", status, body)
	}
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, appID)), "")
	var updated map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated["name"] != "Sonarr v2" || updated["href"] != nil || updated["description"] != "downloads" {
		t.Fatalf("updated app = %+v", updated)
	}
	expectError(t, srv, rootName, http.MethodPost, "/api/trpc/app.update",
		superjsonEnvelope(`{"id":"missing","name":"X","description":"","iconUrl":"i","href":"","pingUrl":""}`), "NOT_FOUND")

	// app.delete requires app-full-all and removes the row.
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/app.delete",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q}`, appID)), "FORBIDDEN")
	status, body = call(t, srv, rootName, http.MethodPost, "/api/trpc/app.delete",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q}`, appID)))
	if status != http.StatusOK {
		t.Fatalf("app.delete status %d body %s", status, body)
	}
	expectError(t, srv, aliceName, http.MethodGet,
		`/api/trpc/app.byId`+encodedInput(fmt.Sprintf(`{"id":%q}`, appID)), "", "NOT_FOUND")

	// app.createMany inserts several apps (icon fallback when null).
	status, body = call(t, srv, rootName, http.MethodPost, "/api/trpc/app.createMany",
		superjsonEnvelope(`[{"name":"First","description":null,"iconUrl":null,"href":null},{"name":"Second","description":null,"iconUrl":"https://example.com/2.png","href":null}]`))
	if status != http.StatusOK {
		t.Fatalf("app.createMany status %d body %s", status, body)
	}
	status, body = call(t, srv, rootName, http.MethodGet, "/api/trpc/app.all", "")
	if err := json.Unmarshal(unwrapData(t, body), &apps); err != nil {
		t.Fatalf("decode app.all: %v (%s)", err, body)
	}
	if len(apps) != 3 { // Private App + First + Second
		t.Fatalf("app.all length = %d, want 3 (%s)", len(apps), body)
	}
	for _, a := range apps {
		if a["name"] == "First" && a["iconUrl"] == nil {
			t.Fatalf("createMany icon fallback missing: %+v", a)
		}
	}
}
