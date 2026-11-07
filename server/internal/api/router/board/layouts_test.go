package board_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// Procedure tests for the layouts domain: board.saveLayouts,
// board.resetLayout and board.duplicateBoard — the three procedures built on
// the responsive grid projection (grid.go).

// seedIntegration inserts one integration row.
func seedIntegration(t *testing.T, d *db.DB, id, kind string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO "integration" (id, name, url, kind) VALUES (?, ?, ?, ?)`,
		id, "Integration "+id, "https://integration.example.com", kind); err != nil {
		t.Fatalf("seed integration: %v", err)
	}
}

// grantIntegrationUse grants the user direct "use" access to an integration.
func grantIntegrationUse(t *testing.T, d *db.DB, userID, integrationID string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO "integrationUserPermission" (integration_id, user_id, permission) VALUES (?, ?, 'use')`,
		integrationID, userID); err != nil {
		t.Fatalf("seed integration permission: %v", err)
	}
}

// saveBoardInput2 builds the board.saveBoard payload (distinct name from the
// harness helper used by content_test.go).
func saveBoardInput2(boardID string, sections, items string) string {
	return fmt.Sprintf(`{"id":%q,"sections":%s,"items":%s}`, boardID, sections, items)
}

// saveLayoutsInput builds a board.saveLayouts payload from layout entries.
func saveLayoutsInput(boardID string, layouts string) string {
	return fmt.Sprintf(`{"id":%q,"layouts":[%s]}`, boardID, layouts)
}

