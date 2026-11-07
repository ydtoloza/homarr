package admin

import (
	"context"
	"errors"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// Onboarding step resolution, ported from
// packages/api/src/router/onboard/onboard-queries.ts. Shared by the onboard
// router and by user.initUser / group.createInitialExternalGroup, which run
// behind onboardingProcedure gates.

// Onboarding steps (packages/definitions/src/onboarding.ts:1).
const (
	OnboardingStepStart  = "start"
	OnboardingStepUser   = "user"
	OnboardingStepGroup  = "group"
	OnboardingStepSetup  = "setup"
	OnboardingStepFinish = "finish"
)

// OnboardingState is the output of getOnboardingOrFallbackAsync
// (onboard-queries.ts:14-22).
type OnboardingState struct {
	Current  string
	Previous *string // nil when no previous step is recorded
}

// NormalizeOnboardingStep is normalizeOnboardingStep
// (onboard-queries.ts:31-35): unknown values fall back to "start"; the
// retired "settings"/"integrations" steps map to "setup".
func NormalizeOnboardingStep(step string) string {
	switch step {
	case OnboardingStepUser, OnboardingStepGroup, OnboardingStepSetup, OnboardingStepFinish:
		return step
	case "settings", "integrations":
		return OnboardingStepSetup
	default:
		return OnboardingStepStart
	}
}

// GetOnboardingState is getOnboardingOrFallbackAsync (onboard-queries.ts:
// 14-22): a missing row behaves like a fresh installation ("start").
func GetOnboardingState(ctx context.Context, database *db.DB) (OnboardingState, error) {
	row, err := database.GetOnboardingRow(ctx)
	if errors.Is(err, db.ErrNotFound) {
		return OnboardingState{Current: OnboardingStepStart}, nil
	}
	if err != nil {
		return OnboardingState{}, err
	}
	state := OnboardingState{Current: NormalizeOnboardingStep(row.Step)}
	if row.PreviousStep != "" {
		previous := NormalizeOnboardingStep(row.PreviousStep)
		state.Previous = &previous
	}
	return state, nil
}

// NextOnboardingStep is getNextOnboardingStepAsync (onboard-queries.ts:24-29)
// with the fork provider assumptions: homarr-go ships the credentials
// provider only (internal/auth), so the ldap/oidc branches are unreachable.
//   - credentials enabled and no user yet -> "user" (first administrator)
//   - otherwise -> "setup"
func NextOnboardingStep(ctx context.Context, database *db.DB) (string, error) {
	hasUser, err := database.HasAnyUser(ctx)
	if err != nil {
		return "", err
	}
	if !hasUser {
		return OnboardingStepUser, nil
	}
	return OnboardingStepSetup, nil
}
