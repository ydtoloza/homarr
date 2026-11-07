// Package onboard is the Go port of the onboard tRPC router
// (packages/api/src/router/onboard/onboard-router.ts): the first-run wizard
// state machine over the onboarding table.
//
// Ported procedures:
//   - onboard.currentStep (publicProcedure): the current/previous step.
//   - onboard.nextStep (onboardingClaimedProcedure): advances start -> user
//     (or setup when a user already exists).
//
// Deferred procedures (ADMIN.md §6):
//   - onboard.testIntegration and onboard.completeSetup need the
//     integrations domain (connection tests, secret encryption, the widget
//     defaults and integration maps of @homarr/definitions).
//   - onboard.detectRuntimeCapabilities needs the docker/workshop
//     environment probes.
//   - onboard.discoverDockerServices needs the Docker Engine API discovery.
//
// They are intentionally NOT registered, exactly like the deferred board
// procedures (BOARDS.md §6): callers get the standard NOT_FOUND envelope.
//
// The package also hosts ClaimHandler (claimrest.go): the REST route
// POST /api/onboarding/claim that MINTS the onboarding claim the tRPC gates
// verify. It is a plain http.Handler, mounted by the integrator behind the
// auth middleware (ADMIN.md §3.5).
package onboard

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options configures the onboard router.
type Options struct {
	// OnboardingClaimTokenFromContext resolves the onboarding claim token
	// (cookie "homarr-onboarding-claim") for onboard.nextStep, which runs
	// behind onboardingClaimedProcedure. The httpserver middleware injects
	// the cookie value; nil or empty means "no claim".
	OnboardingClaimTokenFromContext func(ctx context.Context) string
}

// OnboardingClaimCookieName re-exports admin.OnboardingClaimCookieName for
// integrators outside the router subtree (cmd/homarr-go), which cannot
// import the router-internal admin package.
const OnboardingClaimCookieName = admin.OnboardingClaimCookieName

func (o *Options) claimToken(ctx context.Context) string {
	if o != nil && o.OnboardingClaimTokenFromContext != nil {
		return o.OnboardingClaimTokenFromContext(ctx)
	}
	return ""
}

type service struct {
	db   *db.DB
	opts *Options
}

// Register registers the ported onboard procedures. Call once at startup.
func Register(r *trpc.Router, database *db.DB, opts Options) {
	s := &service{db: database, opts: &opts}
	// onboard.currentStep (onboard-router.ts:140): publicProcedure.
	r.Register("onboard.currentStep", s.currentStep)
	// onboard.nextStep (onboard-router.ts:142-148):
	// onboardingClaimedProcedure (mutation).
	r.Register("onboard.nextStep", admin.Writable(s.nextStep))
}

// currentStep is onboard.currentStep: getOnboardingOrFallbackAsync
// (onboard-queries.ts:14-22) — a missing row behaves like "start".
func (s *service) currentStep(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	state, err := admin.GetOnboardingState(ctx, s.db)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	var previous any
	if state.Previous != nil {
		previous = *state.Previous
	}
	return map[string]any{"current": state.Current, "previous": previous}, nil
}

// nextStep is onboard.nextStep: the claim gate (enforceOnboardingAccess,
// packages/api/src/trpc.ts:148-159) and the start-step transition
// (onboard-router.ts:142-148).
func (s *service) nextStep(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	allowed, err := admin.OnboardingAccessAllowed(ctx, s.db, c, s.opts.claimToken(ctx))
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if !allowed {
		return nil, trpc.NewError(trpc.CodeForbidden, "This onboarding session is not claimed.")
	}

	state, err := admin.GetOnboardingState(ctx, s.db)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if state.Current != admin.OnboardingStepStart {
		return nil, trpc.NewError(trpc.CodePreconditionFailed, "The welcome step is already complete.")
	}
	nextStep, err := admin.NextOnboardingStep(ctx, s.db)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	// nextOnboardingStepAsync (onboard-queries.ts:6-12): update every row
	// (the single seeded one) with previousStep = start.
	if _, err := s.db.UpdateOnboardingAll(ctx, nextStep, admin.OnboardingStepStart); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}
