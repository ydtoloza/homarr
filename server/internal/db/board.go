package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// BoardIsPublic reports the is_public column of one board
// (serverSettings.updateBoardSettings validates home boards against it).
// A missing board returns ErrNotFound.
func (d *DB) BoardIsPublic(ctx context.Context, id string) (bool, error) {
	const query = `SELECT is_public FROM "board" WHERE id = ?`
	var isPublic bool
	err := d.QueryRowContext(ctx, query, id).Scan(&isPublic)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("db: board is public: %w", err)
	}
	return isPublic, nil
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

// BoardFull maps every column of the upstream "board" table (drizzle schema
// boards, packages/db/schema/sqlite.ts). Nullable text columns are "" for
// NULL; callers that must write NULL back use pointers in the dedicated
// update types below.
type BoardFull struct {
	Board
	MetaTitle                 string // empty = NULL
	LogoImageUrl              string // empty = NULL
	FaviconImageUrl           string // empty = NULL
	BackgroundImageUrl        string // empty = NULL
	BackgroundImageAttachment string
	BackgroundImageRepeat     string
	BackgroundImageSize       string
	PrimaryColor              string
	SecondaryColor            string
	Opacity                   int
	CustomCss                 string // empty = NULL
	IconColor                 string // empty = NULL
	ItemRadius                string
	DisableStatus             bool
}

// GetBoardFullByID returns every column of one board.
func (d *DB) GetBoardFullByID(ctx context.Context, id string) (BoardFull, error) {
	const query = `SELECT id, name, is_public, creator_id, page_title, meta_title, logo_image_url,
		favicon_image_url, background_image_url, background_image_attachment, background_image_repeat,
		background_image_size, primary_color, secondary_color, opacity, custom_css, icon_color,
		item_radius, disable_status
		FROM "board" WHERE id = ?`
	return scanBoardFull(d.QueryRowContext(ctx, query, id))
}

func scanBoardFull(row *sql.Row) (BoardFull, error) {
	var (
		b       BoardFull
		isPub   int
		creator sql.NullString
		nulls   [7]sql.NullString // page_title, meta_title, logo, favicon, bg url, custom css, icon color
		disable int
	)
	err := row.Scan(&b.ID, &b.Name, &isPub, &creator, &nulls[0], &nulls[1], &nulls[2], &nulls[3], &nulls[4],
		&b.BackgroundImageAttachment, &b.BackgroundImageRepeat, &b.BackgroundImageSize,
		&b.PrimaryColor, &b.SecondaryColor, &b.Opacity, &nulls[5], &nulls[6], &b.ItemRadius, &disable)
	if errors.Is(err, sql.ErrNoRows) {
		return BoardFull{}, ErrNotFound
	}
	if err != nil {
		return BoardFull{}, err
	}
	b.IsPublic = isPub != 0
	b.CreatorID = creator.String
	b.PageTitle = nulls[0].String
	b.MetaTitle = nulls[1].String
	b.LogoImageUrl = nulls[2].String
	b.FaviconImageUrl = nulls[3].String
	b.BackgroundImageUrl = nulls[4].String
	b.CustomCss = nulls[5].String
	b.IconColor = nulls[6].String
	b.DisableStatus = disable != 0
	return b, nil
}

// GetBoardIDByNameInsensitive resolves a board by case-insensitive name,
// mirroring the getBoardByName lookup (eq(sql`UPPER(boards.name)`,
// input.name.toUpperCase()), packages/api/src/router/board.ts:1205).
func (d *DB) GetBoardIDByNameInsensitive(ctx context.Context, name string) (string, error) {
	const query = `SELECT id FROM "board" WHERE UPPER(name) = UPPER(?)`
	var id string
	err := d.QueryRowContext(ctx, query, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: get board by name: %w", err)
	}
	return id, nil
}

