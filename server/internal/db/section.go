package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Section maps to the upstream "section" table (drizzle schema sections).
// kind is "empty" (a canvas root or lane column) or "container" (a boxed
// group). Root sections carry the lane offset in xOffset (-1 left, 0 main,
// 1 right); containers keep both offsets NULL and place themselves through
// the section_layout rows.
type Section struct {
	ID      string
	BoardID string
	Kind    string
	XOffset sql.NullInt64
	YOffset sql.NullInt64
	Name    sql.NullString
	// Options is the raw superjson-serialized options string
	// (default '{"json": {}}').
	Options sql.NullString
}

const sectionColumns = `id, board_id, kind, x_offset, y_offset, name, options`

func scanSection(row *sql.Row) (Section, error) {
	var s Section
	err := row.Scan(&s.ID, &s.BoardID, &s.Kind, &s.XOffset, &s.YOffset, &s.Name, &s.Options)
	if errors.Is(err, sql.ErrNoRows) {
		return Section{}, ErrNotFound
	}
	if err != nil {
		return Section{}, err
	}
	return s, nil
}

func scanSections(rows *sql.Rows) ([]Section, error) {
	defer rows.Close()
	var out []Section
	for rows.Next() {
		var s Section
		if err := rows.Scan(&s.ID, &s.BoardID, &s.Kind, &s.XOffset, &s.YOffset, &s.Name, &s.Options); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// InsertSection inserts one section row (caller supplies the id).
func (d *DB) InsertSection(ctx context.Context, s Section) error {
	var options any
	if s.Options.Valid {
		options = s.Options.String
	}
	_, err := d.ExecContext(ctx, `INSERT INTO "section" (id, board_id, kind, x_offset, y_offset, name, options)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.BoardID, s.Kind, s.XOffset, s.YOffset, s.Name, options)
	if err != nil {
		return fmt.Errorf("db: insert section: %w", err)
	}
	return nil
}

// ListSectionsByBoard returns every section of a board.
func (d *DB) ListSectionsByBoard(ctx context.Context, boardID string) ([]Section, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+sectionColumns+` FROM "section" WHERE board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list sections by board: %w", err)
	}
	return scanSections(rows)
}

// GetSectionByID returns one section (section.changeCollapsed).
func (d *DB) GetSectionByID(ctx context.Context, id string) (Section, error) {
	s, err := scanSection(d.QueryRowContext(ctx, `SELECT `+sectionColumns+` FROM "section" WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return Section{}, ErrNotFound
	}
	if err != nil {
		return Section{}, fmt.Errorf("db: get section by id: %w", err)
	}
	return s, nil
}

// UpdateSectionContent applies the saveBoard section update: offsets only
// exist for empty sections (containers store NULL) and the name column is
// always nulled (packages/api/src/router/board.ts:1862-1871).
func (d *DB) UpdateSectionContent(ctx context.Context, s Section) error {
	_, err := d.ExecContext(ctx, `UPDATE "section" SET x_offset = ?, y_offset = ?, options = ?, name = NULL
		WHERE id = ?`,
		s.XOffset, s.YOffset, s.Options, s.ID)
	if err != nil {
		return fmt.Errorf("db: update section content: %w", err)
	}
	return nil
}

// DeleteSectionsByIDs deletes the given sections (saveBoard removedSections).
// Like upstream, child rows are not cascaded (foreign keys off, see package
// doc).
func (d *DB) DeleteSectionsByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM "section" WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("db: delete sections: %w", err)
	}
	return nil
}

// SortEmptySectionsByOffset orders empty root sections like the board code
// does before picking the placement target: yOffset (NULL = 0) ascending,
// then id (addItem, packages/api/src/router/board.ts:2327-2332).
func SortEmptySectionsByOffset(sections []Section) {
	sort.SliceStable(sections, func(i, j int) bool {
		yi := int64(0)
		if sections[i].YOffset.Valid {
			yi = sections[i].YOffset.Int64
		}
		yj := int64(0)
		if sections[j].YOffset.Valid {
			yj = sections[j].YOffset.Int64
		}
		if yi != yj {
			return yi < yj
		}
		return sections[i].ID < sections[j].ID
	})
}

// --- section collapse state ("section_collapse_state") -----------------------

// SectionCollapseState is one row of the per-user collapse state table.
type SectionCollapseState struct {
	SectionID string
	UserID    string
	Collapsed bool
}

// ListSectionCollapseStatesForUser returns the collapse states a user has set
// (the collapseStates relation filtered by user id in
// getFullBoardWithWhereAsync, packages/api/src/router/board.ts:2963-2967).
func (d *DB) ListSectionCollapseStatesForUser(ctx context.Context, userID string) ([]SectionCollapseState, error) {
	rows, err := d.QueryContext(ctx, `SELECT section_id, user_id, collapsed FROM "section_collapse_state"
		WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list section collapse states: %w", err)
	}
	defer rows.Close()
	var out []SectionCollapseState
	for rows.Next() {
		var s SectionCollapseState
		var collapsed int
		if err := rows.Scan(&s.SectionID, &s.UserID, &collapsed); err != nil {
			return nil, fmt.Errorf("db: list section collapse states: %w", err)
		}
		s.Collapsed = collapsed != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// InsertSectionCollapseState creates the collapse state row
// (section.changeCollapsed insert branch, packages/api/src/router/section/
// section-router.ts:54-61).
func (d *DB) InsertSectionCollapseState(ctx context.Context, s SectionCollapseState) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "section_collapse_state" (section_id, user_id, collapsed)
		VALUES (?, ?, ?)`, s.SectionID, s.UserID, boolToInt(s.Collapsed))
	if err != nil {
		return fmt.Errorf("db: insert section collapse state: %w", err)
	}
	return nil
}

// UpdateSectionCollapseState updates an existing collapse state row
// (section.changeCollapsed update branch).
func (d *DB) UpdateSectionCollapseState(ctx context.Context, s SectionCollapseState) error {
	_, err := d.ExecContext(ctx, `UPDATE "section_collapse_state" SET collapsed = ?
		WHERE section_id = ? AND user_id = ?`, boolToInt(s.Collapsed), s.SectionID, s.UserID)
	if err != nil {
		return fmt.Errorf("db: update section collapse state: %w", err)
	}
	return nil
}

// HasSectionCollapseState reports whether a collapse state row exists.
func (d *DB) HasSectionCollapseState(ctx context.Context, sectionID, userID string) (bool, error) {
	var one int
	err := d.QueryRowContext(ctx, `SELECT 1 FROM "section_collapse_state" WHERE section_id = ? AND user_id = ?`,
		sectionID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: has section collapse state: %w", err)
	}
	return true, nil
}
