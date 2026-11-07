package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// Layout maps to the upstream "layout" table (drizzle schema layouts,
// packages/db/schema/sqlite.ts). A board owns a Mobile layout (breakpoint 0),
// a Base layout (breakpoint 768) and optional custom layouts.
type Layout struct {
	ID          string
	Name        string
	BoardID     string
	ColumnCount int64
	// LeftGutterColumnCount / RightGutterColumnCount are the responsive
	// gutter columns added by migration 0043.
	LeftGutterColumnCount  int64
	RightGutterColumnCount int64
	Breakpoint             int64
	// Role is "mobile" | "base" | "custom".
	Role string
	// ResizeSource is empty for NULL (layout_resize_source, migration 0048).
	ResizeSource string
}

const layoutColumns = `id, name, board_id, column_count, left_gutter_column_count, right_gutter_column_count,
	breakpoint, role, resize_source`

func scanLayouts(rows *sql.Rows) ([]Layout, error) {
	defer rows.Close()
	var out []Layout
	for rows.Next() {
		var (
			l         Layout
			resizeSrc sql.NullString
		)
		if err := rows.Scan(&l.ID, &l.Name, &l.BoardID, &l.ColumnCount, &l.LeftGutterColumnCount,
			&l.RightGutterColumnCount, &l.Breakpoint, &l.Role, &resizeSrc); err != nil {
			return nil, err
		}
		l.ResizeSource = resizeSrc.String
		out = append(out, l)
	}
	return out, rows.Err()
}

// InsertLayout inserts one layout row (caller supplies the id).
func (d *DB) InsertLayout(ctx context.Context, l Layout) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "layout" (id, name, board_id, column_count,
		left_gutter_column_count, right_gutter_column_count, breakpoint, role, resize_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Name, l.BoardID, l.ColumnCount, l.LeftGutterColumnCount, l.RightGutterColumnCount,
		l.Breakpoint, l.Role, nullableString(l.ResizeSource))
	if err != nil {
		return fmt.Errorf("db: insert layout: %w", err)
	}
	return nil
}

// ListLayoutsByBoard returns the layouts of a board ordered by breakpoint.
// Upstream sorts in application code when rendering (getFullBoardWithWhereAsync,
// packages/api/src/router/board.ts:3017-3023); the SQL order here matches it.
func (d *DB) ListLayoutsByBoard(ctx context.Context, boardID string) ([]Layout, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+layoutColumns+` FROM "layout"
		WHERE board_id = ? ORDER BY breakpoint`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list layouts by board: %w", err)
	}
	return scanLayouts(rows)
}

// GetLayoutByID returns one layout.
func (d *DB) GetLayoutByID(ctx context.Context, id string) (Layout, error) {
	var (
		l         Layout
		resizeSrc sql.NullString
	)
	err := d.QueryRowContext(ctx, `SELECT `+layoutColumns+` FROM "layout" WHERE id = ?`, id).
		Scan(&l.ID, &l.Name, &l.BoardID, &l.ColumnCount, &l.LeftGutterColumnCount,
			&l.RightGutterColumnCount, &l.Breakpoint, &l.Role, &resizeSrc)
	if errors.Is(err, sql.ErrNoRows) {
		return Layout{}, ErrNotFound
	}
	if err != nil {
		return Layout{}, fmt.Errorf("db: get layout by id: %w", err)
	}
	l.ResizeSource = resizeSrc.String
	return l, nil
}

// UpdateLayoutRoleAndBreakpoint is the protected-layout repair write
// (seedProtectedBoardLayoutsAsync, packages/db/migrations/seed.ts:144,164-170).
func (d *DB) UpdateLayoutRoleAndBreakpoint(ctx context.Context, id string, role string, breakpoint int64) error {
	_, err := d.ExecContext(ctx, `UPDATE "layout" SET role = ?, breakpoint = ? WHERE id = ?`, role, breakpoint, id)
	if err != nil {
		return fmt.Errorf("db: update layout role/breakpoint: %w", err)
	}
	return nil
}

// SortLayoutsByBreakpoint orders layouts ascending by breakpoint like
// getFullBoardWithWhereAsync does before returning them (stable, so equal
// breakpoints keep their DB order).
func SortLayoutsByBreakpoint(layouts []Layout) {
	sort.SliceStable(layouts, func(i, j int) bool { return layouts[i].Breakpoint < layouts[j].Breakpoint })
}
