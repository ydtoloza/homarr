package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Item maps to the upstream "item" table (drizzle schema items): a widget or
// app instance placed on a board. Options and AdvancedOptions are the raw
// superjson-serialized strings ('{"json": {...}}'); the board router unwraps
// them.
type Item struct {
	ID              string
	BoardID         string
	Kind            string
	Options         string
	AdvancedOptions string
}

const itemColumns = `id, board_id, kind, options, advanced_options`

func scanItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var i Item
		if err := rows.Scan(&i.ID, &i.BoardID, &i.Kind, &i.Options, &i.AdvancedOptions); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// InsertItem inserts an item (saveBoard addedItems / addItem).
func (d *DB) InsertItem(ctx context.Context, i Item) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "item" (id, board_id, kind, options, advanced_options)
		VALUES (?, ?, ?, ?, ?)`, i.ID, i.BoardID, i.Kind, i.Options, i.AdvancedOptions)
	if err != nil {
		return fmt.Errorf("db: insert item: %w", err)
	}
	return nil
}

// UpdateItemContent applies the saveBoard item update (kind, options and
// advancedOptions; packages/api/src/router/board.ts:1833-1841).
func (d *DB) UpdateItemContent(ctx context.Context, i Item) error {
	_, err := d.ExecContext(ctx, `UPDATE "item" SET kind = ?, options = ?, advanced_options = ? WHERE id = ?`,
		i.Kind, i.Options, i.AdvancedOptions, i.ID)
	if err != nil {
		return fmt.Errorf("db: update item content: %w", err)
	}
	return nil
}

// DeleteItemsByIDs deletes items (saveBoard removedItems). Child item_layout
// rows are not cascaded (foreign keys off, see package doc) exactly like
// upstream.
func (d *DB) DeleteItemsByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM "item" WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("db: delete items: %w", err)
	}
	return nil
}

// ListItemsByBoard returns every item of a board.
func (d *DB) ListItemsByBoard(ctx context.Context, boardID string) ([]Item, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+itemColumns+` FROM "item" WHERE board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list items by board: %w", err)
	}
	return scanItems(rows)
}

