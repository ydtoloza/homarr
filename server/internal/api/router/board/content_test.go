package board_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Content mutation tests: board.saveBoard, board.addItem,
// board.updateItemLayout, section.changeCollapsed and userItemState.

// saveBoardInput builds the board.saveBoard payload.
func saveBoardInput(boardID string, sections, items string) string {
	return fmt.Sprintf(`{"id":%q,"sections":%s,"items":%s}`, boardID, sections, items)
}

// TestSectionItemCRUDAndLayoutPersistence exercises the editor loop: save
// sections/items, read them back through getBoardByName, move an item and
// verify the persisted placements.
func TestSectionItemCRUDAndLayoutPersistence(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "EditorBoard", 10, false)
	// Find the root section the createBoard procedure seeded.
	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	containerID, itemID, notebookID := "sec-container-1", "item-clock-1", "item-notebook-1"
	sections := fmt.Sprintf(`[
		{"id":%q,"kind":"empty","xOffset":0,"yOffset":0},
		{"id":%q,"kind":"container","options":{"title":"My Box","collapsible":true},"layouts":[{"layoutId":%q,"parentSectionId":%q,"xOffset":6,"yOffset":0,"width":4,"height":3}]}
	]`, rootSectionID, containerID, baseLayoutID, rootSectionID)
	items := fmt.Sprintf(`[
		{"id":%q,"kind":"clock","options":{"timeZone":"utc"},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[],"advancedOptions":{"title":"Clock"}},
		{"id":%q,"kind":"notebook","options":{"content":"<p>hi</p>"},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":0,"width":4,"height":2}],"integrationIds":[]}
	]`, itemID, baseLayoutID, rootSectionID, notebookID, baseLayoutID, rootSectionID)

	// Bob has no access: NOT_FOUND (anti-snooping).
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID, sections, items)), "NOT_FOUND")

	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID, sections, items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Read the board back and validate the output shapes.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardByName`+encodedInput(`{"name":"EditorBoard"}`), "")
	if status != http.StatusOK {
		t.Fatalf("getBoardByName status %d body %s", status, body)
	}
	var full struct {
		Sections []map[string]any `json:"sections"`
		Items    []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &full); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	if len(full.Sections) != 2 || len(full.Items) != 2 {
		t.Fatalf("sections=%d items=%d, want 2/2 (%s)", len(full.Sections), len(full.Items), body)
	}
	var container map[string]any
	for _, sec := range full.Sections {
		switch sec["kind"] {
		case "empty":
			// Empty sections strip options/collapsed/layouts (union schema).
			if got := sortedKeys(sec); fmt.Sprint(got) != fmt.Sprint([]string{"id", "kind", "xOffset", "yOffset"}) {
				t.Fatalf("empty section keys = %v", got)
			}
		case "container":
			container = sec
			if got := sortedKeys(sec); fmt.Sprint(got) != fmt.Sprint([]string{"collapsed", "id", "kind", "layouts", "options"}) {
				t.Fatalf("container section keys = %v", got)
			}
		}
	}
	if container == nil {
		t.Fatalf("container section missing (%s)", body)
	}
	options, ok := container["options"].(map[string]any)
	if !ok || options["title"] != "My Box" || options["collapsible"] != true {
		t.Fatalf("container options = %+v", container["options"])
	}
	// Defaults applied by containerSectionOptionsSchema.
	if options["showLabel"] != true || options["borderColor"] != "" || options["scrollable"] != false {
		t.Fatalf("container option defaults = %+v", options)
	}
	if container["collapsed"] != false {
		t.Fatalf("collapsed = %+v, want false", container["collapsed"])
	}
	layouts, ok := container["layouts"].([]any)
	if !ok || len(layouts) != 1 {
		t.Fatalf("container layouts = %+v", container["layouts"])
	}
	layout := layouts[0].(map[string]any)
	if layout["parentSectionId"] != rootSectionID || layout["width"] != float64(4) || layout["height"] != float64(3) {
		t.Fatalf("container layout = %+v", layout)
	}

	var clock map[string]any
	for _, item := range full.Items {
		if item["id"] == itemID {
			clock = item
		}
	}
	if clock == nil {
		t.Fatalf("clock item missing (%s)", body)
	}
	if clock["kind"] != "clock" {
		t.Fatalf("clock kind = %+v", clock["kind"])
	}
	if got := sortedKeys(clock); fmt.Sprint(got) != fmt.Sprint([]string{"advancedOptions", "id", "integrationIds", "kind", "layouts", "options"}) {
		t.Fatalf("item keys = %v", got)
	}
	advanced, ok := clock["advancedOptions"].(map[string]any)
	if !ok || advanced["title"] != "Clock" || advanced["borderColor"] != "" || advanced["customCssClasses"] == nil {
		t.Fatalf("advancedOptions = %+v", clock["advancedOptions"])
	}

	// updateItemLayout moves the clock; the input echoes back.
	moveInput := fmt.Sprintf(`{"boardId":%q,"itemId":%q,"layoutId":%q,"xOffset":2,"yOffset":2,"width":2,"height":1}`,
		boardID, itemID, baseLayoutID)
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.updateItemLayout", superjsonEnvelope(moveInput))
	if status != http.StatusOK {
		t.Fatalf("updateItemLayout status %d body %s", status, body)
	}
	var echo map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &echo); err != nil {
		t.Fatalf("decode echo: %v (%s)", err, body)
	}
	if echo["xOffset"] != float64(2) || echo["yOffset"] != float64(2) || echo["itemId"] != itemID {
		t.Fatalf("updateItemLayout echo = %+v", echo)
	}
	var xOffset, yOffset int
	if err := d.QueryRow(`SELECT x_offset, y_offset FROM "item_layout" WHERE item_id = ? AND layout_id = ?`,
		itemID, baseLayoutID).Scan(&xOffset, &yOffset); err != nil {
		t.Fatalf("read item layout: %v", err)
	}
	if xOffset != 2 || yOffset != 2 {
		t.Fatalf("persisted position = (%d,%d), want (2,2)", xOffset, yOffset)
	}

	// Overlapping the notebook conflicts.
	expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.updateItemLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"itemId":%q,"layoutId":%q,"xOffset":3,"yOffset":0,"width":2,"height":1}`,
			boardID, itemID, baseLayoutID)), "CONFLICT")
	// Out of the 10-column canvas: a bounds BAD_REQUEST carrying the
	// isBoardLayoutBoundsError marker.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.updateItemLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"itemId":%q,"layoutId":%q,"xOffset":9,"yOffset":0,"width":4,"height":1}`,
			boardID, itemID, baseLayoutID)))
	code, _ := unwrapError(t, body)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Fatalf("bounds error status %d body %s", status, body)
	}
	if !strings.Contains(body, `"isBoardLayoutBoundsError":true`) {
		t.Fatalf("bounds error missing marker: %s", body)
	}

	// bob without access: NOT_FOUND.
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.updateItemLayout",
		superjsonEnvelope(moveInput), "NOT_FOUND")

	// saveBoard with the notebook removed deletes it (removedItems diff).
	itemsWithoutNotebook := fmt.Sprintf(`[
		{"id":%q,"kind":"clock","options":{"timeZone":"utc"},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":1,"width":2,"height":1}],"integrationIds":[]}
	]`, itemID, baseLayoutID, rootSectionID)
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID, sections, itemsWithoutNotebook)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard diff status %d body %s", status, body)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "item" WHERE board_id = ?`, boardID).Scan(&count); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if count != 1 {
		t.Fatalf("items after diff = %d, want 1", count)
	}

	// board.addItem places a clock at the first free slot of the main canvas.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.addItem",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"kind":"clock","options":{},"integrationIds":[]}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("addItem status %d body %s", status, body)
	}
	var added struct {
		ItemID string `json:"itemId"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &added); err != nil || added.ItemID == "" {
		t.Fatalf("addItem output = %s", body)
	}
	var placedSectionID string
	var placedX, placedY int
	if err := d.QueryRow(`SELECT section_id, x_offset, y_offset FROM "item_layout" WHERE item_id = ? AND layout_id = ?`,
		added.ItemID, baseLayoutID).Scan(&placedSectionID, &placedX, &placedY); err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if placedSectionID != rootSectionID || placedX != 0 || placedY != 0 {
		t.Fatalf("addItem placement = (%s,%d,%d), want (%s,0,0)", placedSectionID, placedX, placedY, rootSectionID)
	}

	// Anonymous callers are rejected.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/board.addItem",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"kind":"clock"}`, boardID)), "UNAUTHORIZED")
}

