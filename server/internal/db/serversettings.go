package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repositories for the "serverSetting" table, serving the serverSettings
// tRPC router (packages/api/src/router/serverSettings.ts) and the onboarding
// claim row (__onboarding_claim). Values are stored as superjson payload
// strings, exactly like upstream writes them (superjson.stringify);
// interpreting/merging them is the router's job (getServerSettingByKeyAsync,
// packages/db/queries/server-setting.ts:34-51).

// ServerSetting maps to the upstream "serverSetting" table
// (packages/db/schema/sqlite.ts, export serverSettings).
type ServerSetting struct {
	SettingKey string
	// Value is the raw superjson payload string ('{"json": ...}').
	Value string
}

// GetServerSetting returns one setting row, or ErrNotFound (upstream falls
// back to the defaults when the row is missing).
func (d *DB) GetServerSetting(ctx context.Context, key string) (ServerSetting, error) {
	const query = `SELECT setting_key, value FROM "serverSetting" WHERE setting_key = ?`
	var s ServerSetting
	err := d.QueryRowContext(ctx, query, key).Scan(&s.SettingKey, &s.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return ServerSetting{}, ErrNotFound
	}
	if err != nil {
		return ServerSetting{}, fmt.Errorf("db: get server setting: %w", err)
	}
	return s, nil
}

// ListServerSettings returns every setting row
// (getServerSettingsAsync, packages/db/queries/server-setting.ts:10-32).
func (d *DB) ListServerSettings(ctx context.Context) ([]ServerSetting, error) {
	rows, err := d.QueryContext(ctx, `SELECT setting_key, value FROM "serverSetting"`)
	if err != nil {
		return nil, fmt.Errorf("db: list server settings: %w", err)
	}
	defer rows.Close()
	var out []ServerSetting
	for rows.Next() {
		var s ServerSetting
		if err := rows.Scan(&s.SettingKey, &s.Value); err != nil {
			return nil, fmt.Errorf("db: list server settings: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// InsertServerSetting inserts a new setting row
// (insertServerSettingByKeyAsync, server-setting.ts:92-101).
func (d *DB) InsertServerSetting(ctx context.Context, key, value string) error {
	const query = `INSERT INTO "serverSetting" (setting_key, value) VALUES (?, ?)`
	if _, err := d.ExecContext(ctx, query, key, value); err != nil {
		return fmt.Errorf("db: insert server setting: %w", err)
	}
	return nil
}

// UpdateServerSetting overwrites the value of an existing row
// (updateServerSettingByKeyAsync, server-setting.ts:53-64). A missing row
// updates nothing (upstream UPDATE ... WHERE has the same no-op behavior).
func (d *DB) UpdateServerSetting(ctx context.Context, key, value string) error {
	const query = `UPDATE "serverSetting" SET value = ? WHERE setting_key = ?`
	if _, err := d.ExecContext(ctx, query, value, key); err != nil {
		return fmt.Errorf("db: update server setting: %w", err)
	}
	return nil
}

// UpsertServerSetting writes a setting regardless of whether the row
// exists (saveBoardSettingsAsync branches on existence
// (serverSettings.ts:71-78); a single upsert keeps the same outcome).
func (d *DB) UpsertServerSetting(ctx context.Context, key, value string) error {
	const query = `INSERT INTO "serverSetting" (setting_key, value) VALUES (?, ?)
		ON CONFLICT(setting_key) DO UPDATE SET value = excluded.value`
	if _, err := d.ExecContext(ctx, query, key, value); err != nil {
		return fmt.Errorf("db: upsert server setting: %w", err)
	}
	return nil
}

// DeleteServerSetting removes a setting row (onboard.completeSetup deletes
// the __onboarding_claim row when the setup finishes).
func (d *DB) DeleteServerSetting(ctx context.Context, key string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM "serverSetting" WHERE setting_key = ?`, key); err != nil {
		return fmt.Errorf("db: delete server setting: %w", err)
	}
	return nil
}
