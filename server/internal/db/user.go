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
	// Password is the hashed password (bcrypt format upstream, cost 10:
	// packages/auth/security.ts); empty = NULL (the user cannot sign in with
	// credentials).
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

// UpdateUserPassword replaces the stored password hash of the user, the
// upstream changePassword flow (packages/api/src/router/user.ts:78,153,666,910
// write hashPasswordAsync output into the same column). The hash must be in
// the upstream bcrypt format ($2a$/$2b$, cost 10).
func (d *DB) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	const query = `UPDATE "user" SET password = ? WHERE id = ?`
	if _, err := d.ExecContext(ctx, query, passwordHash, userID); err != nil {
		return fmt.Errorf("db: update user password: %w", err)
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

// ---------------------------------------------------------------------------
// User router repositories (packages/api/src/router/user.ts)
// ---------------------------------------------------------------------------

// ErrInviteInvalid is returned by CreateUserWithInvite when the invite
// delete inside the registration transaction affected zero rows — the
// upstream user.register aborts with FORBIDDEN "Invalid invite"
// (user.ts:168-176).
var ErrInviteInvalid = errors.New("db: invite invalid")

// UserSummary is a user row projected to the list shapes of the user router
// (user.getAll / selectable / search). EmailVerified is zero for NULL.
type UserSummary struct {
	ID            string
	Name          string // empty = NULL
	Email         string // empty = NULL
	EmailVerified time.Time
	Image         string // empty = NULL
	Provider      string
}

// UserPreferencesRow holds the preference columns of the user router's
// preferences shape (userPreferencesSchema, packages/validation/src/user.ts:
// 177-188). HeaderPreferences is the serialized JSON string as stored
// (parsing lives in the router, mirroring parseHeaderPreferences).
type UserPreferencesRow struct {
	ColorScheme               string
	ByteUnitSystem            string
	FirstDayOfWeek            int64
	PingIconsEnabled          bool
	EnableRightClickOnWidgets bool
	HomeBoardID               string // empty = NULL
	MobileHomeBoardID         string // empty = NULL
	DefaultSearchEngineID     string // empty = NULL
	OpenSearchInNewTab        bool
	DdgBangs                  bool
	HeaderPreferences         string
}

// UserDetail is the user.getById profile (user.ts:464-487): the full
// preference set plus identity columns. Password is never selected.
type UserDetail struct {
	ID            string
	Name          string
	Email         string
	EmailVerified time.Time
	Image         string
	Provider      string
	UserPreferencesRow
	CompletedManageTour bool
	CompletedBoardTour  bool
}

const userSummaryColumns = `id, name, email, email_verified, image, provider`

const userPreferencesColumns = `color_scheme, byte_unit_system, first_day_of_week, ping_icons_enabled,
	enable_right_click_on_widgets, home_board_id, mobile_home_board_id, default_search_engine_id,
	open_search_in_new_tab, ddg_bangs, header_preferences`

// ListUsers is user.getAll (user.ts:306-316).
func (d *DB) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+userSummaryColumns+` FROM "user"`)
	if err != nil {
		return nil, fmt.Errorf("db: list users: %w", err)
	}
	return scanUserSummaries(rows)
}

// ListSelectableUsers is user.selectable (user.ts:341-351), optionally
// restricted to the given providers (empty slice = no filter).
func (d *DB) ListSelectableUsers(ctx context.Context, providers []string) ([]UserSummary, error) {
	query := `SELECT ` + userSummaryColumns + ` FROM "user"`
	var args []any
	if len(providers) > 0 {
		query += ` WHERE provider IN (` + placeholders(len(providers)) + `)`
		for _, p := range providers {
			args = append(args, p)
		}
	}
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list selectable users: %w", err)
	}
	return scanUserSummaries(rows)
}

// SearchUsers is user.search (user.ts:381-391): LIKE '%query%' on the name
// with a limit.
func (d *DB) SearchUsers(ctx context.Context, namePattern string, limit int64) ([]UserSummary, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+userSummaryColumns+` FROM "user" WHERE name LIKE ? LIMIT ?`,
		namePattern, limit)
	if err != nil {
		return nil, fmt.Errorf("db: search users: %w", err)
	}
	return scanUserSummaries(rows)
}

