package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repositories for the "group", "groupMember" and "groupPermission" tables,
// serving the group tRPC router (packages/api/src/router/group.ts) and the
// user router group assignments. Table names stay camelCase like the drizzle
// schema; columns are snake_case after migration 0016.

// Group maps to the upstream "group" table (packages/db/schema/sqlite.ts,
// export groups). Empty strings on pointer columns (ownerId, homeBoardId,
// mobileHomeBoardId) mean NULL.
type Group struct {
	ID                string
	Name              string
	OwnerID           string
	HomeBoardID       string
	MobileHomeBoardID string
	Position          int64
}

const groupColumns = `id, name, owner_id, home_board_id, mobile_home_board_id, position`

// GroupMemberUser is a user row as exposed through a group membership. Only
// the columns the group router selects are loaded; Provider is only part of
// group.getById.
type GroupMemberUser struct {
	ID       string
	Name     string // empty = NULL
	Email    string // empty = NULL
	Image    string // empty = NULL
	Provider string
}

// GroupMemberRow is one membership with its group id, used to stitch
// members into group lists.
type GroupMemberRow struct {
	GroupID string
	User    GroupMemberUser
}

// MaxGroupPosition is getMaxGroupPositionAsync
// (packages/db/queries/group.ts:6-11): SELECT max(position) with NULL
// (empty table) coalescing to 1, so the first created group gets position 2.
func (d *DB) MaxGroupPosition(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := d.QueryRowContext(ctx, `SELECT MAX(position) FROM "group"`).Scan(&max); err != nil {
		return 0, fmt.Errorf("db: max group position: %w", err)
	}
	if !max.Valid {
		return 1, nil
	}
	return max.Int64, nil
}

// CreateGroup inserts a group row (upstream createGroup /
// createInitialExternalGroup set id, name, position and optionally ownerId).
func (d *DB) CreateGroup(ctx context.Context, g Group) error {
	const query = `INSERT INTO "group" (id, name, owner_id, home_board_id, mobile_home_board_id, position)
		VALUES (?, ?, ?, ?, ?, ?)`
	var owner, home, mobileHome any
	if g.OwnerID != "" {
		owner = g.OwnerID
	}
	if g.HomeBoardID != "" {
		home = g.HomeBoardID
	}
	if g.MobileHomeBoardID != "" {
		mobileHome = g.MobileHomeBoardID
	}
	if _, err := d.ExecContext(ctx, query, g.ID, g.Name, owner, home, mobileHome, g.Position); err != nil {
		return fmt.Errorf("db: create group: %w", err)
	}
	return nil
}

// GetGroupByID returns the group with the given id, or ErrNotFound.
func (d *DB) GetGroupByID(ctx context.Context, id string) (Group, error) {
	const query = `SELECT ` + groupColumns + ` FROM "group" WHERE id = ?`
	g, err := scanGroup(d.QueryRowContext(ctx, query, id))
	if err != nil {
		return Group{}, fmt.Errorf("db: get group by id: %w", err)
	}
	return g, nil
}

// ListGroups returns every group. Upstream findMany has no ORDER BY
// (group.getAll, packages/api/src/router/group.ts:24-46); SQLite scans in
// rowid order, which this query reproduces.
func (d *DB) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+groupColumns+` FROM "group"`)
	if err != nil {
		return nil, fmt.Errorf("db: list groups: %w", err)
	}
	return scanGroups(rows)
}

// CountGroups is db.$count(groups, whereQuery) with the optional
// `like(groups.name, pattern)` filter of group.getPaginated. An empty
// pattern counts all groups. SQLite LIKE is ASCII case-insensitive, the
// same as drizzle's like() on SQLite.
func (d *DB) CountGroups(ctx context.Context, namePattern string) (int64, error) {
	query := `SELECT COUNT(*) FROM "group"`
	var args []any
	if namePattern != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, namePattern)
	}
	var count int64
	if err := d.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("db: count groups: %w", err)
	}
	return count, nil
}

// ListGroupsPaginated is the group.getPaginated page query (limit/offset
// with the optional name filter, no ORDER BY like upstream).
func (d *DB) ListGroupsPaginated(ctx context.Context, namePattern string, limit, offset int64) ([]Group, error) {
	query := `SELECT ` + groupColumns + ` FROM "group"`
	var args []any
	if namePattern != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, namePattern)
	}
	query += ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list groups paginated: %w", err)
	}
	return scanGroups(rows)
}

// ListSelectableGroups is the {id, name} projection of group.selectable and
// group.search. pattern "" lists all groups; otherwise LIKE '%pattern%'.
func (d *DB) ListSelectableGroups(ctx context.Context, pattern string, limit int64) ([]Group, error) {
	query := `SELECT id, name FROM "group"`
	var args []any
	if pattern != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, pattern)
	}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list selectable groups: %w", err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, fmt.Errorf("db: list selectable groups: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpdateGroupName renames a group (group.updateGroup).
