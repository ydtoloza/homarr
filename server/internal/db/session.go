package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session maps to the upstream "session" table (NextAuth database session
// strategy: packages/auth/configuration.ts sets strategy: "database"; the
// session token is a 48-character secure random string from
// packages/auth/session.ts).
type Session struct {
	// Token is the session_token primary key (cookie value).
	Token  string
	UserID string
	// Expires is stored as epoch milliseconds (drizzle timestamp_ms mode).
	Expires time.Time
}

// CreateSession inserts a session row. The token is caller-supplied, like
// upstream (generateSessionToken in packages/auth/session.ts).
func (d *DB) CreateSession(ctx context.Context, s Session) error {
	const query = `INSERT INTO "session" (session_token, user_id, expires) VALUES (?, ?, ?)`
	_, err := d.ExecContext(ctx, query, s.Token, s.UserID, s.Expires.UnixMilli())
	if err != nil {
		return fmt.Errorf("db: create session: %w", err)
	}
	return nil
}

// SessionWithUser is a session joined with its user, the shape needed to
// render an authenticated request (upstream getSessionFromTokenAsync in
// packages/auth/session.ts joins the user and returns id, name, email, image
// and colorScheme).
type SessionWithUser struct {
	Session Session
	User    User
}

// GetSession returns the session with the given token and its user. Expired
// sessions are returned as-is: expiry checking is the caller's concern (the
// auth layer), matching upstream which reads the row and compares expires in
// application code.
func (d *DB) GetSession(ctx context.Context, token string) (SessionWithUser, error) {
	const query = `SELECT s.session_token, s.user_id, s.expires,
		u.id, u.name, u.email, u.email_verified, u.image, u.password, u.provider, u.color_scheme
		FROM "session" s
		JOIN "user" u ON u.id = s.user_id
		WHERE s.session_token = ?`
	var (
		s             SessionWithUser
		expires       int64
		name          sql.NullString
		email         sql.NullString
		emailVerified sql.NullInt64
		image         sql.NullString
		password      sql.NullString
		colorScheme   sql.NullString
	)
	err := d.QueryRowContext(ctx, query, token).Scan(
		&s.Session.Token, &s.Session.UserID, &expires,
		&s.User.ID, &name, &email, &emailVerified, &image, &password, &s.User.Provider, &colorScheme,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionWithUser{}, ErrNotFound
	}
	if err != nil {
		return SessionWithUser{}, fmt.Errorf("db: get session: %w", err)
	}
	s.Session.Expires = time.UnixMilli(expires)
	s.User.Name = name.String
	s.User.Email = email.String
	if emailVerified.Valid {
		s.User.EmailVerified = time.UnixMilli(emailVerified.Int64)
	}
	s.User.Image = image.String
	s.User.Password = password.String
	if colorScheme.Valid {
		s.User.ColorScheme = colorScheme.String
	}
	return s, nil
}

// DeleteSession removes the session with the given token (sign-out).
func (d *DB) DeleteSession(ctx context.Context, token string) error {
	const query = `DELETE FROM "session" WHERE session_token = ?`
	if _, err := d.ExecContext(ctx, query, token); err != nil {
		return fmt.Errorf("db: delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions whose expires is before now, the
// cleanup NextAuth performs via deleteSession callbacks upstream.
func (d *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	const query = `DELETE FROM "session" WHERE expires < ?`
	res, err := d.ExecContext(ctx, query, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("db: delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: delete expired sessions: %w", err)
	}
	return n, nil
}