// GetUserDetailByID is the user.getById read (user.ts:464-487).
func (d *DB) GetUserDetailByID(ctx context.Context, id string) (UserDetail, error) {
	const query = `SELECT u.id, u.name, u.email, u.email_verified, u.image, u.provider, ` + userPreferencesColumns + `,
		u.completed_manage_tour, u.completed_board_tour
		FROM "user" u WHERE u.id = ?`
	var (
		detail        UserDetail
		name          sql.NullString
		email         sql.NullString
		emailVerified sql.NullInt64
		image         sql.NullString
		homeBoard     sql.NullString
		mobileBoard   sql.NullString
		searchEngine  sql.NullString
	)
	err := d.QueryRowContext(ctx, query, id).Scan(
		&detail.ID, &name, &email, &emailVerified, &image, &detail.Provider,
		&detail.ColorScheme, &detail.ByteUnitSystem, &detail.FirstDayOfWeek,
		&detail.PingIconsEnabled, &detail.EnableRightClickOnWidgets,
		&homeBoard, &mobileBoard, &searchEngine,
		&detail.OpenSearchInNewTab, &detail.DdgBangs, &detail.HeaderPreferences,
		&detail.CompletedManageTour, &detail.CompletedBoardTour)
	if errors.Is(err, sql.ErrNoRows) {
		return UserDetail{}, ErrNotFound
	}
	if err != nil {
		return UserDetail{}, fmt.Errorf("db: get user detail by id: %w", err)
	}
	detail.Name, detail.Email, detail.Image = name.String, email.String, image.String
	if emailVerified.Valid {
		detail.EmailVerified = time.UnixMilli(emailVerified.Int64)
	}
	detail.HomeBoardID, detail.MobileHomeBoardID, detail.DefaultSearchEngineID =
		homeBoard.String, mobileBoard.String, searchEngine.String
	return detail, nil
}

// GetUserPreferences is the preference read of getUserPreferencesAsync
// (packages/api/src/router/user/preferences.ts:26-47).
func (d *DB) GetUserPreferences(ctx context.Context, userID string) (UserPreferencesRow, error) {
	const query = `SELECT ` + userPreferencesColumns + ` FROM "user" WHERE id = ?`
	var (
		prefs        UserPreferencesRow
		homeBoard    sql.NullString
		mobileBoard  sql.NullString
		searchEngine sql.NullString
	)
	err := d.QueryRowContext(ctx, query, userID).Scan(
		&prefs.ColorScheme, &prefs.ByteUnitSystem, &prefs.FirstDayOfWeek,
		&prefs.PingIconsEnabled, &prefs.EnableRightClickOnWidgets,
		&homeBoard, &mobileBoard, &searchEngine,
		&prefs.OpenSearchInNewTab, &prefs.DdgBangs, &prefs.HeaderPreferences)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPreferencesRow{}, ErrNotFound
	}
	if err != nil {
		return UserPreferencesRow{}, fmt.Errorf("db: get user preferences: %w", err)
	}
	prefs.HomeBoardID, prefs.MobileHomeBoardID, prefs.DefaultSearchEngineID =
		homeBoard.String, mobileBoard.String, searchEngine.String
	return prefs, nil
}

// UserPreferencesPatch is the partial preference write of
// updateUserPreferencesAsync (preferences.ts:99-101): nil fields stay
// unchanged; string pointer fields write NULL when empty.
type UserPreferencesPatch struct {
	ColorScheme               *string
	ByteUnitSystem            *string
	FirstDayOfWeek            *int64
	PingIconsEnabled          *bool
	EnableRightClickOnWidgets *bool
	HomeBoardID               *string // empty string = NULL
	MobileHomeBoardID         *string
	DefaultSearchEngineID     *string
	OpenSearchInNewTab        *bool
	DdgBangs                  *bool
	HeaderPreferences         *string
}

