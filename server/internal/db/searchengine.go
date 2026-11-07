package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repositories for the "search_engine" table, serving the searchEngine tRPC
// router (packages/api/src/router/search-engine/search-engine-router.ts).

// SearchEngine maps to the upstream "search_engine" table
// (packages/db/schema/sqlite.ts, export searchEngines). Empty strings on
// pointer columns (description, urlTemplate, integrationId) mean NULL.
type SearchEngine struct {
	ID            string
	IconURL       string
	Name          string
	Short         string
	Description   string
	URLTemplate   string
	Type          string // "generic" | "fromIntegration"
	IntegrationID string
}

// IntegrationRef is the {id, kind, url} projection the search engine
// queries attach through the integration relation
// (search-engine-router.ts:88-95,148-154,167-175).
type IntegrationRef struct {
	ID   string
	Kind string
	URL  string
}

// SearchEngineFull is a search engine with its (nullable) integration
// relation attached.
type SearchEngineFull struct {
	SearchEngine
	Integration *IntegrationRef // nil when integrationId is NULL
}

// CreateSearchEngine inserts a row (searchEngine.create).
func (d *DB) CreateSearchEngine(ctx context.Context, engine SearchEngine) error {
	const query = `INSERT INTO "search_engine" (id, icon_url, name, short, description, url_template, type, integration_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := d.ExecContext(ctx, query,
		engine.ID, engine.IconURL, engine.Name, engine.Short,
		nullableStringArg(engine.Description), nullableStringArg(engine.URLTemplate),
		engine.Type, nullableStringArg(engine.IntegrationID)); err != nil {
		return fmt.Errorf("db: create search engine: %w", err)
	}
	return nil
}

// UpdateSearchEngine applies searchEngine.update. The "short" column is NOT
// updated: the edit schema omits it (search-engine.ts:32-38).
func (d *DB) UpdateSearchEngine(ctx context.Context, engine SearchEngine) error {
	const query = `UPDATE "search_engine" SET name = ?, icon_url = ?, description = ?, url_template = ?,
		type = ?, integration_id = ? WHERE id = ?`
	if _, err := d.ExecContext(ctx, query,
		engine.Name, engine.IconURL,
		nullableStringArg(engine.Description), nullableStringArg(engine.URLTemplate),
		engine.Type, nullableStringArg(engine.IntegrationID), engine.ID); err != nil {
		return fmt.Errorf("db: update search engine: %w", err)
	}
	return nil
}

// GetSearchEngineByID returns the engine with the given id, or ErrNotFound.
func (d *DB) GetSearchEngineByID(ctx context.Context, id string) (SearchEngine, error) {
	const query = `SELECT ` + searchEngineColumns + ` FROM "search_engine" se WHERE se.id = ?`
	engine, err := scanSearchEngine(d.QueryRowContext(ctx, query, id))
	if err != nil {
		return SearchEngine{}, fmt.Errorf("db: get search engine by id: %w", err)
	}
	return engine, nil
}

// DeleteSearchEngine removes the row (searchEngine.delete). The user
// default references are cleared first by the router.
func (d *DB) DeleteSearchEngine(ctx context.Context, id string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM "search_engine" WHERE id = ?`, id); err != nil {
		return fmt.Errorf("db: delete search engine: %w", err)
	}
	return nil
}

// GetSearchEngineWithIntegrationByID returns one engine with its
// integration relation attached, or nil when the id is unknown
// (getDefaultSearchEngine's findFirst on the resolved default id).
func (d *DB) GetSearchEngineWithIntegrationByID(ctx context.Context, id string) (*SearchEngineFull, error) {
	const query = `SELECT ` + searchEngineColumns + `, i.id, i.kind, i.url
		FROM "search_engine" se LEFT JOIN "integration" i ON i.id = se.integration_id
		WHERE se.id = ?`
	rows, err := d.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("db: get search engine with integration by id: %w", err)
	}
	list, err := scanSearchEnginesFull(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// CountSearchEngines is db.$count(searchEngines, whereQuery) with the
// optional `like(searchEngines.name, pattern)` filter.
func (d *DB) CountSearchEngines(ctx context.Context, namePattern string) (int64, error) {
	query := `SELECT COUNT(*) FROM "search_engine"`
	var args []any
	if namePattern != "" {
		query += ` WHERE name LIKE ?`
		args = append(args, namePattern)
	}
	var count int64
	if err := d.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("db: count search engines: %w", err)
	}
	return count, nil
}

// ListSearchEnginesPaginated is the searchEngine.getPaginated page query
// (no ORDER BY, like upstream).
func (d *DB) ListSearchEnginesPaginated(ctx context.Context, namePattern string, limit, offset int64) ([]SearchEngine, error) {
	query := `SELECT ` + searchEngineColumns + ` FROM "search_engine" se`
	var args []any
	if namePattern != "" {
		query += ` WHERE se.name LIKE ?`
		args = append(args, namePattern)
	}
	query += ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list search engines paginated: %w", err)
	}
	return scanSearchEngines(rows)
}

// ListSelectableSearchEngines is searchEngine.getSelectable: {id, name}
// ordered by name, optionally restricted to generic engines.
func (d *DB) ListSelectableSearchEngines(ctx context.Context, onlyGeneric bool) ([]SearchEngine, error) {
	query := `SELECT id, name FROM "search_engine"`
	if onlyGeneric {
		query += ` WHERE type = 'generic'`
	}
	query += ` ORDER BY name ASC`
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("db: list selectable search engines: %w", err)
	}
	defer rows.Close()
	var out []SearchEngine
	for rows.Next() {
		var engine SearchEngine
		if err := rows.Scan(&engine.ID, &engine.Name); err != nil {
			return nil, fmt.Errorf("db: list selectable search engines: %w", err)
		}
		out = append(out, engine)
	}
	return out, rows.Err()
}

