package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// App maps to the upstream "app" table (drizzle schema apps): a bookmark /
// shortcut placed on boards through the "app" and "bookmarks" widgets.
type App struct {
	ID string
	// Name is NOT NULL upstream.
	Name string
	// Description is empty for NULL.
	Description string
	// IconURL is NOT NULL upstream.
	IconURL string
	// HREF is empty for NULL.
	HREF string
	// PingURL is empty for NULL.
	PingURL string
}

const appColumns = `id, name, description, icon_url, href, ping_url`

func scanApps(rows *sql.Rows) ([]App, error) {
	defer rows.Close()
	var out []App
	for rows.Next() {
		var (
			a           App
			description sql.NullString
			href        sql.NullString
			pingURL     sql.NullString
		)
		if err := rows.Scan(&a.ID, &a.Name, &description, &a.IconURL, &href, &pingURL); err != nil {
			return nil, err
		}
		a.Description = description.String
		a.HREF = href.String
		a.PingURL = pingURL.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertApp inserts an app (appRouter create / createMany,
// packages/api/src/router/app.ts:211-255).
func (d *DB) InsertApp(ctx context.Context, a App) error {
	_, err := d.ExecContext(ctx, `INSERT INTO "app" (id, name, description, icon_url, href, ping_url)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, nullableString(a.Description), a.IconURL, nullableString(a.HREF), nullableString(a.PingURL))
	if err != nil {
		return fmt.Errorf("db: insert app: %w", err)
	}
	return nil
}

// UpdateApp applies the appRouter update (packages/api/src/router/app.ts:288-297).
func (d *DB) UpdateApp(ctx context.Context, a App) error {
	_, err := d.ExecContext(ctx, `UPDATE "app" SET name = ?, description = ?, icon_url = ?, href = ?, ping_url = ?
		WHERE id = ?`,
		a.Name, nullableString(a.Description), a.IconURL, nullableString(a.HREF), nullableString(a.PingURL), a.ID)
	if err != nil {
		return fmt.Errorf("db: update app: %w", err)
	}
	return nil
}

// DeleteApp deletes an app (appRouter delete, packages/api/src/router/app.ts:322-329).
func (d *DB) DeleteApp(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM "app" WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete app: %w", err)
	}
	return nil
}

// CountApps counts apps, optionally filtered by a LIKE pattern
// (db.$count in appRouter getPaginated).
func (d *DB) CountApps(ctx context.Context, nameLike string) (int, error) {
	query := `SELECT COUNT(*) FROM "app"`
	args := []any{}
	if nameLike != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, nameLike)
	}
	var n int
	if err := d.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count apps: %w", err)
	}
	return n, nil
}

// ListAppsPaginated returns one page of apps ordered by name (getPaginated).
// nameLike is the LIKE pattern ("" = no filter).
func (d *DB) ListAppsPaginated(ctx context.Context, nameLike string, limit, offset int64) ([]App, error) {
	query := `SELECT ` + appColumns + ` FROM "app"`
	args := []any{}
	if nameLike != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, nameLike)
	}
	query += ` ORDER BY name LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list apps paginated: %w", err)
	}
	return scanApps(rows)
}

// ListAllApps returns every app ordered by name (appRouter all).
func (d *DB) ListAllApps(ctx context.Context) ([]App, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+appColumns+` FROM "app" ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: list all apps: %w", err)
	}
	return scanApps(rows)
}

// SearchApps finds apps whose name contains the query (appRouter search,
// like(apps.name, `%${query}%`), limit applied).
func (d *DB) SearchApps(ctx context.Context, query string, limit int64) ([]App, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+appColumns+` FROM "app" WHERE name LIKE ?
		ORDER BY name LIMIT ?`, "%"+query+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("db: search apps: %w", err)
	}
	return scanApps(rows)
}

// ListSelectableApps returns the app columns of the selectable procedure
// (packages/api/src/router/app.ts:145-157).
func (d *DB) ListSelectableApps(ctx context.Context) ([]App, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, description, icon_url, href, ping_url
		FROM "app" ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: list selectable apps: %w", err)
	}
	return scanApps(rows)
}

// ListAppsByIDs returns the apps with the given ids (byIds; no order like
// upstream findMany).
func (d *DB) ListAppsByIDs(ctx context.Context, ids []string) ([]App, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.QueryContext(ctx, `SELECT `+appColumns+` FROM "app" WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list apps by ids: %w", err)
	}
	return scanApps(rows)
}

// GetAppByID returns one app.
func (d *DB) GetAppByID(ctx context.Context, id string) (App, error) {
	apps, err := d.ListAppsByIDs(ctx, []string{id})
	if err != nil {
		return App{}, err
	}
	if len(apps) == 0 {
		return App{}, ErrNotFound
	}
	return apps[0], nil
}

// PublicBoardAppIDs returns the app ids exposed by items on public boards:
// "app" items contribute their options.appId, "bookmarks" items their legacy
// options.items entries (AppAccessControl.getAllAppIdsOnPublicBoardsAsync,
// packages/api/src/router/app/app-access-control.ts:28-46). The caller
// parses the returned raw option payloads.
func (d *DB) ListAppOptionsOnPublicBoards(ctx context.Context) ([]Item, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, board_id, kind, options, advanced_options FROM "item"
		WHERE kind IN ('app', 'bookmarks')
		  AND board_id IN (SELECT id FROM "board" WHERE is_public = 1)`)
	if err != nil {
		return nil, fmt.Errorf("db: list app options on public boards: %w", err)
	}
	return scanItems(rows)
}

// FindIconURLByName is the getIconForName lookup of appRouter createMany
// (like(icons.name, `%${name}%`), first row; packages/icons/src/
// auto-icon-searcher.ts:5-10).
func (d *DB) FindIconURLByName(ctx context.Context, name string) (string, error) {
	var url string
	err := d.QueryRowContext(ctx, `SELECT url FROM "icon" WHERE name LIKE ? LIMIT 1`, "%"+name+"%").Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: find icon url: %w", err)
	}
	return url, nil
}
