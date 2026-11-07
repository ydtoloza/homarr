package db

import (
	"context"
	"fmt"
)

// Repositories for the onboarding claim row (__onboarding_claim in the
// serverSetting table), serving the claim issuance of
// internal/api/router/internal/admin (claimOnboardingAsync,
// packages/api/src/onboarding-claim.ts:78-117). Reading the row is done with
// GetServerSetting; only the write the issuance needs lives here.

// DeleteServerSettingIfValue removes a setting row only while it still holds
// the expected value. claimOnboardingAsync uses it as an optimistic lock
// before re-issuing the claim (onboarding-claim.ts:97-101): a concurrent
// minter's row (a different value) is left alone, so the loser of the race
// observes the winner's claim at the insert step and answers "locked".
func (d *DB) DeleteServerSettingIfValue(ctx context.Context, key, value string) error {
	const query = `DELETE FROM "serverSetting" WHERE setting_key = ? AND value = ?`
	if _, err := d.ExecContext(ctx, query, key, value); err != nil {
		return fmt.Errorf("db: delete server setting if value: %w", err)
	}
	return nil
}