// TestSectionCollapse covers section.changeCollapsed.
func TestSectionCollapse(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice
	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "CollapseBoard", 10, false)
	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}
	containerID := "sec-collapse-1"
	sections := fmt.Sprintf(`[
		{"id":%q,"kind":"empty","xOffset":0,"yOffset":0},
		{"id":%q,"kind":"container","options":{"collapsible":true},"layouts":[{"layoutId":%q,"parentSectionId":%q,"xOffset":0,"yOffset":0,"width":4,"height":3}]}
	]`, rootSectionID, containerID, baseLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID, sections, `[]`)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Unknown section: NOT_FOUND with the upstream message.
	message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/section.changeCollapsed",
		superjsonEnvelope(`{"sectionId":"missing","collapsed":true}`), "NOT_FOUND")
	if message != "Section not found id=missing" {
		t.Fatalf("collapse not-found message = %q", message)
	}
	// Empty sections cannot collapse: BAD_REQUEST.
	message = expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/section.changeCollapsed",
		superjsonEnvelope(fmt.Sprintf(`{"sectionId":%q,"collapsed":true}`, rootSectionID)), "BAD_REQUEST")
	if message != "Section cannot be collapsed id="+rootSectionID {
		t.Fatalf("collapse bad-request message = %q", message)
	}

	// Anonymous: UNAUTHORIZED.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/section.changeCollapsed",
		superjsonEnvelope(fmt.Sprintf(`{"sectionId":%q,"collapsed":true}`, containerID)), "UNAUTHORIZED")

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/section.changeCollapsed",
		superjsonEnvelope(fmt.Sprintf(`{"sectionId":%q,"collapsed":true}`, containerID)))
	if status != http.StatusOK {
		t.Fatalf("changeCollapsed status %d body %s", status, body)
	}
	var collapsed int
	if err := d.QueryRow(`SELECT collapsed FROM "section_collapse_state" WHERE section_id = ? AND user_id = 'user-alice'`,
		containerID).Scan(&collapsed); err != nil {
		t.Fatalf("read collapse state: %v", err)
	}
	if collapsed != 1 {
		t.Fatalf("collapse state = %d, want 1", collapsed)
	}

	// The full board read renders the caller-scoped collapsed flag.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardByName`+encodedInput(`{"name":"CollapseBoard"}`), "")
	var full struct {
		Sections []map[string]any `json:"sections"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &full); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	for _, sec := range full.Sections {
		if sec["id"] == containerID && sec["collapsed"] != true {
			t.Fatalf("collapsed flag = %+v, want true", sec)
		}
	}
}

// TestUserItemState covers userItemState.set/get with per-user isolation and
// the 500-key cap.
func TestUserItemState(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob
	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "StateBoard", 10, false)
	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}
	itemID := "item-state-1"
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID),
			fmt.Sprintf(`[{"id":%q,"kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[]}]`,
				itemID, baseLayoutID, rootSectionID))))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Anonymous: UNAUTHORIZED.
	expectError(t, srv, "", http.MethodGet,
		`/api/trpc/userItemState.get`+encodedInput(fmt.Sprintf(`{"itemId":%q}`, itemID)), "", "UNAUTHORIZED")
	// Unknown item: NOT_FOUND.
	expectError(t, srv, aliceName, http.MethodGet,
		`/api/trpc/userItemState.get`+encodedInput(`{"itemId":"missing"}`), "", "NOT_FOUND")

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/userItemState.set",
		superjsonEnvelope(fmt.Sprintf(`{"itemId":%q,"key":"collapsed","value":"true"}`, itemID)))
	if status != http.StatusOK {
		t.Fatalf("set status %d body %s", status, body)
	}

	// get returns the raw JSON values keyed by state key.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/userItemState.get`+encodedInput(fmt.Sprintf(`{"itemId":%q}`, itemID)), "")
	if status != http.StatusOK {
		t.Fatalf("get status %d body %s", status, body)
	}
	if got := unwrapData(t, body); string(got) != `{"collapsed":true}` {
		t.Fatalf("userItemState.get = %s, want {\"collapsed\":true}", got)
	}

	// Per-user isolation: bob has view access to the board (granted so the
	// itemStateProcedure gate passes) but no state rows for the item.
	seedBoardUserPermission(t, d, boardID, "user-bob", "view")
	status, body = call(t, srv, bobName, http.MethodGet,
		`/api/trpc/userItemState.get`+encodedInput(fmt.Sprintf(`{"itemId":%q}`, itemID)), "")
	if status != http.StatusOK || string(unwrapData(t, body)) != "{}" {
		t.Fatalf("bob get = %s (%d), want {}", body, status)
	}

	// Updating an existing key skips the cap; a new key past 500 fails.
	for i := 0; i < 500; i++ {
		if _, err := d.Exec(`INSERT INTO "user_item_state" (user_id, item_id, key, value)
			VALUES ('user-alice', ?, ?, '1')`, itemID, fmt.Sprintf("key-%03d", i)); err != nil {
			t.Fatalf("seed state: %v", err)
		}
	}
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/userItemState.set",
		superjsonEnvelope(fmt.Sprintf(`{"itemId":%q,"key":"collapsed","value":"false"}`, itemID)))
	if status != http.StatusOK {
		t.Fatalf("set existing key status %d body %s", status, body)
	}
	message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/userItemState.set",
		superjsonEnvelope(fmt.Sprintf(`{"itemId":%q,"key":"another","value":"1"}`, itemID)), "BAD_REQUEST")
	if message != "Each user can save at most 500 state keys per item. Existing keys can still be updated." {
		t.Fatalf("cap message = %q", message)
	}

	// Invalid JSON values are rejected (Value must be JSON).
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/userItemState.set",
		superjsonEnvelope(fmt.Sprintf(`{"itemId":%q,"key":"x","value":"not-json"}`, itemID)), "BAD_REQUEST")
}