// BoardNameTaken reports whether a board whose lowercase name equals name
// exists, ignoring the given ids. This mirrors noBoardWithSimilarNameAsync
// (packages/api/src/router/board.ts:2548-2566), which loads every board and
// compares names in application code — reproduced here for identical
// semantics (SQLite lower() is ASCII-only, like the JS toLowerCase() call).
func (d *DB) BoardNameTaken(ctx context.Context, name string, ignoredIDs []string) (bool, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name FROM "board"`)
	if err != nil {
		return false, fmt.Errorf("db: board name taken: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, boardName string
		if err := rows.Scan(&id, &boardName); err != nil {
			return false, fmt.Errorf("db: board name taken: %w", err)
		}
		if strings.EqualFold(boardName, name) && !containsString(ignoredIDs, id) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// UpdateBoardName renames a board (renameBoard).
func (d *DB) UpdateBoardName(ctx context.Context, id, name string) error {
	_, err := d.ExecContext(ctx, `UPDATE "board" SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("db: update board name: %w", err)
	}
	return nil
}

// UpdateBoardVisibility flips is_public (changeBoardVisibility).
func (d *DB) UpdateBoardVisibility(ctx context.Context, id string, isPublic bool) error {
	_, err := d.ExecContext(ctx, `UPDATE "board" SET is_public = ? WHERE id = ?`, boolToInt(isPublic), id)
	if err != nil {
		return fmt.Errorf("db: update board visibility: %w", err)
	}
	return nil
}

// BoardPartialSettings carries the optional columns of
// savePartialBoardSettings; nil members are left unchanged, mirroring drizzle
// skipping undefined values in .set (packages/api/src/router/board.ts:1658-1686).
type BoardPartialSettings struct {
	PageTitle                 *string
	MetaTitle                 *string
	LogoImageUrl              *string
	FaviconImageUrl           *string
	BackgroundImageUrl        *string
	BackgroundImageAttachment *string
	BackgroundImageRepeat     *string
	BackgroundImageSize       *string
	PrimaryColor              *string
	SecondaryColor            *string
	Opacity                   *int
	CustomCss                 *string
	IconColor                 *string
	ItemRadius                *string
	DisableStatus             *bool
}