// layoutEntry is one boardLayoutSchema JSON entry.
func layoutEntry(id, name string, columnCount, left, right, breakpoint int, role string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"columnCount":%d,"leftGutterColumnCount":%d,"rightGutterColumnCount":%d,"breakpoint":%d,"role":%q}`,
		id, name, columnCount, left, right, breakpoint, role)
}

// layoutOut mirrors one entry of the saveLayouts output.
type layoutOut struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	ColumnCount            int    `json:"columnCount"`
	LeftGutterColumnCount  int    `json:"leftGutterColumnCount"`
	RightGutterColumnCount int    `json:"rightGutterColumnCount"`
	Breakpoint             int    `json:"breakpoint"`
	Role                   string `json:"role"`
}

// decodeLayoutsOutput unwraps the saveLayouts output array.
func decodeLayoutsOutput(t *testing.T, body string) []layoutOut {
	t.Helper()
	var out []layoutOut
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode saveLayouts output: %v (%s)", err, body)
	}
	return out
}

// itemPlacementRow is one item_layout row as read back through SQL.
type itemPlacementRow struct {
	itemID  string
	xOffset int
	yOffset int
	width   int
	height  int
}

// readItemPlacements returns the placements of one layout ordered by item id.
func readItemPlacements(t *testing.T, d *db.DB, layoutID string) []itemPlacementRow {
	t.Helper()
	rows, err := d.Query(`SELECT item_id, x_offset, y_offset, width, height FROM "item_layout"
		WHERE layout_id = ? ORDER BY item_id`, layoutID)
	if err != nil {
		t.Fatalf("read item layouts: %v", err)
	}
	defer rows.Close()
	var out []itemPlacementRow
	for rows.Next() {
		var row itemPlacementRow
		if err := rows.Scan(&row.itemID, &row.xOffset, &row.yOffset, &row.width, &row.height); err != nil {
			t.Fatalf("scan item layout: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read item layouts: %v", err)
	}
	return out
}

// layoutResizeSource reads the stored resize_source of one layout ("" = NULL).
func layoutResizeSource(t *testing.T, d *db.DB, layoutID string) string {
	t.Helper()
	var resizeSource []byte
	if err := d.QueryRow(`SELECT resize_source FROM "layout" WHERE id = ?`, layoutID).Scan(&resizeSource); err != nil {
		t.Fatalf("read resize source: %v", err)
	}
	return string(resizeSource)
}

// TestSaveLayoutsRoundtrip exercises the editor loop over the layouts
// settings: widening keeps offsets and stores the resize history, a follow-up
// resize back to the original geometry rewinds and clears it, and adding a
// custom layout projects the Base content onto it.
func TestSaveLayoutsRoundtrip(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "LayoutBoard", 6, false)

	var mobileLayoutID, rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	containerID, clockID, notebookID := "sec-container-1", "item-clock-1", "item-notebook-1"
	sections := fmt.Sprintf(`[
		{"id":%q,"kind":"empty","xOffset":0,"yOffset":0},
		{"id":%q,"kind":"container","options":{"title":"Box","collapsible":true},"layouts":[{"layoutId":%q,"parentSectionId":%q,"xOffset":3,"yOffset":0,"width":3,"height":2}]}
	]`, rootSectionID, containerID, baseLayoutID, rootSectionID)
	items := fmt.Sprintf(`[
		{"id":%q,"kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[]},
		{"id":%q,"kind":"notebook","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":3,"width":2,"height":1}],"integrationIds":[]}
	]`, clockID, baseLayoutID, rootSectionID, notebookID, baseLayoutID, rootSectionID)

	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID, sections, items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Guards first: anonymous callers are UNAUTHORIZED (protectedProcedure)
	// and callers without access get the anti-snooping NOT_FOUND. The input
	// validation runs before the board access check (like the upstream zod
	// parse), so the payloads must be schema-valid.
	mobileEntry := layoutEntry(mobileLayoutID, "Mobile", 3, 0, 0, 0, "mobile")
	baseEntry := layoutEntry(baseLayoutID, "Base", 10, 0, 0, 768, "base")
	validPair := mobileEntry + "," + layoutEntry(baseLayoutID, "Base", 6, 0, 0, 768, "base")
	expectError(t, srv, "", http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, validPair)), "UNAUTHORIZED")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, validPair)), "NOT_FOUND")

	// Validation errors, in the order upstream rejects them. A single layout
	// fails the responsiveBoardLayoutsSchema min(2) + superRefine checks.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, mobileEntry)), "BAD_REQUEST"); message != "Boards require exactly one Mobile and one Base layout" {
		t.Fatalf("single layout message = %q", message)
	}
	// Mobile layouts cannot have sidebars.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID,
			layoutEntry(mobileLayoutID, "Mobile", 3, 1, 0, 0, "mobile")+","+baseEntry)), "BAD_REQUEST"); message != "Mobile layouts cannot have sidebars" {
		t.Fatalf("mobile gutters message = %q", message)
	}
	// Gutters must leave at least one dashboard column.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID,
			mobileEntry+","+layoutEntry(baseLayoutID, "Base", 4, 2, 2, 768, "base"))), "BAD_REQUEST"); message != "Gutters must leave at least one dashboard column" {
		t.Fatalf("gutters message = %q", message)
	}
	// Unique breakpoints.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID,
			mobileEntry+","+layoutEntry(baseLayoutID, "Base", 10, 0, 0, 0, "base"))), "BAD_REQUEST"); message != "Layout breakpoints must be unique" {
		t.Fatalf("breakpoints message = %q", message)
	}
	// Added layouts must use the custom role: a new layout claiming the base
	// role (with the stored base removed) fails before anything is written.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID,
			mobileEntry+","+layoutEntry("input-new-base", "Base", 10, 0, 0, 500, "base"))), "BAD_REQUEST"); message != "New layouts must use the custom role" {
		t.Fatalf("added role message = %q", message)
	}

	// Widening 6 -> 10: the output echoes the submitted layouts (sorted by
	// breakpoint) and the Base layout gains a resize history.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, mobileEntry+","+baseEntry)))
	if status != http.StatusOK {
		t.Fatalf("saveLayouts status %d body %s", status, body)
	}
	layouts := decodeLayoutsOutput(t, body)
	if len(layouts) != 2 {
		t.Fatalf("layouts = %d entries (%s)", len(layouts), body)
	}
	if layouts[0].ID != mobileLayoutID || layouts[0].Breakpoint != 0 || layouts[0].Role != "mobile" || layouts[0].ColumnCount != 3 {
		t.Fatalf("mobile output = %+v", layouts[0])
	}
	if layouts[1].ID != baseLayoutID || layouts[1].Breakpoint != 768 || layouts[1].Role != "base" || layouts[1].ColumnCount != 10 {
		t.Fatalf("base output = %+v", layouts[1])
	}

	resizeSource := layoutResizeSource(t, d, baseLayoutID)
	if resizeSource == "" {
		t.Fatal("base resize source missing after resize")
	}
	var source struct {
		Geometry struct {
			ColumnCount int `json:"columnCount"`
		} `json:"geometry"`
		ProjectedGeometry struct {
			ColumnCount int `json:"columnCount"`
		} `json:"projectedGeometry"`
		Elements []struct {
			ID string `json:"id"`
		} `json:"elements"`
		ProjectedElements []struct {
			ID string `json:"id"`
		} `json:"projectedElements"`
	}
	if err := json.Unmarshal([]byte(resizeSource), &source); err != nil {
		t.Fatalf("decode resize source %s: %v", resizeSource, err)
	}
	if source.Geometry.ColumnCount != 6 || source.ProjectedGeometry.ColumnCount != 10 {
		t.Fatalf("resize source geometry = %d -> %d", source.Geometry.ColumnCount, source.ProjectedGeometry.ColumnCount)
	}
	if len(source.Elements) == 0 || len(source.Elements) != len(source.ProjectedElements) {
		t.Fatalf("resize source elements = %d / %d", len(source.Elements), len(source.ProjectedElements))
	}

	// Widening keeps every offset.
	for _, row := range readItemPlacements(t, d, baseLayoutID) {
		if row.itemID == clockID && (row.xOffset != 0 || row.yOffset != 0 || row.width != 2) {
			t.Fatalf("clock placement = %+v", row)
		}
		if row.itemID == notebookID && (row.xOffset != 0 || row.yOffset != 3 || row.width != 2) {
			t.Fatalf("notebook placement = %+v", row)
		}
	}

	// Resizing back to the original column count rewinds through the stored
	// history and clears it.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID,
			mobileEntry+","+layoutEntry(baseLayoutID, "Base", 6, 0, 0, 768, "base"))))
	if status != http.StatusOK {
		t.Fatalf("saveLayouts rewind status %d body %s", status, body)
	}
	if got := layoutResizeSource(t, d, baseLayoutID); got != "" {
		t.Fatalf("resize source after rewind = %s, want NULL", got)
	}

	// Adding a custom layout projects the current Base arrangement onto it.
	tabletInputID := "input-tablet-layout"
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, mobileEntry+","+
			layoutEntry(baseLayoutID, "Base", 6, 0, 0, 768, "base")+","+
			layoutEntry(tabletInputID, "Tablet", 5, 0, 0, 500, "custom"))))
	if status != http.StatusOK {
		t.Fatalf("saveLayouts add status %d body %s", status, body)
	}
	layouts = decodeLayoutsOutput(t, body)
	if len(layouts) != 3 || layouts[1].Breakpoint != 500 || layouts[1].Role != "custom" || layouts[1].Name != "Tablet" {
		t.Fatalf("tablet output = %+v (%s)", layouts, body)
	}
	tabletID := layouts[1].ID
	if tabletID == tabletInputID || tabletID == "" {
		t.Fatalf("tablet id not generated: %q", tabletID)
	}
	// The projection produced placements on the new layout.
	if rows := readItemPlacements(t, d, tabletID); len(rows) != 2 {
		t.Fatalf("tablet placements = %d rows, want 2 (%+v)", len(rows), rows)
	}

	// Removed custom layouts disappear with their placements.
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, mobileEntry+","+
			layoutEntry(baseLayoutID, "Base", 6, 0, 0, 768, "base"))))
	if status != http.StatusOK {
		t.Fatalf("saveLayouts remove status %d body %s", status, body)
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "layout" WHERE id = ?`, tabletID).Scan(&count); err != nil {
		t.Fatalf("count layouts: %v", err)
	}
	if count != 0 {
		t.Fatal("custom layout not removed")
	}
}