// UpdateUserPreferences writes the supplied preference columns in one
// statement (preferences.ts:98-102: "the single preference write").
func (d *DB) UpdateUserPreferences(ctx context.Context, userID string, patch UserPreferencesPatch) error {
	var sets []string
	var args []any
	add := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	addStr := func(column string, value *string) {
		if value == nil {
			return
		}
		add(column, nullableStringArg(*value))
	}
	addBool := func(column string, value *bool) {
		if value == nil {
			return
		}
		if *value {
			add(column, 1)
		} else {
			add(column, 0)
		}
	}
	addStr("color_scheme", patch.ColorScheme)
	addStr("byte_unit_system", patch.ByteUnitSystem)
	if patch.FirstDayOfWeek != nil {
		add("first_day_of_week", *patch.FirstDayOfWeek)
	}
	addBool("ping_icons_enabled", patch.PingIconsEnabled)
	addBool("enable_right_click_on_widgets", patch.EnableRightClickOnWidgets)
	addStr("home_board_id", patch.HomeBoardID)
	addStr("mobile_home_board_id", patch.MobileHomeBoardID)
	addStr("default_search_engine_id", patch.DefaultSearchEngineID)
	addBool("open_search_in_new_tab", patch.OpenSearchInNewTab)
	addBool("ddg_bangs", patch.DdgBangs)
	addStr("header_preferences", patch.HeaderPreferences)
	if len(sets) == 0 {
		return nil
	}
	args = append(args, userID)
	if _, err := d.ExecContext(ctx,
		`UPDATE "user" SET `+joins(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("db: update user preferences: %w", err)
	}
	return nil
}

// UpdateUserProfile is user.editProfile's write (user.ts:550-557): the name
// always updates; email and the emailVerified reset only apply when the
// email changed (emailDirty). Empty email writes NULL.
func (d *DB) UpdateUserProfile(ctx context.Context, id, name, email string, emailDirty bool) error {
	query := `UPDATE "user" SET name = ?`
	args := []any{name}
	if emailDirty {
		query += `, email = ?, email_verified = NULL`
		args = append(args, nullableStringArg(email))
	}
	query += ` WHERE id = ?`
	args = append(args, id)
	if _, err := d.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("db: update user profile: %w", err)
	}
	return nil
}

// UpdateUserImage is user.setProfileImage's write (user.ts:273-278). A nil
// image writes NULL (the null input clears the image).
func (d *DB) UpdateUserImage(ctx context.Context, id string, image *string) error {
	const query = `UPDATE "user" SET image = ? WHERE id = ?`
	var value any
	if image != nil && *image != "" {
		value = *image
	}
	if _, err := d.ExecContext(ctx, query, value, id); err != nil {
		return fmt.Errorf("db: update user image: %w", err)
	}
	return nil
}

// DeleteUser removes the user row (user.delete, user.ts:590).
func (d *DB) DeleteUser(ctx context.Context, id string) error {
	if _, err := d.ExecContext(ctx, `DELETE FROM "user" WHERE id = ?`, id); err != nil {
		return fmt.Errorf("db: delete user: %w", err)
	}
	return nil
}

// UsernameTaken is checkUsernameAlreadyTakenAndThrowAsync (user.ts:922-939):
// the credentials-provider name match, excluding ignoreID when set.
func (d *DB) UsernameTaken(ctx context.Context, nameLower, provider, ignoreID string) (bool, error) {
	const query = `SELECT EXISTS(SELECT 1 FROM "user" WHERE name = ? AND provider = ? AND id <> ?)`
	var exists bool
	if err := d.QueryRowContext(ctx, query, nameLower, provider, ignoreID).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: username taken: %w", err)
	}
	return exists, nil
}

