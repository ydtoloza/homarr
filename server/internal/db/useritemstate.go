package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UserItemState is one row of the "user_item_state" table: a per-user,
// per-item widget state blob (primary key user_id + item_id + key). The value
// is an arbitrary JSON string (validated by the router).
type UserItemState struct {
	UserID string
	ItemID string
	Key    string
	Value  string
}

// ListUserItemStates returns every state row of one user for one item
// (userItemState.get, packages/api/src/router/user-item-state.ts:23-30).
func (d *DB) ListUserItemStates(ctx context.Context, userID, itemID string) ([]UserItemState, error) {
	rows, err := d.QueryContext(ctx, `SELECT user_id, item_id, key, value FROM "user_item_state"
		WHERE user_id = ? AND item_id = ?`, userID, itemID)
	if err != nil {
		return nil, fmt.Errorf("db: list user item states: %w", err)
	}
	defer rows.Close()
	var out []UserItemState
	for rows.Next() {
		var s UserItemState
		if err := rows.Scan(&s.UserID, &s.ItemID, &s.Key, &s.Value); err != nil {
			return nil, fmt.Errorf("db: list user item states: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CountUserItemStates counts the state rows of one user for one item — the
// 500-key cap check of userItemState.set (packages/api/src/router/
// user-item-state.ts:58-66).
func (d *DB) CountUserItemStates(ctx context.Context, userID, itemID string) (int, error) {
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM "user_item_state" WHERE user_id = ? AND item_id = ?`,
		userID, itemID).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count user item states: %w", err)
	}
	return n, nil
}

// UpsertUserItemState inserts or updates one state key
// (onConflictDoUpdate over userId+itemId+key,
// packages/api/src/router/user-item-state.ts:68-74).
func (d *DB) UpsertUserItemState(ctx context.Context, s UserItemState) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "user_item_state" (user_id, item_id, key, value)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, item_id, key) DO UPDATE SET value = excluded.value`,
		s.UserID, s.ItemID, s.Key, s.Value)
	if err != nil {
		return fmt.Errorf("db: upsert user item state: %w", err)
	}
	return nil
}

// HasUserItemState reports whether one specific key exists for the user and
// item (the existingState lookup of userItemState.set).
func (d *DB) HasUserItemState(ctx context.Context, userID, itemID, key string) (bool, error) {
	var one int
	err := d.QueryRowContext(ctx, `SELECT 1 FROM "user_item_state"
		WHERE user_id = ? AND item_id = ? AND key = ?`, userID, itemID, key).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("db: has user item state: %w", err)
	}
	return one == 1, nil
}
