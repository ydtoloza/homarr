package board_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// Board-domain tests: visibility matrix, JSON shapes, mutation guards,
// section/item CRUD and the layout persistence roundtrip. Every case runs
// through a real trpc.Router over httptest, so the tRPC envelopes are part of
// the assertions.

// TestBoardVisibilityMatrix exercises the upstream visibility filter: public
// boards, creator boards, direct user permissions, group permissions and the
// administrator bypass (board.getAllBoards / getAccessibleBoardsWhere,
// packages/api/src/router/board.ts:2932-2939).
func TestBoardVisibilityMatrix(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	carol := seedUser(t, d, testUser{id: "user-carol", name: carolName})
	admin := seedUser(t, d, testUser{id: "user-root", name: rootName, admin: true})
	sessions[aliceName], sessions[bobName], sessions[carolName], sessions[rootName] = alice, bob, carol, admin

	grantBoardCreate(t, d, "user-alice")
	aliceBoard, _ := mustCreateBoard(t, srv, aliceName, "AliceBoard", 10, false)
	_, _ = mustCreateBoard(t, srv, aliceName, "SharedBoard", 10, false)
	if _, err := d.Exec(`UPDATE "board" SET is_public = 1 WHERE name = 'SharedBoard'`); err != nil {
		t.Fatalf("make board public: %v", err)
	}

	// Carol sees Alice's private board through a direct user permission;
	// Bob through his group membership.
	seedBoardUserPermission(t, d, aliceBoard, "user-carol", "view")
	seedGroup(t, d, "group-editors", "editors", []string{"user-bob"}, nil)
	seedBoardGroupPermission(t, d, aliceBoard, "group-editors", "modify")

	boardNames := func(user string) []string {
		status, body := call(t, srv, user, http.MethodGet, "/api/trpc/board.getAllBoards", "")
		if status != http.StatusOK {
			t.Fatalf("getAllBoards as %q: status %d body %s", user, status, body)
		}
		var boards []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			IsPublic bool   `json:"isPublic"`
		}
		if err := json.Unmarshal(unwrapData(t, body), &boards); err != nil {
			t.Fatalf("decode getAllBoards as %q: %v (%s)", user, err, body)
		}
		names := []string{}
		for _, b := range boards {
			names = append(names, b.Name)
		}
		sort.Strings(names)
		return names
	}

	assertNames := func(got []string, want ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("board list = %v, want %v", got, want)
		}
	}

	assertNames(boardNames(aliceName), "AliceBoard", "SharedBoard") // creator sees own private boards
	assertNames(boardNames(bobName), "AliceBoard", "SharedBoard")   // via group permission
	assertNames(boardNames(carolName), "AliceBoard", "SharedBoard") // via direct user permission
	assertNames(boardNames(""), "SharedBoard")                      // anonymous: public only
	assertNames(boardNames(rootName), "AliceBoard", "SharedBoard")  // administrator bypass

	// catalog mirrors the same filter (board.catalog).
	status, body := call(t, srv, carolName, http.MethodGet, "/api/trpc/board.catalog", "")
	if status != http.StatusOK {
		t.Fatalf("catalog status %d body %s", status, body)
	}
	var catalog []struct {
		Name        string `json:"name"`
		Permissions struct {
			HasViewAccess   bool `json:"hasViewAccess"`
			HasChangeAccess bool `json:"hasChangeAccess"`
			HasFullAccess   bool `json:"hasFullAccess"`
		} `json:"permissions"`
		IsHome bool `json:"isHome"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &catalog); err != nil {
		t.Fatalf("decode catalog: %v (%s)", err, body)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog length = %d, want 2 (%s)", len(catalog), body)
	}
	for _, entry := range catalog {
		if entry.Name != "AliceBoard" {
			continue
		}
		// Carol has the view permission: view yes, change/full no.
		if !entry.Permissions.HasViewAccess || entry.Permissions.HasChangeAccess || entry.Permissions.HasFullAccess {
			t.Fatalf("carol permissions on Alice Board: %+v", entry.Permissions)
		}
	}

	// board.search finds by name substring.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.search`+encodedInput(`{"query":"alice","limit":5}`), "")
	if status != http.StatusOK {
		t.Fatalf("search status %d body %s", status, body)
	}
	var results []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode search: %v (%s)", err, body)
	}
	if len(results) != 1 || results[0].Name != "AliceBoard" {
		t.Fatalf("search results = %+v", results)
	}
}