// MarkUserTourCompleted sets one tour flag (user.completeTour, user.ts:
// 867-876).
func (d *DB) MarkUserTourCompleted(ctx context.Context, userID, tour string) error {
	column := map[string]string{"manage": "completed_manage_tour", "board": "completed_board_tour"}[tour]
	if column == "" {
		return fmt.Errorf("db: mark user tour completed: unknown tour %q", tour)
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE "user" SET `+column+` = 1 WHERE id = ?`, userID); err != nil {
		return fmt.Errorf("db: mark user tour completed: %w", err)
	}
	return nil
}

// ResetUserTours clears both tour flags for the caller (user.resetTours,
// user.ts:877-885).
func (d *DB) ResetUserTours(ctx context.Context, userID string) error {
	const query = `UPDATE "user" SET completed_manage_tour = 0, completed_board_tour = 0 WHERE id = ?`
	if _, err := d.ExecContext(ctx, query, userID); err != nil {
		return fmt.Errorf("db: reset user tours: %w", err)
	}
	return nil
}

// GetUserTourStatus reads the tour flags; a missing user yields false/false
// (user.getTourStatus, user.ts:894-905: user?.completedManageTour ?? false).
func (d *DB) GetUserTourStatus(ctx context.Context, userID string) (manage, board bool, err error) {
	const query = `SELECT completed_manage_tour, completed_board_tour FROM "user" WHERE id = ?`
	err = d.QueryRowContext(ctx, query, userID).Scan(&manage, &board)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("db: get user tour status: %w", err)
	}
	return manage, board, nil
}

// CreateUserWithAdminGroup is the user.initUser transaction (user.ts:95-128):
// transition the onboarding row from "user" to nextStep first — aborting
// with a zero count when the step no longer matches — then insert the
// administrator user, the credentials-admin group, its admin permission and
// the membership. The returned count is the onboarding transition row count
// (the router maps 0 to CONFLICT).
func (d *DB) CreateUserWithAdminGroup(ctx context.Context, u User, group Group, nextStep string) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE "onboarding" SET previous_step = 'user', step = ? WHERE step = 'user'`, nextStep)
	if err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	transitioned, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	if transitioned != 1 {
		return transitioned, nil
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
	if u.Provider == "" {
		u.Provider = "credentials"
	}
	if u.ColorScheme == "" {
		u.ColorScheme = "dark"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "user" (id, name, email, email_verified, image, password, provider, color_scheme)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Name, email, nil, image, password, u.Provider, u.ColorScheme); err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "group" (id, name, owner_id, position) VALUES (?, ?, ?, ?)`,
		group.ID, group.Name, u.ID, group.Position); err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "groupPermission" (group_id, permission) VALUES (?, 'admin')`, group.ID); err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, group.ID, u.ID); err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("db: create user with admin group: %w", err)
	}
	return transitioned, nil
}

// CreateUserWithInvite is the user.register transaction (user.ts:162-192):
// insert the invited user, then delete the invite; a zero-row delete rolls
// the insert back and reports ErrInviteInvalid. The caller has already
// verified id + token + expiration.
func (d *DB) CreateUserWithInvite(ctx context.Context, u User, inviteID, inviteToken string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: create user with invite: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
	if u.Provider == "" {
		u.Provider = "credentials"
	}
	if u.ColorScheme == "" {
		u.ColorScheme = "dark"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "user" (id, name, email, email_verified, image, password, provider, color_scheme)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Name, email, nil, image, password, u.Provider, u.ColorScheme); err != nil {
		return fmt.Errorf("db: create user with invite: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM "invite" WHERE id = ? AND token = ?`, inviteID, inviteToken)
	if err != nil {
		return fmt.Errorf("db: create user with invite: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: create user with invite: %w", err)
	}
	if deleted == 0 {
		return ErrInviteInvalid
	}
	return tx.Commit()
}

// CreateGroupDuringOnboarding is the group.createInitialExternalGroup
// transaction (group.ts:193-223): transition onboarding "group" -> "setup"
// (aborting with a zero count when already transitioned), then insert the
// group and its admin permission row.
func (d *DB) CreateGroupDuringOnboarding(ctx context.Context, g Group) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE "onboarding" SET previous_step = 'group', step = 'setup' WHERE step = 'group'`)
	if err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	transitioned, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	if transitioned != 1 {
		return transitioned, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "group" (id, name, owner_id, position) VALUES (?, ?, NULL, ?)`,
		g.ID, g.Name, g.Position); err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "groupPermission" (group_id, permission) VALUES (?, 'admin')`, g.ID); err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("db: create group during onboarding: %w", err)
	}
	return transitioned, nil
}

func scanUserSummaries(rows *sql.Rows) ([]UserSummary, error) {
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var (
			u             UserSummary
			name          sql.NullString
			email         sql.NullString
			emailVerified sql.NullInt64
			image         sql.NullString
		)
		if err := rows.Scan(&u.ID, &name, &email, &emailVerified, &image, &u.Provider); err != nil {
			return nil, fmt.Errorf("db: scan user summaries: %w", err)
		}
		u.Name, u.Email, u.Image = name.String, email.String, image.String
		if emailVerified.Valid {
			u.EmailVerified = time.UnixMilli(emailVerified.Int64)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