// TestSaveLayoutsNarrowingProjectsPlacements verifies the packing projection
// of a narrowing resize through the procedure.
func TestSaveLayoutsNarrowingProjectsPlacements(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "NarrowBoard", 12, false)

	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	items := fmt.Sprintf(`[
		{"id":"item-a","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[]},
		{"id":"item-b","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":0,"width":6,"height":1}],"integrationIds":[]},
		{"id":"item-c","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":8,"yOffset":0,"width":3,"height":1}],"integrationIds":[]}
	]`, baseLayoutID, rootSectionID, baseLayoutID, rootSectionID, baseLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID, fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID), items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	mobileEntry := layoutEntry("mobile-placeholder", "Mobile", 3, 0, 0, 0, "mobile")
	var mobileLayoutID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	mobileEntry = layoutEntry(mobileLayoutID, "Mobile", 3, 0, 0, 0, "mobile")
	baseEntry := layoutEntry(baseLayoutID, "Base", 5, 0, 0, 768, "base")

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveLayouts",
		superjsonEnvelope(saveLayoutsInput(boardID, mobileEntry+","+baseEntry)))
	if status != http.StatusOK {
		t.Fatalf("saveLayouts status %d body %s", status, body)
	}

	// 12 -> 5 packs in scan order: a stays at (0,0) with width 2, c (3 wide)
	// fits the remaining gap of row 0 at x=2, and b (clamped to 5) takes the
	// next row.
	placements := map[[4]int]bool{}
	for _, row := range readItemPlacements(t, d, baseLayoutID) {
		placements[[4]int{row.xOffset, row.yOffset, row.width, row.height}] = true
	}
	want := map[[4]int]bool{
		{0, 0, 2, 1}: true, // item-a
		{2, 0, 3, 1}: true, // item-c
		{0, 1, 5, 1}: true, // item-b, width clamped to 5
	}
	if len(placements) != len(want) {
		t.Fatalf("placements = %+v, want %+v", placements, want)
	}
	for position := range want {
		if !placements[position] {
			t.Fatalf("placements = %+v, missing %+v", placements, position)
		}
	}
}

