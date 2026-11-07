package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Repositories extending "apiKey" for the apiKeys tRPC router
// (packages/api/src/router/apiKeys.ts). Creation and by-id lookup live in
// account.go (they serve the auth layer); this file adds the listings and
// deletions the router needs.

// APIKeyOAuth is one row of apiKeys.getOwnOAuth / apiKeys.getAll
// (apiKeys.ts:12-19): the key without its hash plus the OAuth provenance
// columns added by migrations 0049/0052.
type APIKeyOAuth struct {
	ID string
	// ReadOnly mirrors the read_only column.
	ReadOnly bool
	// OAuthClientID is empty for NULL (the key was not issued through an
	// OAuth client flow).
	OAuthClientID    string
	OAuthClientName  string // empty = NULL
	OAuthRedirectURI string // empty = NULL
	// OAuthIssuedAt is zero for NULL.
	OAuthIssuedAt time.Time
}

// APIKeyUser is the user projection joined into apiKeys.getAll
// (apiKeys.ts:50-59): {id, name, image, email}.
type APIKeyUser struct {
	ID    string
	Name  string // empty = NULL
	Image string // empty = NULL
	Email string // empty = NULL
}

// APIKeyWithUser is one apiKeys.getAll row: the OAuth projection plus the
// owning user (apiKeys.ts:50-59).
type APIKeyWithUser struct {
	APIKeyOAuth
	User APIKeyUser
}

const apiKeyOAuthColumns = `k.id, k.read_only, k.oauth_client_id, k.oauth_client_name, k.oauth_redirect_uri, k.oauth_issued_at`

// ListOAuthAPIKeysForUser returns the caller's keys that carry an
// oauth_client_id (apiKeys.getOwnOAuth, apiKeys.ts:22-27).
func (d *DB) ListOAuthAPIKeysForUser(ctx context.Context, userID string) ([]APIKeyOAuth, error) {
	const query = `SELECT ` + apiKeyOAuthColumns + ` FROM "apiKey" k
		WHERE k.user_id = ? AND k.oauth_client_id IS NOT NULL`
	rows, err := d.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("db: list oauth api keys for user: %w", err)
	}
	return scanAPIKeyOAuth(rows)
}

// ListAPIKeysWithUser returns every API key with its owning user
// (apiKeys.getAll, apiKeys.ts:42-61).
func (d *DB) ListAPIKeysWithUser(ctx context.Context) ([]APIKeyWithUser, error) {
	const query = `SELECT ` + apiKeyOAuthColumns + `, u.id, u.name, u.image, u.email
		FROM "apiKey" k JOIN "user" u ON u.id = k.user_id`
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("db: list api keys with user: %w", err)
	}
	defer rows.Close()
	var out []APIKeyWithUser
	for rows.Next() {
		var (
			row        APIKeyWithUser
			clientID   sql.NullString
			clientName sql.NullString
			redirect   sql.NullString
			issuedAt   sql.NullInt64
			name       sql.NullString
			image      sql.NullString
			email      sql.NullString
		)
		if err := rows.Scan(&row.ID, &row.ReadOnly, &clientID, &clientName,
			&redirect, &issuedAt, &row.User.ID, &name, &image, &email); err != nil {
			return nil, fmt.Errorf("db: list api keys with user: %w", err)
		}
		row.OAuthClientID, row.OAuthClientName, row.OAuthRedirectURI = clientID.String, clientName.String, redirect.String
		if issuedAt.Valid {
			row.OAuthIssuedAt = time.UnixMilli(issuedAt.Int64)
		}
		row.User.Name, row.User.Image, row.User.Email = name.String, image.String, email.String
		out = append(out, row)
	}
	return out, rows.Err()
}

// DeleteOAuthAPIKeyOfUser deletes one of the caller's OAuth keys
// (apiKeys.revokeOwnOAuth, apiKeys.ts:28-41: id + userId + oauthClientId
// IS NOT NULL). It reports whether a row was deleted.
func (d *DB) DeleteOAuthAPIKeyOfUser(ctx context.Context, id, userID string) (bool, error) {
	const query = `DELETE FROM "apiKey" WHERE id = ? AND user_id = ? AND oauth_client_id IS NOT NULL`
	res, err := d.ExecContext(ctx, query, id, userID)
	if err != nil {
		return false, fmt.Errorf("db: delete oauth api key of user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: delete oauth api key of user: %w", err)
	}
	return n > 0, nil
}

// DeleteAPIKeyByID deletes any API key by id (apiKeys.delete, apiKeys.ts:
// 85-96). It reports whether a row was deleted.
func (d *DB) DeleteAPIKeyByID(ctx context.Context, id string) (bool, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM "apiKey" WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("db: delete api key by id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: delete api key by id: %w", err)
	}
	return n > 0, nil
}

func scanAPIKeyOAuth(rows *sql.Rows) ([]APIKeyOAuth, error) {
	defer rows.Close()
	var out []APIKeyOAuth
	for rows.Next() {
		var (
			key        APIKeyOAuth
			clientID   sql.NullString
			clientName sql.NullString
			redirect   sql.NullString
			issuedAt   sql.NullInt64
		)
		if err := rows.Scan(&key.ID, &key.ReadOnly, &clientID, &clientName,
			&redirect, &issuedAt); err != nil {
			return nil, fmt.Errorf("db: scan oauth api keys: %w", err)
		}
		key.OAuthClientID, key.OAuthClientName, key.OAuthRedirectURI = clientID.String, clientName.String, redirect.String
		if issuedAt.Valid {
			key.OAuthIssuedAt = time.UnixMilli(issuedAt.Int64)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