// TestCreateBoardShapeAndGuards checks the createBoard output shape, the
// seeded Mobile/Base layouts and the permission gates.
func TestCreateBoardShapeAndGuards(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	// Anonymous callers are rejected by the protectedProcedure gate.
	message := expectError(t, srv, "", http.MethodPost, "/api/trpc/board.createBoard",
		superjsonEnvelope(`{"name":"Nope","columnCount":10,"isPublic":false}`), "UNAUTHORIZED")
	if message != "UNAUTHORIZED" {
		t.Fatalf("unauthenticated message = %q (upstream falls back to the code string)", message)
	}

	// A signed-in user without board-create gets FORBIDDEN "Permission denied"
	// (permissionRequiredProcedure).
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.createBoard",
		superjsonEnvelope(`{"name":"Nope","columnCount":10,"isPublic":false}`), "FORBIDDEN")

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "MyBoard", 12, true)

	// The board appears in the list with the exact boardSummarySchema keys
	// and the creator's home board flag.
	status, body := call(t, srv, aliceName, http.MethodGet, "/api/trpc/board.getAllBoards", "")
	if status != http.StatusOK {
		t.Fatalf("getAllBoards status %d body %s", status, body)
	}
	var boards []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &boards); err != nil {
		t.Fatalf("decode boards: %v (%s)", err, body)
	}
	if len(boards) != 1 {
		t.Fatalf("boards length = %d, want 1 (%s)", len(boards), body)
	}
	entry := boards[0]
	wantKeys := []string{"creator", "groupPermissions", "id", "isHome", "isMobileHome", "isPublic",
		"logoImageUrl", "name", "userPermissions"}
	if got := sortedKeys(entry); fmt.Sprint(got) != fmt.Sprint(wantKeys) {
		t.Fatalf("getAllBoards keys = %v, want %v", got, wantKeys)
	}
	if entry["id"] != boardID || entry["isHome"] != true || entry["isMobileHome"] != false ||
		entry["isPublic"] != true || entry["logoImageUrl"] != nil {
		t.Fatalf("getAllBoards entry = %+v", entry)
	}
	creator, ok := entry["creator"].(map[string]any)
	if !ok || creator["id"] != "user-alice" || creator["name"] != "alice" || creator["image"] != nil {
		t.Fatalf("creator = %+v", entry["creator"])
	}
	// The summary permission rows strip to {permission} entries.
	userPerms, ok := entry["userPermissions"].([]any)
	if !ok || len(userPerms) != 0 {
		t.Fatalf("userPermissions = %+v, want []", entry["userPermissions"])
	}
	groupPerms, ok := entry["groupPermissions"].([]any)
	if !ok || len(groupPerms) != 0 {
		t.Fatalf("groupPermissions = %+v, want []", entry["groupPermissions"])
	}

	// getBoardByName: the full board shape with the seeded layouts and the
	// root section.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardByName`+encodedInput(`{"name":"MyBoard"}`), "")
	if status != http.StatusOK {
		t.Fatalf("getBoardByName status %d body %s", status, body)
	}
	full := unwrapData(t, body)
	var boardOut map[string]any
	if err := json.Unmarshal(full, &boardOut); err != nil {
		t.Fatalf("decode full board: %v", err)
	}
	wantFullKeys := []string{"backgroundImageAttachment", "backgroundImageRepeat", "backgroundImageSize",
		"backgroundImageUrl", "creator", "creatorId", "customCss", "disableStatus", "faviconImageUrl",
		"groupPermissions", "iconColor", "id", "isPublic", "itemRadius", "items", "layouts", "logoImageUrl",
		"metaTitle", "name", "opacity", "pageTitle", "primaryColor", "secondaryColor", "sections",
		"userPermissions"}
	if got := sortedKeys(boardOut); fmt.Sprint(got) != fmt.Sprint(wantFullKeys) {
		t.Fatalf("full board keys = %v\nwant            %v", got, wantFullKeys)
	}
	layouts, ok := boardOut["layouts"].([]any)
	if !ok || len(layouts) != 2 {
		t.Fatalf("layouts = %+v, want the Mobile + Base pair", boardOut["layouts"])
	}
	first := layouts[0].(map[string]any)
	second := layouts[1].(map[string]any)
	if first["role"] != "mobile" || first["breakpoint"] != float64(0) || first["columnCount"] != float64(3) {
		t.Fatalf("mobile layout = %+v", first)
	}
	if second["role"] != "base" || second["breakpoint"] != float64(768) || second["columnCount"] != float64(12) ||
		second["id"] != baseLayoutID {
		t.Fatalf("base layout = %+v", second)
	}
	sections, ok := boardOut["sections"].([]any)
	if !ok || len(sections) != 1 {
		t.Fatalf("sections = %+v, want one root section", boardOut["sections"])
	}
	section := sections[0].(map[string]any)
	if got := sortedKeys(section); fmt.Sprint(got) != fmt.Sprint([]string{"id", "kind", "xOffset", "yOffset"}) {
		t.Fatalf("empty section keys = %v", got)
	}
	if section["kind"] != "empty" || section["xOffset"] != float64(0) || section["yOffset"] != float64(0) {
		t.Fatalf("root section = %+v", section)
	}
	items, ok := boardOut["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %+v, want []", boardOut["items"])
	}

	// Invalid names are rejected with BAD_REQUEST (boardNameSchema).
	expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.createBoard",
		superjsonEnvelope(`{"name":"invalid name!","columnCount":10,"isPublic":false}`), "BAD_REQUEST")
}