// TestResetLayoutRestoresFromBase verifies that resetting a layout replaces
// its placements with the projection of the Base layout.
func TestResetLayoutRestoresFromBase(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "ResetBoard", 6, false)

	var mobileLayoutID, rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	// The items carry different placements per layout; the Mobile one is
	// wiped by the reset.
	items := fmt.Sprintf(`[
		{"id":"item-a","kind":"clock","options":{},"layouts":[
			{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1},
			{"layoutId":%q,"sectionId":%q,"xOffset":1,"yOffset":1,"width":1,"height":1}],"integrationIds":[]},
		{"id":"item-b","kind":"clock","options":{},"layouts":[
			{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":0,"width":2,"height":1},
			{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":2,"width":1,"height":1}],"integrationIds":[]}
	]`, baseLayoutID, rootSectionID, mobileLayoutID, rootSectionID,
		baseLayoutID, rootSectionID, mobileLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID), items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Guards: anonymous, missing board and missing layout.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/board.resetLayout",
		superjsonEnvelope(`{"boardId":"x","layoutId":"y"}`), "UNAUTHORIZED")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.resetLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"layoutId":%q}`, boardID, mobileLayoutID)), "NOT_FOUND")
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.resetLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"layoutId":"unknown-layout"}`, boardID)), "NOT_FOUND"); message != "Layout not found" {
		t.Fatalf("unknown layout message = %q", message)
	}
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.resetLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"layoutId":%q}`, boardID, baseLayoutID)), "BAD_REQUEST"); message != "The Base layout cannot be reset" {
		t.Fatalf("base reset message = %q", message)
	}

	// Reset the Mobile layout: every placement comes from the Base
	// projection (6 -> 3 columns, packing).
	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.resetLayout",
		superjsonEnvelope(fmt.Sprintf(`{"boardId":%q,"layoutId":%q}`, boardID, mobileLayoutID)))
	if status != http.StatusOK {
		t.Fatalf("resetLayout status %d body %s", status, body)
	}
	rows := map[string]itemPlacementRow{}
	for _, row := range readItemPlacements(t, d, mobileLayoutID) {
		rows[row.itemID] = row
	}
	if got := rows["item-a"]; got.xOffset != 0 || got.yOffset != 0 || got.width != 2 || got.height != 1 {
		t.Fatalf("item-a mobile placement = %+v, want projected (0,0)", got)
	}
	if got := rows["item-b"]; got.xOffset != 0 || got.yOffset != 1 || got.width != 2 || got.height != 1 {
		t.Fatalf("item-b mobile placement = %+v, want projected (0,1)", got)
	}
	// The Base placements are untouched.
	baseRows := map[string]itemPlacementRow{}
	for _, row := range readItemPlacements(t, d, baseLayoutID) {
		baseRows[row.itemID] = row
	}
	if got := baseRows["item-a"]; got.xOffset != 0 || got.yOffset != 0 || got.width != 2 {
		t.Fatalf("item-a base placement = %+v", got)
	}
	if got := baseRows["item-b"]; got.xOffset != 2 || got.yOffset != 0 || got.width != 2 {
		t.Fatalf("item-b base placement = %+v", got)
	}
	if got := layoutResizeSource(t, d, mobileLayoutID); got != "" {
		t.Fatalf("mobile resize source = %s, want NULL", got)
	}
}

// TestDuplicateBoardClonesBoard verifies the full clone: board row, layouts,
// sections (with placements and collapse states), items (with placements and
// integrations) all get fresh ids.
func TestDuplicateBoardClonesBoard(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	bob := seedUser(t, d, testUser{id: "user-bob", name: bobName})
	sessions[aliceName], sessions[bobName] = alice, bob

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "Source", 6, false)

	var mobileLayoutID, rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	seedIntegration(t, d, "int-radar", "radarr")
	grantIntegrationUse(t, d, "user-alice", "int-radar")

	containerID, clockID := "sec-container-1", "item-clock-1"
	sections := fmt.Sprintf(`[
		{"id":%q,"kind":"empty","xOffset":0,"yOffset":0},
		{"id":%q,"kind":"container","options":{"title":"Box","collapsible":true},"layouts":[{"layoutId":%q,"parentSectionId":%q,"xOffset":3,"yOffset":0,"width":3,"height":2}]}
	]`, rootSectionID, containerID, baseLayoutID, rootSectionID)
	items := fmt.Sprintf(`[
		{"id":%q,"kind":"clock","options":{"timeZone":"utc"},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":["int-radar"]}
	]`, clockID, baseLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID, sections, items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Bob collapses the container: the per-user collapse state must be cloned
	// with the new section id.
	status, body = call(t, srv, bobName, http.MethodPost, "/api/trpc/section.changeCollapsed",
		superjsonEnvelope(fmt.Sprintf(`{"sectionId":%q,"collapsed":true}`, containerID)))
	if status != http.StatusOK {
		t.Fatalf("changeCollapsed status %d body %s", status, body)
	}

	// Guards.
	expectError(t, srv, "", http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(`{"id":"x","name":"Copy"}`), "UNAUTHORIZED")
	// Bob is signed in but lacks board-create: FORBIDDEN before the board
	// access check.
	if message := expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Copy"}`, boardID)), "FORBIDDEN"); message != "Permission denied" {
		t.Fatalf("permission message = %q", message)
	}
	// With the permission but without view access: anti-snooping NOT_FOUND,
	// for unknown boards and inaccessible ones alike.
	grantBoardCreate(t, d, "user-bob")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(`{"id":"board-does-not-exist","name":"Copy"}`), "NOT_FOUND")
	expectError(t, srv, bobName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Copy"}`, boardID)), "NOT_FOUND")
	// Invalid and conflicting names.
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"No Spaces!"}`, boardID)), "BAD_REQUEST"); message != "invalid board name" {
		t.Fatalf("invalid name message = %q", message)
	}
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"SOURCE"}`, boardID)), "CONFLICT"); message != "Board with similar name already exists" {
		t.Fatalf("conflict message = %q", message)
	}

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Copy"}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("duplicateBoard status %d body %s", status, body)
	}
	var out struct {
		BoardID string `json:"boardId"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode duplicateBoard output: %v (%s)", err, body)
	}
	newBoardID := out.BoardID
	if newBoardID == "" || newBoardID == boardID {
		t.Fatalf("boardId = %q", newBoardID)
	}

	// Board row: name from the input, creator the caller, the remaining
	// columns copied.
	var name, creatorID string
	var isPublic int
	if err := d.QueryRow(`SELECT name, is_public, creator_id FROM "board" WHERE id = ?`, newBoardID).
		Scan(&name, &isPublic, &creatorID); err != nil {
		t.Fatalf("read duplicate board: %v", err)
	}
	if name != "Copy" || isPublic != 0 || creatorID != "user-alice" {
		t.Fatalf("duplicate board row = %s public %d creator %s", name, isPublic, creatorID)
	}

	// Layouts: same roles/breakpoints/column counts, fresh ids, no resize
	// history.
	var newMobileID, newBaseID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, newBoardID).Scan(&newMobileID); err != nil {
		t.Fatalf("read duplicate mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'base'`, newBoardID).Scan(&newBaseID); err != nil {
		t.Fatalf("read duplicate base layout: %v", err)
	}
	if newMobileID == mobileLayoutID || newBaseID == baseLayoutID {
		t.Fatal("layout ids not regenerated")
	}
	var columnCount int
	var breakpoint int
	if err := d.QueryRow(`SELECT column_count, breakpoint FROM "layout" WHERE id = ?`, newBaseID).Scan(&columnCount, &breakpoint); err != nil {
		t.Fatalf("read duplicate base layout: %v", err)
	}
	if columnCount != 6 || breakpoint != 768 {
		t.Fatalf("duplicate base layout = %d cols bp %d", columnCount, breakpoint)
	}
	if got := layoutResizeSource(t, d, newBaseID); got != "" {
		t.Fatalf("duplicate resize source = %s, want NULL", got)
	}

	// Sections, placements, items, relations and collapse states cloned with
	// fresh ids.
	var sectionCount int
	var containerOptions string
	var newContainerID, newRootID string
	if err := d.QueryRow(`SELECT COUNT(*) FROM "section" WHERE board_id = ?`, newBoardID).Scan(&sectionCount); err != nil {
		t.Fatalf("count sections: %v", err)
	}
	if sectionCount != 2 {
		t.Fatalf("sections = %d, want 2", sectionCount)
	}
	rows, err := d.Query(`SELECT id, kind, options FROM "section" WHERE board_id = ? ORDER BY kind`, newBoardID)
	if err != nil {
		t.Fatalf("read duplicate sections: %v", err)
	}
	for rows.Next() {
		var id, kind string
		var options []byte
		if err := rows.Scan(&id, &kind, &options); err != nil {
			t.Fatalf("scan duplicate sections: %v", err)
		}
		if kind == "container" {
			newContainerID = id
			containerOptions = string(options)
		} else {
			newRootID = id
		}
	}
	rows.Close()
	if newContainerID == containerID || newRootID == rootSectionID || !strings.Contains(containerOptions, `"title":"Box"`) {
		t.Fatalf("duplicate sections = root %q container %q options %s", newRootID, newContainerID, containerOptions)
	}
	var placementCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "section_layout" WHERE section_id = ?`, newContainerID).Scan(&placementCount); err != nil {
		t.Fatalf("count section layouts: %v", err)
	}
	if placementCount != 1 {
		t.Fatalf("section layouts = %d, want 1", placementCount)
	}

	var itemCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "item" WHERE board_id = ?`, newBoardID).Scan(&itemCount); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if itemCount != 1 {
		t.Fatalf("items = %d, want 1", itemCount)
	}
	var newItemID, storedOptions string
	if err := d.QueryRow(`SELECT id, options FROM "item" WHERE board_id = ?`, newBoardID).Scan(&newItemID, &storedOptions); err != nil {
		t.Fatalf("read duplicate item: %v", err)
	}
	if newItemID == clockID || !strings.Contains(storedOptions, `"timeZone":"utc"`) {
		t.Fatalf("duplicate item = %q options %s", newItemID, storedOptions)
	}

	var relationCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "integration_item" WHERE item_id = ?`, newItemID).Scan(&relationCount); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if relationCount != 1 {
		t.Fatalf("integration relations = %d, want 1", relationCount)
	}

	// Bob's collapse state follows the new section id.
	var collapseCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "section_collapse_state" WHERE section_id = ? AND user_id = 'user-bob' AND collapsed = 1`, newContainerID).
		Scan(&collapseCount); err != nil {
		t.Fatalf("count collapse states: %v", err)
	}
	if collapseCount != 1 {
		t.Fatalf("collapse states = %d, want 1", collapseCount)
	}

	// The original board is untouched.
	var originalItems int
	if err := d.QueryRow(`SELECT COUNT(*) FROM "item" WHERE board_id = ?`, boardID).Scan(&originalItems); err != nil {
		t.Fatalf("count original items: %v", err)
	}
	if originalItems != 1 {
		t.Fatalf("original items = %d", originalItems)
	}
}