// ListSearchEnginesWithIntegration is searchEngine.catalog: every engine
// (only generic ones for anonymous callers) ordered by name, with the
// integration relation attached.
func (d *DB) ListSearchEnginesWithIntegration(ctx context.Context, onlyGeneric bool) ([]SearchEngineFull, error) {
	query := `SELECT ` + searchEngineColumns + `, i.id, i.kind, i.url
		FROM "search_engine" se LEFT JOIN "integration" i ON i.id = se.integration_id`
	if onlyGeneric {
		query += ` WHERE se.type = 'generic'`
	}
	query += ` ORDER BY se.name ASC`
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("db: list search engines with integration: %w", err)
	}
	return scanSearchEnginesFull(rows)
}

// SearchSearchEnginesByShortPrefix is searchEngine.search: LIKE
// '<prefix>%' on the short column (SQLite LIKE is ASCII case-insensitive,
// matching drizzle's like() with the lowercased input), with the
// integration relation attached and a limit.
func (d *DB) SearchSearchEnginesByShortPrefix(ctx context.Context, prefix string, limit int64, onlyGeneric bool) ([]SearchEngineFull, error) {
	query := `SELECT ` + searchEngineColumns + `, i.id, i.kind, i.url
		FROM "search_engine" se LEFT JOIN "integration" i ON i.id = se.integration_id
		WHERE se.short LIKE ?`
	args := []any{prefix + "%"}
	if onlyGeneric {
		query += ` AND se.type = 'generic'`
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: search search engines by short prefix: %w", err)
	}
	return scanSearchEnginesFull(rows)
}

// GetUserDefaultSearchEngineID returns user.default_search_engine_id, or
// ErrNotFound when the user row does not exist (the router maps both the
// missing user and a NULL column to "no default").
func (d *DB) GetUserDefaultSearchEngineID(ctx context.Context, userID string) (string, error) {
	const query = `SELECT default_search_engine_id FROM "user" WHERE id = ?`
	var id sql.NullString
	err := d.QueryRowContext(ctx, query, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: get user default search engine id: %w", err)
	}
	return id.String, nil
}

// ClearUserDefaultSearchEngine detaches the engine from every user whose
// default it is, before deletion (searchEngine.delete, search-engine-router.ts:
// 221-231).
func (d *DB) ClearUserDefaultSearchEngine(ctx context.Context, engineID string) error {
	const query = `UPDATE "user" SET default_search_engine_id = NULL WHERE default_search_engine_id = ?`
	if _, err := d.ExecContext(ctx, query, engineID); err != nil {
		return fmt.Errorf("db: clear user default search engine: %w", err)
	}
	return nil
}

const searchEngineColumns = `se.id, se.icon_url, se.name, se.short, se.description, se.url_template, se.type, se.integration_id`

func scanSearchEngine(row *sql.Row) (SearchEngine, error) {
	var (
		engine      SearchEngine
		description sql.NullString
		urlTemplate sql.NullString
		integration sql.NullString
	)
	err := row.Scan(&engine.ID, &engine.IconURL, &engine.Name, &engine.Short,
		&description, &urlTemplate, &engine.Type, &integration)
	if errors.Is(err, sql.ErrNoRows) {
		return SearchEngine{}, ErrNotFound
	}
	if err != nil {
		return SearchEngine{}, err
	}
	engine.Description, engine.URLTemplate, engine.IntegrationID = description.String, urlTemplate.String, integration.String
	return engine, nil
}

func scanSearchEngines(rows *sql.Rows) ([]SearchEngine, error) {
	defer rows.Close()
	var out []SearchEngine
	for rows.Next() {
		var (
			engine      SearchEngine
			description sql.NullString
			urlTemplate sql.NullString
			integration sql.NullString
		)
		if err := rows.Scan(&engine.ID, &engine.IconURL, &engine.Name, &engine.Short,
			&description, &urlTemplate, &engine.Type, &integration); err != nil {
			return nil, fmt.Errorf("db: scan search engines: %w", err)
		}
		engine.Description, engine.URLTemplate, engine.IntegrationID = description.String, urlTemplate.String, integration.String
		out = append(out, engine)
	}
	return out, rows.Err()
}

func scanSearchEnginesFull(rows *sql.Rows) ([]SearchEngineFull, error) {
	defer rows.Close()
	var out []SearchEngineFull
	for rows.Next() {
		var (
			engine           SearchEngineFull
			description      sql.NullString
			urlTemplate      sql.NullString
			integrationID    sql.NullString
			integrationIDRef sql.NullString
			integrationKind  sql.NullString
			integrationURL   sql.NullString
		)
		if err := rows.Scan(&engine.ID, &engine.IconURL, &engine.Name, &engine.Short,
			&description, &urlTemplate, &engine.Type, &integrationID,
			&integrationIDRef, &integrationKind, &integrationURL); err != nil {
			return nil, fmt.Errorf("db: scan search engines with integration: %w", err)
		}
		engine.Description, engine.URLTemplate = description.String, urlTemplate.String
		engine.IntegrationID = integrationID.String
		if integrationID.Valid {
			engine.Integration = &IntegrationRef{
				ID:   integrationIDRef.String,
				Kind: integrationKind.String,
				URL:  integrationURL.String,
			}
		}
		out = append(out, engine)
	}
	return out, rows.Err()
}
