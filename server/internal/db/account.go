package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// This file extends the db repositories with the account-adjacent tables the
// auth layer needs: API keys ("apiKey") and group based permissions
// ("groupMember"/"groupPermission"). Table and column names follow the
// post-migration SQLite schema ported in migrations/ (camelCase tables,
// snake_case columns after migration 0016).

// APIKey maps to the upstream "apiKey" table (packages/db/schema/sqlite.ts,
// export apiKeys; OAuth provenance columns added by migrations 0049/0052 are
// not needed for key validation and stay at their defaults).
type APIKey struct {
	ID string
	// Hash is the bcrypt hash stored in the api_key column: the client sends
	// "<id>.<token>" in the ApiKey header and token must verify against this
	// hash (packages/auth/api-key/get-api-key-session.ts:32-65).
	Hash     string
	ReadOnly bool
	UserID   string
}

const apiKeyColumns = `id, api_key, read_only, user_id`

// CreateAPIKey inserts an API key. The id and the bcrypt hash are
// caller-supplied (upstream generates them in packages/api/src/router/
// apiKeys.ts:73-80).
func (d *DB) CreateAPIKey(ctx context.Context, k APIKey) error {
	const query = `INSERT INTO "apiKey" (id, api_key, read_only, user_id) VALUES (?, ?, ?, ?)`
	_, err := d.ExecContext(ctx, query, k.ID, k.Hash, boolToInt(k.ReadOnly), k.UserID)
	if err != nil {
		return fmt.Errorf("db: create api key: %w", err)
	}
	return nil
}

// GetAPIKeyByID returns the API key with the given id (the part of the
// ApiKey header before the first dot).
func (d *DB) GetAPIKeyByID(ctx context.Context, id string) (APIKey, error) {
	const query = `SELECT ` + apiKeyColumns + ` FROM "apiKey" WHERE id = ?`
	var (
		k        APIKey
		readOnly int
	)
	err := d.QueryRowContext(ctx, query, id).Scan(&k.ID, &k.Hash, &readOnly, &k.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("db: get api key: %w", err)
	}
	k.ReadOnly = readOnly != 0
	return k, nil
}

// UserHasPermission reports whether the user holds a global permission
// through any of its groups (upstream getCurrentUserPermissionsAsync in
// packages/auth/callbacks.ts:10-21 joins groupPermission with groupMember and
// selects distinct). The auth layer uses it for the "admin" permission, which
// is how upstream decides administrator access (packages/api/src/trpc.ts:149).
func (d *DB) UserHasPermission(ctx context.Context, userID, permission string) (bool, error) {
	const query = `SELECT EXISTS(
		SELECT 1 FROM "groupPermission" gp
		JOIN "groupMember" gm ON gm.group_id = gp.group_id
		WHERE gm.user_id = ? AND gp.permission = ?)`
	var exists bool
	if err := d.QueryRowContext(ctx, query, userID, permission).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: user has permission: %w", err)
	}
	return exists, nil
}

// UserPermissions returns every global permission the user holds through its
// groups, expanded with the inherited child permissions the same way upstream
// does. The expansion (groupPermissionParents in
// packages/definitions/src/permissions.ts) lives in the auth package; this
// method only returns the raw, distinct group permissions. The result is
// ordered for stable output.
func (d *DB) UserPermissions(ctx context.Context, userID string) ([]string, error) {
	const query = `SELECT DISTINCT gp.permission FROM "groupPermission" gp
		JOIN "groupMember" gm ON gm.group_id = gp.group_id
		WHERE gm.user_id = ?
		ORDER BY gp.permission`
	rows, err := d.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("db: user permissions: %w", err)
	}
	defer rows.Close()

	var permissions []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("db: user permissions: %w", err)
		}
		permissions = append(permissions, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: user permissions: %w", err)
	}
	return permissions, nil
}

// AddUserToGroup adds the user to the group with the given id if not already
// a member. It reports whether a row was inserted. Used by the auth layer to
// mirror the upstream signIn event, which adds every signing-in user to the
// "everyone" group (packages/auth/events.ts:47,95-112).
func (d *DB) AddUserToGroup(ctx context.Context, userID, groupID string) (bool, error) {
	const query = `INSERT OR IGNORE INTO "groupMember" (group_id, user_id) VALUES (?, ?)`
	res, err := d.ExecContext(ctx, query, groupID, userID)
	if err != nil {
		return false, fmt.Errorf("db: add user to group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: add user to group: %w", err)
	}
	return n > 0, nil
}

// GroupIDByName returns the id of the group with the given name
// (upstream looks up the "everyone" group by its unique name in
// packages/auth/events.ts:96-98).
func (d *DB) GroupIDByName(ctx context.Context, name string) (string, error) {
	const query = `SELECT id FROM "group" WHERE name = ?`
	var id string
	err := d.QueryRowContext(ctx, query, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: group id by name: %w", err)
	}
	return id, nil
}