// TestBoardMutationGuards mirrors throwIfActionForbiddenAsync: callers
// without the required board permission get NOT_FOUND (anti-snooping), and
// the name-conflict check returns CONFLICT.
func TestBoardMutationGuards(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	carol := seedUser(t, d, testUser{id: "user-carol", name: carolName})
	sessions[aliceName], sessions[bobName], sessions[carolName] = alice, bob, carol

	grantBoardCreate(t, d, "user-alice")
	boardID, _ := mustCreateBoard(t, srv, aliceName, "Owned", 10, false)

	// Anonymous callers on a protected mutation: UNAUTHORIZED.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Renamed"}`, boardID)), "UNAUTHORIZED")

	// Bob has no relation to the board: NOT_FOUND "Board not found" — the
	// upstream anti-snooping behavior of board-access.ts notAllowed().
	message := expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Renamed"}`, boardID)), "NOT_FOUND")
	if message != "Board not found" {
		t.Fatalf("rename forbidden message = %q", message)
	}

	// A view permission is not enough for rename (full required).
	seedBoardUserPermission(t, d, boardID, "user-bob", "view")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Renamed"}`, boardID)), "NOT_FOUND")

	// Modify is not enough either.
	seedBoardUserPermission(t, d, boardID, "user-bob", "modify")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Renamed"}`, boardID)), "NOT_FOUND")

	// Carol with full permission may rename — but conflicts with the name of
	// the board Alice owns (CONFLICT).
	_, _ = mustCreateBoard(t, srv, aliceName, "Other", 10, false)
	seedBoardUserPermission(t, d, boardID, "user-carol", "full")
	message = expectError(t, srv, carolName, http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Other"}`, boardID)), "CONFLICT")
	if message != "Board with similar name already exists" {
		t.Fatalf("conflict message = %q", message)
	}

	// Alice (owner) renames and changes visibility; getPublicBoards reflects
	// the visibility flip.
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.renameBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Owned2"}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("rename status %d body %s", status, body)
	}
	// z.void() outputs serialize as {"result":{"data":{}}} on the wire
	// (superjson of undefined), so the inner "json" member is absent.
	if data := strings.TrimSpace(string(unwrapData(t, body))); data != "" {
		t.Fatalf("rename output = %q, want empty (z.void() -> data {})", data)
	}

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.changeBoardVisibility",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"visibility":"public"}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("visibility status %d body %s", status, body)
	}
	status, body = call(t, srv, "", http.MethodGet, "/api/trpc/board.getPublicBoards", "")
	var public []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &public); err != nil {
		t.Fatalf("decode public boards: %v", err)
	}
	if status != http.StatusOK || len(public) != 1 || public[0]["name"] != "Owned2" {
		t.Fatalf("public boards = %+v status %d", public, status)
	}

	// The instance home board cannot be made private.
	if _, err := d.Exec(`INSERT INTO "serverSetting" (setting_key, value) VALUES ('board', ?)`,
		fmt.Sprintf(`{"json":{"homeBoardId":%q}}`, boardID)); err != nil {
		t.Fatalf("seed server settings: %v", err)
	}
	message = expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.changeBoardVisibility",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"visibility":"private"}`, boardID)), "BAD_REQUEST")
	if message != "Cannot make home board private" {
		t.Fatalf("home board visibility message = %q", message)
	}

	// setHomeBoard only needs view access: bob holds a view permission, so
	// the mutation succeeds for him (the no-access negative case is already
	// covered by the rename checks above).
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.setHomeBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("setHomeBoard status %d", status)
	}
	var homeBoardID string
	if err := d.QueryRow(`SELECT home_board_id FROM "user" WHERE id = 'user-alice'`).Scan(&homeBoardID); err != nil {
		t.Fatalf("read home board: %v", err)
	}
	if homeBoardID != boardID {
		t.Fatalf("home board = %q, want %q", homeBoardID, boardID)
	}

	// getHomeBoard returns the user's selection.
	status, body = call(t, srv, aliceName, http.MethodGet, "/api/trpc/board.getHomeBoard", "")
	if status != http.StatusOK {
		t.Fatalf("getHomeBoard status %d body %s", status, body)
	}
	var homeBoard struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &homeBoard); err != nil || homeBoard.ID != boardID {
		t.Fatalf("getHomeBoard = %s", body)
	}

	// board.exists: boolean response with the board-create permission.
	status, body = call(t, srv, aliceName, http.MethodGet, "/api/trpc/board.exists"+encodedInput(`"Owned2"`), "")
	if status != http.StatusOK || string(unwrapData(t, body)) != "true" {
		t.Fatalf("exists = %s (%d)", body, status)
	}
	// Anonymous callers lack board-create: permissionRequiredProcedure
	// extends protectedProcedure upstream, so the gate is UNAUTHORIZED.
	expectError(t, srv, "", http.MethodGet, "/api/trpc/board.exists"+encodedInput(`"Owned2"`), "", "UNAUTHORIZED")

	// deleteBoard: the owner deletes and the board disappears.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.deleteBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("deleteBoard status %d body %s", status, body)
	}
	expectError(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardByName`+encodedInput(`{"name":"Owned2"}`), "", "NOT_FOUND")
}

// TestBoardSettingsRoundtrip covers getBoardSettings / savePartialBoardSettings
// with the ""-for-null rendering of the settings schema.
func TestBoardSettingsRoundtrip(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	grantBoardCreate(t, d, "user-alice")
	boardID, _ := mustCreateBoard(t, srv, aliceName, "SettingsBoard", 10, false)

	// Bob (no access) is rejected with NOT_FOUND.
	expectError(t, srv, bobName, http.MethodGet,
		`/api/trpc/board.getBoardSettings`+encodedInput(fmt.Sprintf(`{"id":%q}`, boardID)), "", "NOT_FOUND")

	input := fmt.Sprintf(`{"id":%q,"pageTitle":"Hello","metaTitle":"","primaryColor":"#112233","opacity":50,"disableStatus":true,"itemRadius":"md","backgroundImageAttachment":"scroll","backgroundImageRepeat":"no-repeat","backgroundImageSize":"contain","secondaryColor":"#445566","logoImageUrl":"https://example.com/logo.png","faviconImageUrl":"","backgroundImageUrl":"https://example.com/bg.png","customCss":"body{}","iconColor":""}`, boardID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.savePartialBoardSettings", superjsonEnvelope(input))
	if status != http.StatusOK {
		t.Fatalf("savePartialBoardSettings status %d body %s", status, body)
	}

	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardSettings`+encodedInput(fmt.Sprintf(`{"id":%q}`, boardID)), "")
	if status != http.StatusOK {
		t.Fatalf("getBoardSettings status %d body %s", status, body)
	}
	settings := unwrapData(t, body)
	var out map[string]any
	if err := json.Unmarshal(settings, &out); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	// Nullable strings render "" (boardSettingsSchema) instead of null.
	checks := map[string]any{
		"id": boardID, "name": "SettingsBoard", "pageTitle": "Hello", "metaTitle": "",
		"primaryColor": "#112233", "secondaryColor": "#445566", "opacity": float64(50),
		"disableStatus": true, "itemRadius": "md", "backgroundImageAttachment": "scroll",
		"backgroundImageRepeat": "no-repeat", "backgroundImageSize": "contain",
		"logoImageUrl": "https://example.com/logo.png", "faviconImageUrl": "",
		"backgroundImageUrl": "https://example.com/bg.png", "customCss": "body{}", "iconColor": "",
	}
	for key, want := range checks {
		if out[key] != want {
			t.Fatalf("settings[%q] = %v, want %v", key, out[key], want)
		}
	}

	// Invalid colors and enums are rejected (BAD_REQUEST).
	expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.savePartialBoardSettings",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"primaryColor":"red"}`, boardID)), "BAD_REQUEST")
	expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.savePartialBoardSettings",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"itemRadius":"huge"}`, boardID)), "BAD_REQUEST")
}