// TestDuplicateBoardSingleLayoutGeneratesMobile verifies the legacy path: a
// board with a single layout gets a generated Mobile layout whose placements
// are the Base projection.
func TestDuplicateBoardSingleLayoutGeneratesMobile(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "LegacyBoard", 8, false)

	var mobileLayoutID, rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	items := fmt.Sprintf(`[
		{"id":"item-a","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[]},
		{"id":"item-b","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":0,"width":3,"height":1}],"integrationIds":[]},
		{"id":"item-c","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":5,"yOffset":0,"width":3,"height":1}],"integrationIds":[]}
	]`, baseLayoutID, rootSectionID, baseLayoutID, rootSectionID, baseLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID), items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Simulate the legacy pre-migration board: only the Base layout remains.
	if _, err := d.Exec(`DELETE FROM "item_layout" WHERE layout_id = ?`, mobileLayoutID); err != nil {
		t.Fatalf("delete mobile placements: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM "layout" WHERE id = ?`, mobileLayoutID); err != nil {
		t.Fatalf("delete mobile layout: %v", err)
	}

	status, body = call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"LegacyCopy"}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("duplicateBoard status %d body %s", status, body)
	}
	var out struct {
		BoardID string `json:"boardId"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode duplicateBoard output: %v (%s)", err, body)
	}

	var newMobileID, newBaseID string
	var mobileColumns, baseColumns int
	err := d.QueryRow(`SELECT id, column_count FROM "layout" WHERE board_id = ? AND role = 'mobile'`, out.BoardID).Scan(&newMobileID, &mobileColumns)
	if err != nil {
		t.Fatalf("read generated mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id, column_count FROM "layout" WHERE board_id = ? AND role = 'base'`, out.BoardID).Scan(&newBaseID, &baseColumns); err != nil {
		t.Fatalf("read copied base layout: %v", err)
	}
	if mobileColumns != 3 || baseColumns != 8 || newMobileID == newBaseID {
		t.Fatalf("generated layouts = mobile %d cols, base %d cols", mobileColumns, baseColumns)
	}

	// The generated Mobile layout carries the projection of the Base
	// placements (8 -> 3 columns). The duplicated items have fresh ids, so
	// the placements are compared as a position set.
	placements := map[[4]int]bool{}
	for _, row := range readItemPlacements(t, d, newMobileID) {
		placements[[4]int{row.xOffset, row.yOffset, row.width, row.height}] = true
	}
	want := map[[4]int]bool{
		{0, 0, 2, 1}: true,
		{0, 1, 3, 1}: true,
		{0, 2, 3, 1}: true,
	}
	if len(placements) != len(want) {
		t.Fatalf("mobile placements = %+v, want %+v", placements, want)
	}
	for position := range want {
		if !placements[position] {
			t.Fatalf("mobile placements = %+v, missing %+v", placements, position)
		}
	}
	// The copied Base keeps its placements.
	if baseRows := readItemPlacements(t, d, newBaseID); len(baseRows) != 3 {
		t.Fatalf("base placements = %d rows, want 3", len(baseRows))
	}
}