func (d *DB) UpdateGroupName(ctx context.Context, id, name string) error {
	const query = `UPDATE "group" SET name = ? WHERE id = ?`
	if _, err := d.ExecContext(ctx, query, name, id); err != nil {
		return fmt.Errorf("db: update group name: %w", err)
	}
	return nil
}

// GroupSettingsUpdate is the partial settings payload of
// group.savePartialSettings: a Set* flag marks the key as supplied
// (drizzle skips undefined keys in .set()); the empty string writes NULL.
type GroupSettingsUpdate struct {
	SetHomeBoard       bool
	HomeBoardID        string
	SetMobileHomeBoard bool
	MobileHomeBoardID  string
}

// UpdateGroupSettings applies the supplied settings of
// group.savePartialSettings (packages/api/src/router/group.ts:258-271).
func (d *DB) UpdateGroupSettings(ctx context.Context, id string, u GroupSettingsUpdate) error {
	query := `UPDATE "group" SET `
	var sets []string
	var args []any
	if u.SetHomeBoard {
		sets = append(sets, `home_board_id = ?`)
		args = append(args, nullableStringArg(u.HomeBoardID))
	}
	if u.SetMobileHomeBoard {
		sets = append(sets, `mobile_home_board_id = ?`)
		args = append(args, nullableStringArg(u.MobileHomeBoardID))
	}
	if len(sets) == 0 {
		return nil
	}
	query += joins(sets, ", ") + ` WHERE id = ?`
	args = append(args, id)
	if _, err := d.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("db: update group settings: %w", err)
	}
	return nil
}

// UpdateGroupOwner transfers ownership (group.transferOwnership). An empty
// userID writes NULL.
func (d *DB) UpdateGroupOwner(ctx context.Context, id, userID string) error {
	const query = `UPDATE "group" SET owner_id = ? WHERE id = ?`
	var owner any
	if userID != "" {
		owner = userID
	}
	if _, err := d.ExecContext(ctx, query, owner, id); err != nil {
		return fmt.Errorf("db: update group owner: %w", err)
	}
	return nil
}

// ListGroupPermissionStrings returns the raw permission keys of one group
// (group.getById permissions and group.selectable withPermissions).
func (d *DB) ListGroupPermissionStrings(ctx context.Context, groupID string) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT permission FROM "groupPermission" WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: list group permission strings: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("db: list group permission strings: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupPosition is one entry of group.savePositions.
type GroupPosition struct {
	ID       string
	Position int64
}

// UpdateGroupPositions repositions groups inside a transaction
// (group.savePositions writes position = index + 1 for every id).
func (d *DB) UpdateGroupPositions(ctx context.Context, positions []GroupPosition) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: update group positions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE "group" SET position = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("db: update group positions: %w", err)
	}
	defer stmt.Close()
	for _, p := range positions {
		if _, err := stmt.ExecContext(ctx, p.Position, p.ID); err != nil {
			return fmt.Errorf("db: update group positions: %w", err)
		}
	}
	return tx.Commit()
}

// ReplaceGroupPermissions swaps the global permission rows of a group
// (group.savePermissions: delete all, insert the deduplicated set).
func (d *DB) ReplaceGroupPermissions(ctx context.Context, groupID string, permissions []string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: replace group permissions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM "groupPermission" WHERE group_id = ?`, groupID); err != nil {
		return fmt.Errorf("db: replace group permissions: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("db: replace group permissions: %w", err)
	}
	defer stmt.Close()
	for _, permission := range permissions {
		if _, err := stmt.ExecContext(ctx, groupID, permission); err != nil {
			return fmt.Errorf("db: replace group permissions: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteGroup removes the group row (group.deleteGroup). The
// groupMember/groupPermission rows go through the ON DELETE cascade — like
// upstream, which runs with foreign keys OFF on its own connection but
// never relies on orphans either way (drizzle keeps the schema FKs and
// better-sqlite3 honors the cascade declared per statement semantics).
func (d *DB) DeleteGroup(ctx context.Context, id string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM "group" WHERE id = ?`, id); err != nil {
		return fmt.Errorf("db: delete group: %w", err)
	}
	return nil
}