// GetItemByID returns one item (userItemState itemStateProcedure).
func (d *DB) GetItemByID(ctx context.Context, id string) (Item, error) {
	var i Item
	err := d.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM "item" WHERE id = ?`, id).
		Scan(&i.ID, &i.BoardID, &i.Kind, &i.Options, &i.AdvancedOptions)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("db: get item by id: %w", err)
	}
	return i, nil
}

// --- item placements ("item_layout") -----------------------------------------

// ItemLayout is one row of the "item_layout" table: where an item sits inside
// one layout of its board. The primary key is (item_id, section_id,
// layout_id).
type ItemLayout struct {
	ItemID    string
	SectionID string
	LayoutID  string
	XOffset   int64
	YOffset   int64
	Width     int64
	Height    int64
}

// InsertItemLayout inserts one placement row.
func (d *DB) InsertItemLayout(ctx context.Context, l ItemLayout) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
		x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.ItemID, l.SectionID, l.LayoutID, l.XOffset, l.YOffset, l.Width, l.Height)
	if err != nil {
		return fmt.Errorf("db: insert item layout: %w", err)
	}
	return nil
}

// ListItemLayoutsByBoard returns every placement of a board's items, joined
// over item so callers can map placements per item and per layout
// (getElementsForLayout, packages/api/src/router/board.ts:2826-2864).
func (d *DB) ListItemLayoutsByBoard(ctx context.Context, boardID string) ([]ItemLayout, error) {
	rows, err := d.QueryContext(ctx, `SELECT il.item_id, il.section_id, il.layout_id,
		il.x_offset, il.y_offset, il.width, il.height
		FROM "item_layout" il
		JOIN "item" i ON i.id = il.item_id
		WHERE i.board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list item layouts by board: %w", err)
	}
	defer rows.Close()
	var out []ItemLayout
	for rows.Next() {
		var l ItemLayout
		if err := rows.Scan(&l.ItemID, &l.SectionID, &l.LayoutID, &l.XOffset, &l.YOffset, &l.Width, &l.Height); err != nil {
			return nil, fmt.Errorf("db: list item layouts by board: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateItemLayoutByItemAndLayout is the saveBoard item placement update:
// the WHERE clause matches (itemId, layoutId) only
// (packages/api/src/router/board.ts:1843-1859).
func (d *DB) UpdateItemLayoutByItemAndLayout(ctx context.Context, l ItemLayout) error {
	_, err := d.ExecContext(ctx, `UPDATE "item_layout" SET x_offset = ?, y_offset = ?, width = ?, height = ?,
		section_id = ? WHERE item_id = ? AND layout_id = ?`,
		l.XOffset, l.YOffset, l.Width, l.Height, l.SectionID, l.ItemID, l.LayoutID)
	if err != nil {
		return fmt.Errorf("db: update item layout: %w", err)
	}
	return nil
}

// UpdateItemLayoutPosition is the updateItemLayout write: position changes
// within the same section (packages/api/src/router/board.ts:2466-2480).
func (d *DB) UpdateItemLayoutPosition(ctx context.Context, itemID, layoutID, sectionID string, xOffset, yOffset, width, height int64) error {
	_, err := d.ExecContext(ctx, `UPDATE "item_layout" SET x_offset = ?, y_offset = ?, width = ?, height = ?
		WHERE item_id = ? AND layout_id = ? AND section_id = ?`,
		xOffset, yOffset, width, height, itemID, layoutID, sectionID)
	if err != nil {
		return fmt.Errorf("db: update item layout position: %w", err)
	}
	return nil
}

// --- container placements ("section_layout") ---------------------------------

// SectionLayout is one row of the "section_layout" table: where a container
// section sits inside one layout. parentSectionID is empty for NULL.
type SectionLayout struct {
	SectionID       string
	LayoutID        string
	ParentSectionID string // empty = NULL
	XOffset         int64
	YOffset         int64
	Width           int64
	Height          int64
}

// InsertSectionLayout inserts one container placement row.
func (d *DB) InsertSectionLayout(ctx context.Context, l SectionLayout) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
		x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.SectionID, l.LayoutID, nullableString(l.ParentSectionID), l.XOffset, l.YOffset, l.Width, l.Height)
	if err != nil {
		return fmt.Errorf("db: insert section layout: %w", err)
	}
	return nil
}

// ListSectionLayoutsByBoard returns every container placement of a board.
func (d *DB) ListSectionLayoutsByBoard(ctx context.Context, boardID string) ([]SectionLayout, error) {
	rows, err := d.QueryContext(ctx, `SELECT sl.section_id, sl.layout_id, sl.parent_section_id,
		sl.x_offset, sl.y_offset, sl.width, sl.height
		FROM "section_layout" sl
		JOIN "section" s ON s.id = sl.section_id
		WHERE s.board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list section layouts by board: %w", err)
	}
	defer rows.Close()
	var out []SectionLayout
	for rows.Next() {
		var (
			l      SectionLayout
			parent sql.NullString
		)
		if err := rows.Scan(&l.SectionID, &l.LayoutID, &parent, &l.XOffset, &l.YOffset, &l.Width, &l.Height); err != nil {
			return nil, fmt.Errorf("db: list section layouts by board: %w", err)
		}
		l.ParentSectionID = parent.String
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateSectionLayoutBySectionAndLayout is the saveBoard container placement
// update (packages/api/src/router/board.ts:1875-1891).
func (d *DB) UpdateSectionLayoutBySectionAndLayout(ctx context.Context, l SectionLayout) error {
	_, err := d.ExecContext(ctx, `UPDATE "section_layout" SET x_offset = ?, y_offset = ?, width = ?, height = ?,
		parent_section_id = ? WHERE section_id = ? AND layout_id = ?`,
		l.XOffset, l.YOffset, l.Width, l.Height, nullableString(l.ParentSectionID), l.SectionID, l.LayoutID)
	if err != nil {
		return fmt.Errorf("db: update section layout: %w", err)
	}
	return nil
}

// --- item ↔ integration relations ("integration_item") -----------------------

// IntegrationItem is one row of the "integration_item" table.
type IntegrationItem struct {
	ItemID        string
	IntegrationID string
}

// ListIntegrationItemsByBoard returns every item-integration relation of a
// board (the integrations relation of items in getFullBoardWithWhereAsync).
func (d *DB) ListIntegrationItemsByBoard(ctx context.Context, boardID string) ([]IntegrationItem, error) {
	rows, err := d.QueryContext(ctx, `SELECT ii.item_id, ii.integration_id
		FROM "integration_item" ii
		JOIN "item" i ON i.id = ii.item_id
		WHERE i.board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list integration items by board: %w", err)
	}
	defer rows.Close()
	var out []IntegrationItem
	for rows.Next() {
		var r IntegrationItem
		if err := rows.Scan(&r.ItemID, &r.IntegrationID); err != nil {
			return nil, fmt.Errorf("db: list integration items by board: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertIntegrationItem links an item to an integration.
func (d *DB) InsertIntegrationItem(ctx context.Context, r IntegrationItem) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "integration_item" (item_id, integration_id) VALUES (?, ?)`,
		r.ItemID, r.IntegrationID)
	if err != nil {
		return fmt.Errorf("db: insert integration item: %w", err)
	}
	return nil
}

// DeleteIntegrationItem removes one item-integration relation.
func (d *DB) DeleteIntegrationItem(ctx context.Context, itemID, integrationID string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM "integration_item" WHERE item_id = ? AND integration_id = ?`,
		itemID, integrationID)
	if err != nil {
		return fmt.Errorf("db: delete integration item: %w", err)
	}
	return nil
}

// IntegrationKinds returns the kind of each existing integration id (missing
// ids are absent from the map) — validateWidgetConfigurationsAsync's
// integrationRecords query (packages/api/src/router/board.ts:180-184).
func (d *DB) IntegrationKinds(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, `SELECT id, kind FROM "integration" WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: integration kinds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, fmt.Errorf("db: integration kinds: %w", err)
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// IntegrationUserPermissionsForUser returns the direct integration
// permissions of a user ("integrationUserPermission").
func (d *DB) IntegrationUserPermissionsForUser(ctx context.Context, userID string) (map[string][]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT integration_id, permission FROM "integrationUserPermission"
		WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: integration user permissions: %w", err)
	}
	return scanIntegrationPermissions(rows)
}

// IntegrationGroupPermissionsForUser returns the integration permissions of
// the groups a user belongs to ("integrationGroupPermissions" joined over
// "groupMember").
func (d *DB) IntegrationGroupPermissionsForUser(ctx context.Context, userID string) (map[string][]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT igp.integration_id, igp.permission
		FROM "integrationGroupPermissions" igp
		JOIN "groupMember" gm ON gm.group_id = igp.group_id
		WHERE gm.user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: integration group permissions: %w", err)
	}
	return scanIntegrationPermissions(rows)
}

func scanIntegrationPermissions(rows *sql.Rows) (map[string][]string, error) {
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, permission string
		if err := rows.Scan(&id, &permission); err != nil {
			return nil, err
		}
		out[id] = append(out[id], permission)
	}
	return out, rows.Err()
}