// TestGetHomeBoardFallbacks covers the home board chain of getHomeIdBoardAsync.
func TestGetHomeBoardFallbacks(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice

	grantBoardCreate(t, d, "user-alice")
	boardID, _ := mustCreateBoard(t, srv, aliceName, "HomeBoard", 10, true)

	// No home board anywhere: NOT_FOUND "No home board found".
	message := expectError(t, srv, "", http.MethodGet, "/api/trpc/board.getHomeBoard", "", "NOT_FOUND")
	if message != "No home board found" {
		t.Fatalf("home board message = %q", message)
	}

	// The server settings fallback applies to anonymous callers.
	if _, err := d.Exec(`INSERT INTO "serverSetting" (setting_key, value) VALUES ('board', ?)`,
		fmt.Sprintf(`{"json":{"homeBoardId":%q}}`, boardID)); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	status, body := call(t, srv, "", http.MethodGet, "/api/trpc/board.getHomeBoard", "")
	if status != http.StatusOK {
		t.Fatalf("anonymous getHomeBoard status %d body %s", status, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil || out.ID != boardID {
		t.Fatalf("anonymous home board = %s", body)
	}

	// The user selection wins over the server settings.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.setHomeBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("setHomeBoard status %d", status)
	}
	status, body = call(t, srv, aliceName, http.MethodGet, "/api/trpc/board.getHomeBoard", "")
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil || out.ID != boardID {
		t.Fatalf("user home board = %s", body)
	}
	_ = status
}

// TestGetBoardsForGroup covers the admin-only group board listing.
func TestGetBoardsForGroup(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	root := seedUser(t, d, testUser{id: "user-root", name: rootName, admin: true})
	sessions[aliceName], sessions[rootName] = alice, root

	grantBoardCreate(t, d, "user-alice")
	boardID, _ := mustCreateBoard(t, srv, aliceName, "GroupBoard", 10, false)
	seedGroup(t, d, "group-everyone", "everyone", nil, nil)
	seedGroup(t, d, "group-viewers", "viewers", nil, nil)
	seedBoardGroupPermission(t, d, boardID, "group-viewers", "view")

	// Non-admin callers: FORBIDDEN "Permission denied".
	expectError(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardsForGroup`+encodedInput(`{"groupId":"group-viewers"}`), "", "FORBIDDEN")

	// The admin sees the group's boards (plus public ones).
	status, body := call(t, srv, rootName, http.MethodGet,
		`/api/trpc/board.getBoardsForGroup`+encodedInput(`{"groupId":"group-viewers"}`), "")
	if status != http.StatusOK {
		t.Fatalf("getBoardsForGroup status %d body %s", status, body)
	}
	var boards []map[string]any
	if err := json.Unmarshal(unwrapData(t, body), &boards); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(boards) != 1 || boards[0]["name"] != "GroupBoard" {
		t.Fatalf("group boards = %+v", boards)
	}

	// The anonymous caller lacks the admin permission.
	expectError(t, srv, "", http.MethodGet,
		`/api/trpc/board.getBoardsForGroup`+encodedInput(`{"groupId":"group-viewers"}`), "", "UNAUTHORIZED")
}
