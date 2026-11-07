package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repositories for the "invite" table, serving the invite tRPC router
// (packages/api/src/router/invite.ts) and the invite-based registration of
// the user router. The drizzle column mode is timestamp (SECONDS epoch).

// Invite maps to the upstream "invite" table (packages/db/schema/sqlite.ts,
// export invites).
type Invite struct {
	ID string
	// Token is the plain registration token: upstream stores it
	// un-hashed (randomBytes(20).toString("hex"), invite.ts:86) and the
	// register procedure matches on id + token equality.
	Token string
	// ExpirationDate is the expiration timestamp (seconds epoch).
	ExpirationDate time.Time
	CreatorID      string
}

// InviteWithCreator is the invite.getAll row: the invite without its token
// plus the creator name (invite.ts:41-57, columns.token: false).
type InviteWithCreator struct {
	ID             string
	ExpirationDate time.Time
	CreatorID      string
	CreatorName    string // empty = NULL
}

// CreateInvite inserts an invite row (invite.createInvite).
func (d *DB) CreateInvite(ctx context.Context, invite Invite) error {
	const query = `INSERT INTO "invite" (id, token, expiration_date, creator_id) VALUES (?, ?, ?, ?)`
	if _, err := d.ExecContext(ctx, query,
		invite.ID, invite.Token, invite.ExpirationDate.Unix(), invite.CreatorID); err != nil {
		return fmt.Errorf("db: create invite: %w", err)
	}
	return nil
}

// ListInvitesWithCreator returns the invites ordered by expiration date
// (invite.getAll: orderBy asc(invites.expirationDate)) with the creator
// name joined. The token is never selected, mirroring columns.token: false.
func (d *DB) ListInvitesWithCreator(ctx context.Context) ([]InviteWithCreator, error) {
	const query = `SELECT i.id, i.expiration_date, i.creator_id, u.name
		FROM "invite" i LEFT JOIN "user" u ON u.id = i.creator_id
		ORDER BY i.expiration_date ASC`
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("db: list invites: %w", err)
	}
	defer rows.Close()
	var out []InviteWithCreator
	for rows.Next() {
		var (
			inv        InviteWithCreator
			expiration int64
			name       sql.NullString
		)
		if err := rows.Scan(&inv.ID, &expiration, &inv.CreatorID, &name); err != nil {
			return nil, fmt.Errorf("db: list invites: %w", err)
		}
		inv.ExpirationDate = time.Unix(expiration, 0)
		inv.CreatorName = name.String
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetInviteByID returns the invite with the given id, or ErrNotFound
// (invite.deleteInvite looks the row up before deleting).
func (d *DB) GetInviteByID(ctx context.Context, id string) (Invite, error) {
	const query = `SELECT id, token, expiration_date, creator_id FROM "invite" WHERE id = ?`
	return scanInvite(d.QueryRowContext(ctx, query, id))
}

// GetInviteByIDAndToken returns the invite matching both id and token —
// the user.register lookup (user.ts:135-142, and(eq(invites.id),
// eq(invites.token))). No match returns ErrNotFound.
func (d *DB) GetInviteByIDAndToken(ctx context.Context, id, token string) (Invite, error) {
	const query = `SELECT id, token, expiration_date, creator_id FROM "invite" WHERE id = ? AND token = ?`
	return scanInvite(d.QueryRowContext(ctx, query, id, token))
}

// DeleteInvite removes the invite row and reports whether a row was
// deleted. user.register relies on the count: a zero-row delete inside the
// registration transaction aborts it with FORBIDDEN "Invalid invite"
// (user.ts:168-176). invite.deleteInvite ignores the count (it checked the
// row before).
func (d *DB) DeleteInvite(ctx context.Context, id string) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM "invite" WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("db: delete invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: delete invite: %w", err)
	}
	return n, nil
}

func scanInvite(row *sql.Row) (Invite, error) {
	var invite Invite
	var expiration int64
	err := row.Scan(&invite.ID, &invite.Token, &expiration, &invite.CreatorID)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	invite.ExpirationDate = time.Unix(expiration, 0)
	return invite, nil
}
