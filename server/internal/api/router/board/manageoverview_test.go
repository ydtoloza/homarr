package board_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// board.getManageOverview — the manage boards read-model (BOARDS.md section
// 6, un-deferred). Tests go through the shared harness: boards are created
// through the API (mustCreateBoard), the preview material is seeded directly.

type manageOverviewCard struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	LogoImageUrl *string `json:"logoImageUrl"`
	IsPublic     bool    `json:"isPublic"`
	PrimaryColor string  `json:"primaryColor"`
	Creator      *struct {
		ID    string  `json:"id"`
		Name  *string `json:"name"`
		Image *string `json:"image"`
	} `json:"creator"`
	UserPermissions  []struct{ Permission string } `json:"userPermissions"`
	GroupPermissions []struct{ Permission string } `json:"groupPermissions"`
	IsHome           bool                          `json:"isHome"`
	IsMobileHome     bool                          `json:"isMobileHome"`
	Preview          *struct {
		PrimaryColor string `json:"primaryColor"`
		Layouts      []struct {
			ID          string `json:"id"`
			ColumnCount int64  `json:"columnCount"`
			Role        string `json:"role"`
		} `json:"layouts"`
		Sections []struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			XOffset *int64 `json:"xOffset"`
			Layouts []struct {
				SectionID       string  `json:"sectionId"`
				LayoutID        string  `json:"layoutId"`
				ParentSectionID *string `json:"parentSectionId"`
				XOffset         int64   `json:"xOffset"`
				YOffset         int64   `json:"yOffset"`
				Width           int64   `json:"width"`
				Height          int64   `json:"height"`
			} `json:"layouts"`
		} `json:"sections"`
		Items []struct {
			ID      string  `json:"id"`
			Kind    string  `json:"kind"`
			IconURL *string `json:"iconUrl"`
			Layouts []struct {
				ItemID    string `json:"itemId"`
				LayoutID  string `json:"layoutId"`
				SectionID string `json:"sectionId"`
				XOffset   int64  `json:"xOffset"`
				YOffset   int64  `json:"yOffset"`
				Width     int64  `json:"width"`
				Height    int64  `json:"height"`
			} `json:"layouts"`
		} `json:"items"`
	} `json:"preview"`
}

func getManageOverview(t *testing.T, srv *httptest.Server, user string, input string) (int, []manageOverviewCard, string) {
	t.Helper()
	path := "/api/trpc/board.getManageOverview"
	if input != "" {
		path += encodedInput(input)
	}
	status, body := call(t, srv, user, http.MethodGet, path, "")
	var cards []manageOverviewCard
	if status == http.StatusOK {
		if err := json.Unmarshal(unwrapData(t, body), &cards); err != nil {
			t.Fatalf("decode cards: %v (%s)", err, body)
		}
	}
	return status, cards, body
}

// seedItemWithLayout inserts an item row plus its base-layout placement.
func seedItemWithLayout(t *testing.T, d *db.DB, itemID, boardID, sectionID, layoutID, kind, options string, xOffset, yOffset, width, height int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `INSERT INTO "item" (id, board_id, kind, options, advanced_options)
		VALUES (?, ?, ?, ?, '{"json": {}}')`,
		itemID, boardID, kind, options); err != nil {
		t.Fatalf("seed item %s: %v", itemID, err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id, x_offset, y_offset, width, height)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		itemID, sectionID, layoutID, xOffset, yOffset, width, height); err != nil {
		t.Fatalf("seed item layout %s: %v", itemID, err)
	}
}