// TestDuplicateBoardCustomWidgetGuard verifies the Custom Widget duplication
// guard: the board carries a Custom Widget placed by the administrator, and a
// non-admin with view access may not duplicate it.
func TestDuplicateBoardCustomWidgetGuard(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	root := seedUser(t, d, testUser{id: "user-root", name: rootName, admin: true})
	sessions[aliceName], sessions[rootName] = alice, root

	boardID, baseLayoutID := mustCreateBoard(t, srv, rootName, "WidgetBoard", 6, false)

	var rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}
	items := fmt.Sprintf(`[{"id":"item-custom","kind":"customApi","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":3,"height":2}],"integrationIds":[]}]`,
		baseLayoutID, rootSectionID)
	status, body := call(t, srv, rootName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID), items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Alice has board-create and view access to the board but is not an
	// administrator.
	grantBoardCreate(t, d, "user-alice")
	seedBoardUserPermission(t, d, boardID, "user-alice", "view")
	if message := expectError(t, srv, aliceName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"Copy"}`, boardID)), "FORBIDDEN"); message != "Only administrators can add or configure Custom Widgets" {
		t.Fatalf("custom widget message = %q", message)
	}

	// The instance administrator may duplicate it.
	status, body = call(t, srv, rootName, http.MethodPost, "/api/trpc/board.duplicateBoard",
		superjsonEnvelope(fmt.Sprintf(`{"id":%q,"name":"AdminCopy"}`, boardID)))
	if status != http.StatusOK {
		t.Fatalf("duplicateBoard as admin status %d body %s", status, body)
	}
}

// TestProtectedLayoutRepairProjectsPositions verifies that the lazy repair of
// protected layouts (getFullBoardWithWhereAsync, board.ts:3001-3011) seeds
// the missing Mobile layout with the projected Base positions
// (insertMissingProjectedPositionsAsync, seed.ts:209-303) instead of leaving
// it empty.
func TestProtectedLayoutRepairProjectsPositions(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	alice := seedUser(t, d, testUser{id: "user-alice", name: aliceName})
	sessions[aliceName] = alice

	grantBoardCreate(t, d, "user-alice")
	boardID, baseLayoutID := mustCreateBoard(t, srv, aliceName, "RepairBoard", 8, false)

	var mobileLayoutID, rootSectionID string
	if err := d.QueryRow(`SELECT id FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).Scan(&mobileLayoutID); err != nil {
		t.Fatalf("read mobile layout: %v", err)
	}
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&rootSectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	items := fmt.Sprintf(`[
		{"id":"item-a","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":0,"yOffset":0,"width":2,"height":1}],"integrationIds":[]},
		{"id":"item-b","kind":"clock","options":{},"layouts":[{"layoutId":%q,"sectionId":%q,"xOffset":2,"yOffset":0,"width":3,"height":1}],"integrationIds":[]}
	]`, baseLayoutID, rootSectionID, baseLayoutID, rootSectionID)
	status, body := call(t, srv, aliceName, http.MethodPost, "/api/trpc/board.saveBoard",
		superjsonEnvelope(saveBoardInput2(boardID,
			fmt.Sprintf(`[{"id":%q,"kind":"empty","xOffset":0,"yOffset":0}]`, rootSectionID), items)))
	if status != http.StatusOK {
		t.Fatalf("saveBoard status %d body %s", status, body)
	}

	// Simulate the legacy board: drop the Mobile layout and its placements.
	if _, err := d.Exec(`DELETE FROM "item_layout" WHERE layout_id = ?`, mobileLayoutID); err != nil {
		t.Fatalf("delete mobile placements: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM "layout" WHERE id = ?`, mobileLayoutID); err != nil {
		t.Fatalf("delete mobile layout: %v", err)
	}

	// Any full-board read repairs the layouts.
	status, body = call(t, srv, aliceName, http.MethodGet,
		`/api/trpc/board.getBoardByName`+encodedInput(`{"name":"RepairBoard"}`), "")
	if status != http.StatusOK {
		t.Fatalf("getBoardByName status %d body %s", status, body)
	}

	var newMobileID string
	var columnCount int
	if err := d.QueryRow(`SELECT id, column_count FROM "layout" WHERE board_id = ? AND role = 'mobile'`, boardID).
		Scan(&newMobileID, &columnCount); err != nil {
		t.Fatalf("read repaired mobile layout: %v", err)
	}
	if newMobileID == mobileLayoutID || columnCount != 3 {
		t.Fatalf("repaired mobile layout = %q with %d columns", newMobileID, columnCount)
	}
	// The repaired Mobile layout carries the projected positions (8 -> 3
	// columns, packing).
	placements := map[[4]int]bool{}
	for _, row := range readItemPlacements(t, d, newMobileID) {
		placements[[4]int{row.xOffset, row.yOffset, row.width, row.height}] = true
	}
	want := map[[4]int]bool{
		{0, 0, 2, 1}: true,
		{0, 1, 3, 1}: true,
	}
	if len(placements) != len(want) {
		t.Fatalf("repaired mobile placements = %+v, want %+v", placements, want)
	}
	for position := range want {
		if !placements[position] {
			t.Fatalf("repaired mobile placements = %+v, missing %+v", placements, position)
		}
	}
}