// GroupNameSimilarExists is checkSimilarNameAndThrowAsync
// (packages/api/src/router/group.ts:407-418): LIKE with the raw name as
// pattern (no wildcard escaping upstream), excluding ignoreID when set.
func (d *DB) GroupNameSimilarExists(ctx context.Context, name, ignoreID string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM "group" WHERE name LIKE ? AND id <> ?)`
	var exists bool
	if err := d.QueryRowContext(ctx, query, name, ignoreID).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: group name similar exists: %w", err)
	}
	return exists, nil
}

// GroupIsEveryone reports whether the group with the given id carries the
// reserved "everyone" name (throwIfGroupNameIsReservedAsync
// (packages/api/src/router/group.ts:420-429) counts id = ? AND name =
// 'everyone').
func (d *DB) GroupIsEveryone(ctx context.Context, id string) (bool, error) {
	const query = `SELECT COUNT(*) FROM "group" WHERE id = ? AND name = 'everyone'`
	var count int64
	if err := d.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
		return false, fmt.Errorf("db: group is everyone: %w", err)
	}
	return count > 0, nil
}

// AddGroupMember inserts one membership (group.addMember). A duplicate
// surfaces as the SQLite UNIQUE constraint error, exactly like upstream,
// where the raw insert failure becomes an INTERNAL_SERVER_ERROR.
func (d *DB) AddGroupMember(ctx context.Context, groupID, userID string) error {
	if _, err := d.ExecContext(ctx,
		`INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, groupID, userID); err != nil {
		return fmt.Errorf("db: add group member: %w", err)
	}
	return nil
}

// RemoveGroupMember deletes one membership; removing a non-member is a
// no-op, like upstream's plain delete (group.removeMember).
func (d *DB) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	if _, err := d.ExecContext(ctx,
		`DELETE FROM "groupMember" WHERE group_id = ? AND user_id = ?`, groupID, userID); err != nil {
		return fmt.Errorf("db: remove group member: %w", err)
	}
	return nil
}

// ListGroupMemberUsers returns the member users of one group in insertion
// order (group.getById members, including the provider column).
func (d *DB) ListGroupMemberUsers(ctx context.Context, groupID string) ([]GroupMemberUser, error) {
	rows, err := d.QueryContext(ctx, `SELECT u.id, u.name, u.email, u.image, u.provider
		FROM "groupMember" gm JOIN "user" u ON u.id = gm.user_id
		WHERE gm.group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: list group member users: %w", err)
	}
	defer rows.Close()
	var out []GroupMemberUser
	for rows.Next() {
		var u GroupMemberUser
		var name, email, image sql.NullString
		if err := rows.Scan(&u.ID, &name, &email, &image, &u.Provider); err != nil {
			return nil, fmt.Errorf("db: list group member users: %w", err)
		}
		u.Name, u.Email, u.Image = name.String, email.String, image.String
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListGroupMembersForGroups returns the member users of the given groups
// with their group id, used to stitch group.getAll / getPaginated outputs.
func (d *DB) ListGroupMembersForGroups(ctx context.Context, groupIDs []string) ([]GroupMemberRow, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	query := `SELECT gm.group_id, u.id, u.name, u.email, u.image
		FROM "groupMember" gm JOIN "user" u ON u.id = gm.user_id
		WHERE gm.group_id IN (` + placeholders(len(groupIDs)) + `)`
	args := make([]any, len(groupIDs))
	for i, id := range groupIDs {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list group members for groups: %w", err)
	}
	defer rows.Close()
	var out []GroupMemberRow
	for rows.Next() {
		var r GroupMemberRow
		var name, email, image sql.NullString
		if err := rows.Scan(&r.GroupID, &r.User.ID, &name, &email, &image); err != nil {
			return nil, fmt.Errorf("db: list group members for groups: %w", err)
		}
		r.User.Name, r.User.Email, r.User.Image = name.String, email.String, image.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanGroup(row *sql.Row) (Group, error) {
	var (
		g          Group
		owner      sql.NullString
		home       sql.NullString
		mobileHome sql.NullString
	)
	err := row.Scan(&g.ID, &g.Name, &owner, &home, &mobileHome, &g.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, err
	}
	g.OwnerID, g.HomeBoardID, g.MobileHomeBoardID = owner.String, home.String, mobileHome.String
	return g, nil
}

func scanGroups(rows *sql.Rows) ([]Group, error) {
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var (
			g          Group
			owner      sql.NullString
			home       sql.NullString
			mobileHome sql.NullString
		)
		if err := rows.Scan(&g.ID, &g.Name, &owner, &home, &mobileHome, &g.Position); err != nil {
			return nil, fmt.Errorf("db: scan groups: %w", err)
		}
		g.OwnerID, g.HomeBoardID, g.MobileHomeBoardID = owner.String, home.String, mobileHome.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// nullableStringArg converts a settings string into a database/sql arg where
// the empty string means NULL (the drizzle nullable text columns).
func nullableStringArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// placeholders returns a "?,?,..." list with n entries (shared by the
// IN (...) queries of this package).
func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

// joins joins string fragments with sep (tiny helper for dynamic SET lists).
func joins(parts []string, sep string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += sep
		}
		out += part
	}
	return out
}