// The full happy path: boards of the caller with the preview material.
func TestGetManageOverviewShape(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, testUser{id: "alice", name: "alice"})
	grantBoardCreate(t, d, "alice")

	boardID, layoutID := mustCreateBoard(t, srv, "alice", "my-board", 12, true)

	// The root empty section createBoard seeded.
	var sectionID string
	if err := d.QueryRow(`SELECT id FROM "section" WHERE board_id = ? AND kind = 'empty'`, boardID).Scan(&sectionID); err != nil {
		t.Fatalf("read root section: %v", err)
	}

	// An app + an app widget inside the preview rows, and one outside.
	appID := "app-1"
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO "app" (id, name, icon_url) VALUES (?, 'My App', 'https://icon.example/1.svg')`, appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	seedItemWithLayout(t, d, "item-in", boardID, sectionID, layoutID, "app", `{"json":{"appId":"`+appID+`"}}`, 0, 3, 4, 2)
	seedItemWithLayout(t, d, "item-out", boardID, sectionID, layoutID, "app", `{"json":{"appId":"`+appID+`"}}`, 0, 30, 4, 2)

	// The home board flag (board.setHomeBoard needs view access; seed the
	// column like the procedure would).
	if _, err := d.ExecContext(context.Background(), `UPDATE "user" SET home_board_id = ? WHERE id = 'alice'`, boardID); err != nil {
		t.Fatalf("seed home board: %v", err)
	}

	status, cards, body := getManageOverview(t, srv, "alice", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %d (%s), want 1", len(cards), body)
	}
	card := cards[0]
	if card.ID != boardID || card.Name != "my-board" || !card.IsPublic {
		t.Errorf("card = %+v", card)
	}
	if card.Creator == nil || card.Creator.ID != "alice" {
		t.Errorf("creator = %+v", card.Creator)
	}
	if !card.IsHome || card.IsMobileHome {
		t.Errorf("home flags = %+v", card)
	}
	if card.Preview == nil {
		t.Fatalf("preview = nil (%s)", body)
	}
	if len(card.Preview.Layouts) != 1 || card.Preview.Layouts[0].ID != layoutID || card.Preview.Layouts[0].Role != "base" {
		t.Errorf("preview layouts = %+v", card.Preview.Layouts)
	}
	if len(card.Preview.Sections) != 1 || card.Preview.Sections[0].Kind != "empty" || card.Preview.Sections[0].XOffset == nil || *card.Preview.Sections[0].XOffset != 0 {
		t.Errorf("preview sections = %+v", card.Preview.Sections)
	}
	// Only the item inside the 12-row preview limit, with its app icon.
	if len(card.Preview.Items) != 1 {
		t.Fatalf("preview items = %+v (%s)", card.Preview.Items, body)
	}
	item := card.Preview.Items[0]
	if item.ID != "item-in" || item.Kind != "app" || item.IconURL == nil || *item.IconURL != "https://icon.example/1.svg" {
		t.Errorf("item = %+v", item)
	}
	if len(item.Layouts) != 1 || item.Layouts[0].YOffset != 3 || item.Layouts[0].LayoutID != layoutID || item.Layouts[0].SectionID != sectionID {
		t.Errorf("item layout = %+v", item.Layouts)
	}

	// fullPreview lifts the row limit to 48: the second item appears.
	status, cards, body = getManageOverview(t, srv, "alice", `{"fullPreview":true}`)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(cards) != 1 || cards[0].Preview == nil || len(cards[0].Preview.Items) != 2 {
		t.Errorf("fullPreview items = %+v (%s)", cards[0].Preview, body)
	}
}

// Access rules: a caller sees their own boards; another user's overview
// requires admin; the admin sees the TARGET user's boards and home flags.
func TestGetManageOverviewAccess(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, testUser{id: "alice", name: "alice"})
	sessions["bob"] = seedUser(t, d, testUser{id: "bob", name: "bob"})
	sessions["root"] = seedUser(t, d, testUser{id: "root", name: "root", admin: true})
	grantBoardCreate(t, d, "alice")
	grantBoardCreate(t, d, "bob")

	// Alice's private board, bob's public board.
	alicePrivate, _ := mustCreateBoard(t, srv, "alice", "alice-private", 12, false)
	bobPublic, _ := mustCreateBoard(t, srv, "bob", "bob-public", 12, true)

	// Anonymous: the public boards of the instance.
	status, cards, body := getManageOverview(t, srv, "", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(cards) != 1 || cards[0].ID != bobPublic {
		t.Errorf("anonymous cards = %+v (%s), want the public board only", cards, body)
	}

	// Alice: her own board (creator) plus bob's public one.
	status, cards, _ = getManageOverview(t, srv, "alice", "")
	if len(cards) != 2 || cards[0].ID != alicePrivate || cards[1].ID != bobPublic {
		t.Errorf("alice cards = %+v, want her private + the public board", cards)
	}

	// Alice asking for bob: FORBIDDEN.
	status, _, body = getManageOverview(t, srv, "alice", `{"userId":"bob"}`)
	if status != http.StatusForbidden || !strings.Contains(body, "You are not allowed to view boards for this user") {
		t.Errorf("alice→bob = %d %s, want FORBIDDEN", status, body)
	}

	// The administrator previews alice: her boards AND her home flags.
	if _, err := d.ExecContext(context.Background(), `UPDATE "user" SET home_board_id = ? WHERE id = 'alice'`, alicePrivate); err != nil {
		t.Fatalf("seed home: %v", err)
	}
	status, cards, _ = getManageOverview(t, srv, "root", `{"userId":"alice"}`)
	if status != http.StatusOK {
		t.Fatalf("admin status %d", status)
	}
	if len(cards) != 2 || cards[0].ID != alicePrivate || !cards[0].IsHome {
		t.Errorf("admin→alice cards = %+v, want alice's board (with her home flag) first", cards)
	}
}

// The mobile-vs-base layout selection: the base layout wins (role CASE,
// board.ts:421-428); a board without items serves an empty list, not null.
func TestGetManageOverviewBaseLayoutSelection(t *testing.T) {
	d, srv, sessions := newTestAPI(t)
	sessions["alice"] = seedUser(t, d, testUser{id: "alice", name: "alice"})
	grantBoardCreate(t, d, "alice")

	boardID, layoutID := mustCreateBoard(t, srv, "alice", "layout-board", 12, true)

	status, cards, body := getManageOverview(t, srv, "alice", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if len(cards) != 1 || cards[0].ID != boardID {
		t.Fatalf("cards = %+v (%s)", cards, body)
	}
	if cards[0].Preview == nil || len(cards[0].Preview.Layouts) != 1 {
		t.Fatalf("preview layouts = %+v", cards[0].Preview)
	}
	if cards[0].Preview.Layouts[0].ID != layoutID {
		t.Errorf("layout = %s, want the base layout %s", cards[0].Preview.Layouts[0].ID, layoutID)
	}
	if cards[0].Preview.Layouts[0].Role != "base" {
		t.Errorf("role = %s, want base", cards[0].Preview.Layouts[0].Role)
	}
	// No items: empty list, not null.
	if cards[0].Preview.Items == nil || len(cards[0].Preview.Items) != 0 {
		t.Errorf("items = %+v, want []", cards[0].Preview.Items)
	}
	// previewRowLimit validation: 0 and 49 fail the zod range.
	status, _, body = getManageOverview(t, srv, "alice", `{"previewRowLimit":49}`)
	if status != http.StatusBadRequest {
		t.Errorf("previewRowLimit 49 = %d %s, want 400", status, body)
	}
}
