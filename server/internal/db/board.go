package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Board maps to the upstream "board" table (drizzle schema boards). Only the
// columns needed for the M1 board list are loaded.
type Board struct {
	ID   string
	Name string
	// IsPublic is the is_public 0/1 column.
	IsPublic bool
	// CreatorID is empty for NULL (board without a creator).
	CreatorID string
	// PageTitle is empty for NULL.
	PageTitle string
}

// CreateBoard inserts a board. The id is caller-supplied (upstream generates
// cuid2 ids in application code); every other column keeps its upstream
// default (colors, opacity, background image settings, ...).
func (d *DB) CreateBoard(ctx context.Context, b Board) error {
	const query = `INSERT INTO "board" (id, name, is_public, creator_id, page_title) VALUES (?, ?, ?, ?, ?)`
	var creatorID, pageTitle any
	if b.CreatorID != "" {
		creatorID = b.CreatorID
	}
	if b.PageTitle != "" {
		pageTitle = b.PageTitle
	}
	_, err := d.ExecContext(ctx, query, b.ID, b.Name, boolToInt(b.IsPublic), creatorID, pageTitle)
	if err != nil {
		return fmt.Errorf("db: create board: %w", err)
	}
	return nil
}

// ListBoardsForUser returns the boards the user can access, ordered by name:
// public boards, boards created by the user, boards with a direct user
// permission ("boardUserPermission") and boards shared with a group the user
// belongs to ("boardGroupPermission" joined over "groupMember"). This mirrors
// the upstream visibility filter in packages/api/src/router/board.ts
// (or(eq(boards.isPublic, true), eq(boards.creatorId, userId),
// inArray(boards.id, groupBoardIds)), extended with the direct user
// permission table that the access-control callbacks also consult).
func (d *DB) ListBoardsForUser(ctx context.Context, userID string) ([]Board, error) {
	const query = `SELECT id, name, is_public, creator_id, page_title FROM "board"
		WHERE is_public = 1
		   OR creator_id = ?
		   OR id IN (SELECT board_id FROM "boardUserPermission" WHERE user_id = ?)
		   OR id IN (
		       SELECT bgp.board_id FROM "boardGroupPermission" bgp
		       JOIN "groupMember" gm ON gm.group_id = bgp.group_id
		       WHERE gm.user_id = ?)
		ORDER BY name`
	rows, err := d.QueryContext(ctx, query, userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list boards for user: %w", err)
	}
	defer rows.Close()

	var boards []Board
	for rows.Next() {
		var (
			b         Board
			isPublic  int
			creatorID sql.NullString
			pageTitle sql.NullString
		)
		if err := rows.Scan(&b.ID, &b.Name, &isPublic, &creatorID, &pageTitle); err != nil {
			return nil, fmt.Errorf("db: list boards for user: %w", err)
		}
		b.IsPublic = isPublic != 0
		b.CreatorID = creatorID.String
		b.PageTitle = pageTitle.String
		boards = append(boards, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list boards for user: %w", err)
	}
	return boards, nil
}

// GetBoardByID returns a single board.
func (d *DB) GetBoardByID(ctx context.Context, id string) (Board, error) {
	const query = `SELECT id, name, is_public, creator_id, page_title FROM "board" WHERE id = ?`
	var (
		b         Board
		isPublic  int
		creatorID sql.NullString
		pageTitle sql.NullString
	)
	err := d.QueryRowContext(ctx, query, id).Scan(&b.ID, &b.Name, &isPublic, &creatorID, &pageTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	if err != nil {
		return Board{}, fmt.Errorf("db: get board by id: %w", err)
	}
	b.IsPublic = isPublic != 0
	b.CreatorID = creatorID.String
	b.PageTitle = pageTitle.String
	return b, nil
}

// CountBoards returns the total number of boards (diagnostics/tests).
func (d *DB) CountBoards(ctx context.Context) (int, error) {
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM "board"`).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count boards: %w", err)
	}
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
