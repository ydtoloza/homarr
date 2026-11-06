package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row does not exist. Callers (auth, api) map
// it to their own "not found" semantics.
var ErrNotFound = errors.New("db: row not found")

// User maps to the upstream "user" table (drizzle schema packages/db/schema/
// sqlite.ts, export users). Only the columns needed for M1 (login + session
// rendering) are loaded; the remaining columns keep upstream defaults.
//
// Upstream stores user names lowercased (packages/auth/.../basic-authorization.ts
// queries name = credentials.name.toLowerCase()); callers must pass
// already-lowercased names to GetUserByName.
type User struct {
	ID            string
	Name          string
	Email         string    // empty = NULL
	EmailVerified time.Time // zero = NULL; stored as epoch millis
	Image         string    // empty = NULL
	// Password is the hashed password (argon2id format upstream); empty = NULL
	// (the user cannot sign in with credentials).
	Password string
	// Provider is the auth provider ("credentials", "oidc", "ldap", ...).
	Provider string
	// ColorScheme is "light", "dark" or "system" (default "dark").
	ColorScheme string
}

const userColumns = `id, name, email, email_verified, image, password, provider, color_scheme`

// CreateUser inserts a user. The id is caller-supplied (upstream generates
// cuid2 ids in application code); columns with upstream defaults (booleans,
// header preferences, tours, ...) are left to the database. Provider and
// color scheme fall back to the upstream defaults ("credentials" and "dark",
// from the drizzle schema defaults) when empty, because both columns are NOT
// NULL and the caller usually leaves them unset.
func (d *DB) CreateUser(ctx context.Context, u User) error {
	const query = `INSERT INTO "user" (id, name, email, email_verified, image, password, provider, color_scheme)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if u.Provider == "" {
		u.Provider = "credentials"
	}
	if u.ColorScheme == "" {
		u.ColorScheme = "dark"
	}
	var email, image, password any
	if u.Email != "" {
		email = u.Email
	}
	if u.Image != "" {
		image = u.Image
	}
	if u.Password != "" {
		password = u.Password
	}
	var emailVerified any
	if !u.EmailVerified.IsZero() {
		emailVerified = u.EmailVerified.UnixMilli()
	}
	_, err := d.ExecContext(ctx, query,
		u.ID, u.Name, email, emailVerified, image, password, u.Provider, u.ColorScheme)
	if err != nil {
		return fmt.Errorf("db: create user: %w", err)
	}
	return nil
}

// GetUserByID returns the user with the given id.
func (d *DB) GetUserByID(ctx context.Context, id string) (User, error) {
	const query = `SELECT ` + userColumns + ` FROM "user" WHERE id = ?`
	u, err := scanUser(d.QueryRowContext(ctx, query, id))
	if err != nil {
		return User{}, fmt.Errorf("db: get user by id: %w", err)
	}
	return u, nil
}

// GetUserByName returns a user by lowercased name and provider, mirroring the
// upstream credentials sign-in lookup (name = lowercased input AND
// provider = "credentials").
func (d *DB) GetUserByName(ctx context.Context, nameLower, provider string) (User, error) {
	const query = `SELECT ` + userColumns + ` FROM "user" WHERE name = ? AND provider = ?`
	u, err := scanUser(d.QueryRowContext(ctx, query, nameLower, provider))
	if err != nil {
		return User{}, fmt.Errorf("db: get user by name: %w", err)
	}
	return u, nil
}

func scanUser(row *sql.Row) (User, error) {
	var (
		u             User
		name          sql.NullString
		email         sql.NullString
		emailVerified sql.NullInt64
		image         sql.NullString
		password      sql.NullString
		colorScheme   sql.NullString
	)
	err := row.Scan(&u.ID, &name, &email, &emailVerified, &image, &password, &u.Provider, &colorScheme)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Name = name.String
	u.Email = email.String
	if emailVerified.Valid {
		u.EmailVerified = time.UnixMilli(emailVerified.Int64)
	}
	u.Image = image.String
	u.Password = password.String
	if colorScheme.Valid {
		u.ColorScheme = colorScheme.String
	}
	return u, nil
}