// UpdateBoardPartialSettings applies the non-nil fields of settings.
func (d *DB) UpdateBoardPartialSettings(ctx context.Context, id string, s BoardPartialSettings) error {
	sets := []string{}
	args := []any{}
	appendSet := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	if s.PageTitle != nil {
		appendSet("page_title", nullableString(*s.PageTitle))
	}
	if s.MetaTitle != nil {
		appendSet("meta_title", nullableString(*s.MetaTitle))
	}
	if s.LogoImageUrl != nil {
		appendSet("logo_image_url", nullableString(*s.LogoImageUrl))
	}
	if s.FaviconImageUrl != nil {
		appendSet("favicon_image_url", nullableString(*s.FaviconImageUrl))
	}
	if s.BackgroundImageUrl != nil {
		appendSet("background_image_url", nullableString(*s.BackgroundImageUrl))
	}
	if s.BackgroundImageAttachment != nil {
		appendSet("background_image_attachment", *s.BackgroundImageAttachment)
	}
	if s.BackgroundImageRepeat != nil {
		appendSet("background_image_repeat", *s.BackgroundImageRepeat)
	}
	if s.BackgroundImageSize != nil {
		appendSet("background_image_size", *s.BackgroundImageSize)
	}
	if s.PrimaryColor != nil {
		appendSet("primary_color", *s.PrimaryColor)
	}
	if s.SecondaryColor != nil {
		appendSet("secondary_color", *s.SecondaryColor)
	}
	if s.Opacity != nil {
		appendSet("opacity", *s.Opacity)
	}
	if s.CustomCss != nil {
		appendSet("custom_css", nullableString(*s.CustomCss))
	}
	if s.IconColor != nil {
		appendSet("icon_color", nullableString(*s.IconColor))
	}
	if s.ItemRadius != nil {
		appendSet("item_radius", *s.ItemRadius)
	}
	if s.DisableStatus != nil {
		appendSet("disable_status", boolToInt(*s.DisableStatus))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := d.ExecContext(ctx, `UPDATE "board" SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("db: update board partial settings: %w", err)
	}
	return nil
}

// nullableString maps "" to NULL, mirroring the trimmedNullableString zod
// transform of the upstream settings schemas (packages/validation/src/board.ts).
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// DeleteBoardByID deletes one board. Upstream deletes only the board row and
// relies on SQLite foreign keys, which are OFF at runtime (see the package
// doc): child rows (sections, items, item_layout, ...) stay orphaned exactly
// like upstream better-sqlite3 leaves them.
func (d *DB) DeleteBoardByID(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM "board" WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete board: %w", err)
	}
	return nil
}

// UserHomeBoards carries the two home board columns of the "user" table.
type UserHomeBoards struct {
	HomeBoardID       string // empty = NULL
	MobileHomeBoardID string // empty = NULL
}

// GetUserHomeBoards returns the home board selections of one user
// (getBoardAccessContextAsync, packages/api/src/router/board.ts:2868-2890).
func (d *DB) GetUserHomeBoards(ctx context.Context, userID string) (UserHomeBoards, error) {
	const query = `SELECT home_board_id, mobile_home_board_id FROM "user" WHERE id = ?`
	var (
		out    UserHomeBoards
		home   sql.NullString
		mobile sql.NullString
	)
	err := d.QueryRowContext(ctx, query, userID).Scan(&home, &mobile)
	if errors.Is(err, sql.ErrNoRows) {
		return UserHomeBoards{}, ErrNotFound
	}
	if err != nil {
		return UserHomeBoards{}, fmt.Errorf("db: get user home boards: %w", err)
	}
	out.HomeBoardID = home.String
	out.MobileHomeBoardID = mobile.String
	return out, nil
}

// SetUserHomeBoard selects the desktop home board (setHomeBoard).
func (d *DB) SetUserHomeBoard(ctx context.Context, userID, boardID string) error {
	return d.setUserHomeColumn(ctx, "home_board_id", userID, boardID)
}

// SetUserMobileHomeBoard selects the mobile home board (setMobileHomeBoard).
func (d *DB) SetUserMobileHomeBoard(ctx context.Context, userID, boardID string) error {
	return d.setUserHomeColumn(ctx, "mobile_home_board_id", userID, boardID)
}

func (d *DB) setUserHomeColumn(ctx context.Context, column, userID, boardID string) error {
	_, err := d.ExecContext(ctx, `UPDATE "user" SET `+column+` = ? WHERE id = ?`, boardID, userID)
	if err != nil {
		return fmt.Errorf("db: set user home board: %w", err)
	}
	return nil
}

// BoardServerSettings carries the "board" server-setting keys used by the
// board router (getServerSettingByKeyAsync(db, "board"),
// packages/db/queries/server-setting.ts:34-52; defaults in
// packages/server-settings/src/index.ts:142-147).
type BoardServerSettings struct {
	HomeBoardID       string // empty = NULL / unset
	MobileHomeBoardID string // empty = NULL / unset
}

// GetBoardServerSettings reads the "board" row of the serverSetting table.
// The value is stored superjson-serialized ({"json": {...}}); missing rows or
// missing keys fall back to the upstream defaults (both ids null).
func (d *DB) GetBoardServerSettings(ctx context.Context) (BoardServerSettings, error) {
	var raw sql.NullString
	err := d.QueryRowContext(ctx, `SELECT value FROM "serverSetting" WHERE setting_key = 'board'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return BoardServerSettings{}, nil
	}
	if err != nil {
		return BoardServerSettings{}, fmt.Errorf("db: get board server settings: %w", err)
	}
	if !raw.Valid {
		return BoardServerSettings{}, nil
	}
	var envelope struct {
		JSON struct {
			HomeBoardID       *string `json:"homeBoardId"`
			MobileHomeBoardID *string `json:"mobileHomeBoardId"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(raw.String), &envelope); err != nil {
		return BoardServerSettings{}, fmt.Errorf("db: parse board server settings: %w", err)
	}
	out := BoardServerSettings{}
	if envelope.JSON.HomeBoardID != nil {
		out.HomeBoardID = *envelope.JSON.HomeBoardID
	}
	if envelope.JSON.MobileHomeBoardID != nil {
		out.MobileHomeBoardID = *envelope.JSON.MobileHomeBoardID
	}
	return out, nil
}

// --- board permission tables -------------------------------------------------

// BoardUserPermission is one row of the "boardUserPermission" table.
type BoardUserPermission struct {
	BoardID    string
	UserID     string
	Permission string // "view" | "modify" | "full"
}

// BoardGroupPermission is one row of the "boardGroupPermission" table.
type BoardGroupPermission struct {
	BoardID    string
	GroupID    string
	Permission string // "view" | "modify" | "full"
}

// ListBoardUserPermissionsForUser returns every direct board permission of a
// user across all boards (the userPermissions relation filtered by user id).
func (d *DB) ListBoardUserPermissionsForUser(ctx context.Context, userID string) ([]BoardUserPermission, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT board_id, user_id, permission FROM "boardUserPermission" WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list board user permissions: %w", err)
	}
	return scanBoardUserPermissions(rows)
}

// ListBoardUserPermissionsForBoard returns every direct board permission of
// one board, with the member user columns needed by board.getBoardPermissions.
type BoardUserPermissionWithUser struct {
	BoardUserPermission
	UserName  sql.NullString
	UserImage sql.NullString
	UserEmail sql.NullString
}

// ListBoardUserPermissionsForBoardWithUser is the getBoardPermissions users
// query (packages/api/src/router/board.ts:2144-2156).
func (d *DB) ListBoardUserPermissionsForBoardWithUser(ctx context.Context, boardID string) ([]BoardUserPermissionWithUser, error) {
	rows, err := d.QueryContext(ctx, `SELECT p.board_id, p.user_id, p.permission, u.name, u.image, u.email
		FROM "boardUserPermission" p
		JOIN "user" u ON u.id = p.user_id
		WHERE p.board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list board user permissions with user: %w", err)
	}
	defer rows.Close()
	var out []BoardUserPermissionWithUser
	for rows.Next() {
		var p BoardUserPermissionWithUser
		if err := rows.Scan(&p.BoardID, &p.UserID, &p.Permission, &p.UserName, &p.UserImage, &p.UserEmail); err != nil {
			return nil, fmt.Errorf("db: list board user permissions with user: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanBoardUserPermissions(rows *sql.Rows) ([]BoardUserPermission, error) {
	defer rows.Close()
	var out []BoardUserPermission
	for rows.Next() {
		var p BoardUserPermission
		if err := rows.Scan(&p.BoardID, &p.UserID, &p.Permission); err != nil {
			return nil, fmt.Errorf("db: list board user permissions: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplaceBoardUserPermissions swaps the direct user permissions of a board in
// one transaction (saveUserBoardPermissions,
// packages/api/src/router/board.ts:2195-2233).
func (d *DB) ReplaceBoardUserPermissions(ctx context.Context, boardID string, permissions []BoardUserPermission) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: replace board user permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM "boardUserPermission" WHERE board_id = ?`, boardID); err != nil {
		return fmt.Errorf("db: replace board user permissions: %w", err)
	}
	for _, p := range permissions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "boardUserPermission" (board_id, user_id, permission) VALUES (?, ?, ?)`,
			boardID, p.UserID, p.Permission); err != nil {
			return fmt.Errorf("db: replace board user permissions: %w", err)
		}
	}
	return tx.Commit()
}

// ListBoardGroupPermissionsForUser returns the board group permissions of the
// groups a user belongs to (the groupPermissions relation filtered by the
// caller's memberships).
func (d *DB) ListBoardGroupPermissionsForUser(ctx context.Context, boardID, userID string) ([]BoardGroupPermission, error) {
	rows, err := d.QueryContext(ctx, `SELECT bgp.board_id, bgp.group_id, bgp.permission
		FROM "boardGroupPermission" bgp
		WHERE bgp.board_id = ? AND bgp.group_id IN
			(SELECT group_id FROM "groupMember" WHERE user_id = ?)`, boardID, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list board group permissions for user: %w", err)
	}
	return scanBoardGroupPermissions(rows)
}

// ListBoardGroupPermissionsForGroups returns the board group permissions of
// the given groups across every board.
func (d *DB) ListBoardGroupPermissionsForGroups(ctx context.Context, groupIDs []string) ([]BoardGroupPermission, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(groupIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, `SELECT board_id, group_id, permission FROM "boardGroupPermission"
		WHERE group_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list board group permissions for groups: %w", err)
	}
	return scanBoardGroupPermissions(rows)
}

// BoardGroupPermissionWithGroup is the getBoardPermissions groups query
// (packages/api/src/router/board.ts:2158-2168).
type BoardGroupPermissionWithGroup struct {
	BoardGroupPermission
	GroupName string
}

// ListBoardGroupPermissionsForBoardWithGroup joins the group columns.
func (d *DB) ListBoardGroupPermissionsForBoardWithGroup(ctx context.Context, boardID string) ([]BoardGroupPermissionWithGroup, error) {
	rows, err := d.QueryContext(ctx, `SELECT p.board_id, p.group_id, p.permission, g.name
		FROM "boardGroupPermission" p
		JOIN "group" g ON g.id = p.group_id
		WHERE p.board_id = ?`, boardID)
	if err != nil {
		return nil, fmt.Errorf("db: list board group permissions with group: %w", err)
	}
	defer rows.Close()
	var out []BoardGroupPermissionWithGroup
	for rows.Next() {
		var p BoardGroupPermissionWithGroup
		if err := rows.Scan(&p.BoardID, &p.GroupID, &p.Permission, &p.GroupName); err != nil {
			return nil, fmt.Errorf("db: list board group permissions with group: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanBoardGroupPermissions(rows *sql.Rows) ([]BoardGroupPermission, error) {
	defer rows.Close()
	var out []BoardGroupPermission
	for rows.Next() {
		var p BoardGroupPermission
		if err := rows.Scan(&p.BoardID, &p.GroupID, &p.Permission); err != nil {
			return nil, fmt.Errorf("db: list board group permissions: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplaceBoardGroupPermissions swaps the group permissions of a board
// (saveGroupBoardPermissions, packages/api/src/router/board.ts:2234-2274).
func (d *DB) ReplaceBoardGroupPermissions(ctx context.Context, boardID string, permissions []BoardGroupPermission) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: replace board group permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM "boardGroupPermission" WHERE board_id = ?`, boardID); err != nil {
		return fmt.Errorf("db: replace board group permissions: %w", err)
	}
	for _, p := range permissions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "boardGroupPermission" (board_id, group_id, permission) VALUES (?, ?, ?)`,
			boardID, p.GroupID, p.Permission); err != nil {
			return fmt.Errorf("db: replace board group permissions: %w", err)
		}
	}
	return tx.Commit()
}

// --- group membership / group permission reads (read-only helpers used by the
// board router's visibility filters; the group router itself is owned
// elsewhere) ---------------------------------------------------------------

// ListGroupIDsForUser returns the groups a user belongs to.
func (d *DB) ListGroupIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT group_id FROM "groupMember" WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list group ids for user: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: list group ids for user: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListGroupPermissionsForUser returns the distinct group permission keys
// granted to a user through its groups (getCurrentUserPermissionsAsync,
// packages/auth/callbacks.ts:10-21).
func (d *DB) ListGroupPermissionsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT gp.permission
		FROM "groupPermission" gp
		JOIN "groupMember" gm ON gm.group_id = gp.group_id
		WHERE gm.user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list group permissions for user: %w", err)
	}
	return scanStrings(rows)
}

// ListGroupPermissionsForGroups returns the permission rows of the given
// groups (groupPermissions relation).
func (d *DB) ListGroupPermissionsForGroups(ctx context.Context, groupIDs []string) ([]GroupPermissionRow, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(groupIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, `SELECT group_id, permission FROM "groupPermission"
		WHERE group_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list group permissions for groups: %w", err)
	}
	defer rows.Close()
	var out []GroupPermissionRow
	for rows.Next() {
		var p GroupPermissionRow
		if err := rows.Scan(&p.GroupID, &p.Permission); err != nil {
			return nil, fmt.Errorf("db: list group permissions for groups: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupPermissionRow is one "groupPermission" row.
type GroupPermissionRow struct {
	GroupID    string
	Permission string
}

// ListBoardIDsForGroups returns the boards shared with the given groups via
// the boardGroupPermission table.
func (d *DB) ListBoardIDsForGroups(ctx context.Context, groupIDs []string) ([]string, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(groupIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT board_id FROM "boardGroupPermission"
		WHERE group_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list board ids for groups: %w", err)
	}
	return scanStrings(rows)
}

// ListBoardIDsForUser returns the boards shared directly with a user via the
// boardUserPermission table.
func (d *DB) ListBoardIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT board_id FROM "boardUserPermission" WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list board ids for user: %w", err)
	}
	return scanStrings(rows)
}

// GroupHomeBoard carries the home board columns of the "group" table.
type GroupHomeBoard struct {
	ID                string
	Name              string
	HomeBoardID       string // empty = NULL
	MobileHomeBoardID string // empty = NULL
	Position          int
}

// GetGroupByName returns one group by its unique name (getHomeIdBoardAsync
// everyone group lookup, packages/api/src/router/board.ts:2537-2542).
func (d *DB) GetGroupByName(ctx context.Context, name string) (GroupHomeBoard, error) {
	const query = `SELECT id, name, home_board_id, mobile_home_board_id, position FROM "group" WHERE name = ?`
	g, err := scanGroupHomeBoard(d.QueryRowContext(ctx, query, name))
	if errors.Is(err, ErrNotFound) {
		return GroupHomeBoard{}, ErrNotFound
	}
	if err != nil {
		return GroupHomeBoard{}, fmt.Errorf("db: get group by name: %w", err)
	}
	return g, nil
}

// LowestGroupHomeBoardForUser returns the group with the lowest position that
// the user belongs to, is not the everyone group, and has the device-specific
// home board column and the home board column both set — the "lowest
// positions group" of getHomeIdBoardAsync (packages/api/src/router/board.ts:2515-2532).
// settingColumn is "home_board_id" or "mobile_home_board_id".
func (d *DB) LowestGroupHomeBoardForUser(ctx context.Context, userID, settingColumn string) (GroupHomeBoard, error) {
	if settingColumn != "home_board_id" && settingColumn != "mobile_home_board_id" {
		return GroupHomeBoard{}, fmt.Errorf("db: invalid home board column %q", settingColumn)
	}
	query := `SELECT g.id, g.name, g.home_board_id, g.mobile_home_board_id, g.position
		FROM "group" g
		JOIN "groupMember" gm ON gm.group_id = g.id
		WHERE gm.user_id = ? AND g.name != 'everyone'
			AND g.` + settingColumn + ` IS NOT NULL
			AND g.home_board_id IS NOT NULL
		ORDER BY g.position LIMIT 1`
	g, err := scanGroupHomeBoard(d.QueryRowContext(ctx, query, userID))
	if errors.Is(err, ErrNotFound) {
		return GroupHomeBoard{}, ErrNotFound
	}
	if err != nil {
		return GroupHomeBoard{}, fmt.Errorf("db: lowest group home board: %w", err)
	}
	return g, nil
}

func scanGroupHomeBoard(row *sql.Row) (GroupHomeBoard, error) {
	var (
		g      GroupHomeBoard
		home   sql.NullString
		mobile sql.NullString
	)
	err := row.Scan(&g.ID, &g.Name, &home, &mobile, &g.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupHomeBoard{}, ErrNotFound
	}
	if err != nil {
		return GroupHomeBoard{}, err
	}
	g.HomeBoardID = home.String
	g.MobileHomeBoardID = mobile.String
	return g, nil
}

// BoardSummaryRow is a minimal board row for list endpoints.
type BoardSummaryRow struct {
	ID           string
	Name         string
	LogoImageUrl string // empty = NULL
}

// ListPublicBoards returns the public boards (board.getPublicBoards).
func (d *DB) ListPublicBoards(ctx context.Context) ([]BoardSummaryRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, logo_image_url FROM "board" WHERE is_public = 1`)
	if err != nil {
		return nil, fmt.Errorf("db: list public boards: %w", err)
	}
	return scanBoardSummaryRows(rows)
}

// ListAllBoardsMinimal returns every board (board.getBoardsForGroup with the
// board-view-all expansion).
func (d *DB) ListAllBoardsMinimal(ctx context.Context) ([]BoardSummaryRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, logo_image_url FROM "board"`)
	if err != nil {
		return nil, fmt.Errorf("db: list all boards minimal: %w", err)
	}
	return scanBoardSummaryRows(rows)
}

func scanBoardSummaryRows(rows *sql.Rows) ([]BoardSummaryRow, error) {
	defer rows.Close()
	var out []BoardSummaryRow
	for rows.Next() {
		var (
			r    BoardSummaryRow
			logo sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Name, &logo); err != nil {
			return nil, err
		}
		r.LogoImageUrl = logo.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
