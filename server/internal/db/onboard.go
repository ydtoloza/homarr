package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repositories for the "onboarding" table plus the user-existence checks
// the onboarding step resolution needs, serving the onboard tRPC router
// (packages/api/src/router/onboard) and the onboarding gates of the user
// and group routers. The table holds a single seeded row
// (packages/db/migrations/seed.ts inserts it with step "start").

// OnboardingRow maps to the upstream "onboarding" table
// (packages/db/schema/sqlite.ts, export onboarding). PreviousStep is empty
// for NULL.
type OnboardingRow struct {
	ID           string
	Step         string
	PreviousStep string
}

// GetOnboardingRow returns the first (only) onboarding row, or ErrNotFound
// when the row is missing (getOnboardingOrFallbackAsync falls back to
// "start" in that case; the fallback decision lives in
// internal/api/router/internal/admin).
func (d *DB) GetOnboardingRow(ctx context.Context) (OnboardingRow, error) {
	const query = `SELECT id, step, previous_step FROM "onboarding" LIMIT 1`
	var (
		row      OnboardingRow
		previous sql.NullString
	)
	err := d.QueryRowContext(ctx, query).Scan(&row.ID, &row.Step, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return OnboardingRow{}, ErrNotFound
	}
	if err != nil {
		return OnboardingRow{}, fmt.Errorf("db: get onboarding row: %w", err)
	}
	row.PreviousStep = previous.String
	return row, nil
}

// TransitionOnboardingStep moves the wizard from one step to another and
// reports whether exactly one row transitioned. The upstream procedures
// require rowCount === 1 and answer CONFLICT otherwise
// (user.initUser user.ts:98-104, group.createInitialExternalGroup
// group.ts:196-202, onboard.completeSetup onboard-router.ts:1010-1017).
func (d *DB) TransitionOnboardingStep(ctx context.Context, from, to, previous string) (int64, error) {
	const query = `UPDATE "onboarding" SET previous_step = ?, step = ? WHERE step = ?`
	res, err := d.ExecContext(ctx, query, previous, to, from)
	if err != nil {
		return 0, fmt.Errorf("db: transition onboarding step: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: transition onboarding step: %w", err)
	}
	return n, nil
}

// UpdateOnboardingAll is nextOnboardingStepAsync's update
// (onboard-queries.ts:11): `db.update(onboarding).set(...)` without a WHERE
// clause — every row (the single seeded one) is updated. It reports the
// affected rows; upstream does not check the count here.
func (d *DB) UpdateOnboardingAll(ctx context.Context, step, previous string) (int64, error) {
	const query = `UPDATE "onboarding" SET previous_step = ?, step = ?`
	res, err := d.ExecContext(ctx, query, previous, step)
	if err != nil {
		return 0, fmt.Errorf("db: update onboarding: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: update onboarding: %w", err)
	}
	return n, nil
}

// HasAnyUser reports whether any user row exists. It serves the onboarding
// fallback (onboard-queries.ts:25-28: users.findFirst) and the claim-only
// onboarding access check (packages/api/src/onboarding-claim.ts:69-76).
func (d *DB) HasAnyUser(ctx context.Context) (bool, error) {
	const query = `SELECT EXISTS(SELECT 1 FROM "user")`
	var exists bool
	if err := d.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return false, fmt.Errorf("db: has any user: %w", err)
	}
	return exists, nil
}