// TestBatchCallSanity sends one batch mixing two procedures through the wire
// and checks both the classic array envelope and the JSONL stream head.
func TestBatchCallSanity(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice
	grantBoardCreate(t, d, "user-alice")
	mustCreateBoard(t, srv, aliceName, "BatchBoard", 10, true)

	// Classic batch (httpBatchLink shape): array of envelopes.
	input := fmt.Sprintf(`{"0":%s,"1":%s}`, superjsonEnvelope("null"), superjsonEnvelope("null"))
	status, body := call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getAllBoards,board.getPublicBoards?batch=1&input=`+input, "")
	if status != http.StatusOK {
		t.Fatalf("batch status %d body %s", status, body)
	}
	var envelopes []json.RawMessage
	if err := json.Unmarshal([]byte(body), &envelopes); err != nil {
		t.Fatalf("batch body is not an array: %v (%s)", err, body)
	}
	if len(envelopes) != 2 {
		t.Fatalf("batch length = %d, want 2", len(envelopes))
	}
	for i, envelope := range envelopes {
		var decoded struct {
			Result struct {
				Data struct {
					JSON json.RawMessage `json:"json"`
				} `json:"data"`
			} `json:"result"`
		}
		if err := json.Unmarshal(envelope, &decoded); err != nil {
			t.Fatalf("batch envelope %d malformed: %v (%s)", i, err, envelope)
		}
	}

	// Stream batch (httpBatchStreamLink shape): one JSONL head line wrapping
	// every op response.
	req, err := http.NewRequest(http.MethodGet, srv.URL+`/api/trpc/board.getAllBoards,board.getPublicBoards?batch=1&input=`+input, nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("trpc-accept", "application/jsonl")
	req.Header.Set("X-Test-User", aliceName)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer res.Body.Close()
	stream, err := io_ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d body %s", res.StatusCode, stream)
	}
	var head struct {
		JSON map[string][]json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stream)), &head); err != nil {
		t.Fatalf("stream head malformed: %v (%s)", err, stream)
	}
	if len(head.JSON) != 2 {
		t.Fatalf("stream head ops = %d, want 2 (%s)", len(head.JSON), stream)
	}
	for key, entries := range head.JSON {
		// head.JSON[key] is the EncodedValue tuple [[entry]]; the first level
		// (already unwrapped by []json.RawMessage) holds the single-element
		// array, whose element is the op response (stream/jsonl.ts:234-266,
		// same decode shape as trpc_test.decodeHeadEntry).
		var tuple [1]json.RawMessage
		if err := json.Unmarshal(entries[0], &tuple); err != nil {
			t.Fatalf("stream op %q tuple malformed: %v raw=%s", key, err, entries[0])
		}
		// The stream head wraps each line with superjson, so result.data carries
		// the raw output (no extra {"json":...} level, marshalStreamEntry).
		var entry struct {
			Result *struct {
				Data json.RawMessage `json:"data"`
			} `json:"result"`
		}
		if err := json.Unmarshal(tuple[0], &entry); err != nil {
			t.Fatalf("stream op %q entry malformed: %v", key, err)
		}
		if entry.Result == nil || len(entry.Result.Data) == 0 {
			t.Fatalf("stream op %q has no result (%s)", key, stream)
		}
	}
}

// io_ReadAll avoids importing io in each test file for one call.
func io_ReadAll(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	return string(data), err
}

func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
