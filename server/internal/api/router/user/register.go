// Package user is the Go port of the user tRPC router
// (packages/api/src/router/user.ts + router/user/preferences.ts): account
// CRUD, profile, password change, tours and the per-user preferences.
//
// Permission semantics per procedure follow the upstream middlewares:
// admin-only CRUD (permissionRequiredProcedure), self-service profile and
// preference procedures (protectedProcedure with self-or-admin checks),
// onboarding-gated initUser.
package user

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// credentialsProvider is the only auth provider the fork ships
// (server/internal/auth); upstream env.AUTH_PROVIDERS defaults to it.
const credentialsProvider = "credentials"

// credentialsAdminGroup is credentialsAdminGroup
// (packages/definitions/src/group.ts:2): the group user.initUser creates
// for the first administrator.
const credentialsAdminGroup = "credentials-admin"

// Options configures the user router.
type Options struct {
	// DemoMode mirrors env.DEMO_MODE (isDemoMode, packages/api/src/trpc.ts:
	// 114): user.delete / changePassword / editProfile carry demo-mode
	// FORBIDDEN branches upstream. The fork runs with demo mode off.
	DemoMode bool
	// OnboardingClaimTokenFromContext resolves the onboarding claim token
	// (cookie "homarr-onboarding-claim") for user.initUser, which runs
	// behind onboardingProcedure.
	OnboardingClaimTokenFromContext func(ctx context.Context) string
}

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

// Register registers the user router under its upstream paths. Call once at
// startup (Register panics on duplicate paths).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	s := &service{db: database, opts: &opts}

	// user.initUser (user.ts:70-129): onboardingProcedure "user" step.
	r.Register("user.initUser", s.initUser)
	// user.register (user.ts:130-193): publicProcedure, invite-based.
	r.Register("user.register", s.register)
	// user.create (user.ts:194-224): admin.
	r.Register("user.create", s.create)
	// user.setProfileImage (user.ts:225-279): protected, self-or-admin.
	r.Register("user.setProfileImage", s.setProfileImage)
	// user.getAll (user.ts:280-316): admin.
	r.Register("user.getAll", s.getAll)
	// user.selectable (user.ts:318-351): protected.
	r.Register("user.selectable", s.selectable)
	// user.search (user.ts:352-398): admin.
	r.Register("user.search", s.search)
	// user.getPreferences (user.ts:399-418): protected.
	r.Register("user.getPreferences", s.getPreferences)
	// user.getById (user.ts:419-497): protected, self-or-admin.
	r.Register("user.getById", s.getById)
	// user.editProfile (user.ts:498-558): protected, self-or-admin.
	r.Register("user.editProfile", s.editProfile)
	// user.delete (user.ts:559-591): protected, self-or-admin.
	r.Register("user.delete", s.delete)
	// user.changePassword (user.ts:592-673): protected, self-or-admin.
	r.Register("user.changePassword", s.changePassword)
	// user.updatePreferences (user.ts:674-704): protected.
	r.Register("user.updatePreferences", s.updatePreferences)
	// user.changeHomeBoards (user.ts:705-724).
	r.Register("user.changeHomeBoards", s.changeHomeBoards)
	// user.changeDefaultSearchEngine (user.ts:725-749, deprecated).
	r.Register("user.changeDefaultSearchEngine", s.changeDefaultSearchEngine)
	// user.changeSearchPreferences (user.ts:750-770).
	r.Register("user.changeSearchPreferences", s.changeSearchPreferences)
	// user.changeColorScheme (user.ts:771-786).
	r.Register("user.changeColorScheme", s.changeColorScheme)
	// user.changeByteUnitSystem (user.ts:787-792).
	r.Register("user.changeByteUnitSystem", s.changeByteUnitSystem)
	// user.changeEnableRightClickOnWidgets (user.ts:793-809).
	r.Register("user.changeEnableRightClickOnWidgets", s.changeEnableRightClickOnWidgets)
	// user.changePingIconsEnabled (user.ts:810-814).
	r.Register("user.changePingIconsEnabled", s.changePingIconsEnabled)
	// user.changeHeaderPreferences (user.ts:815-831).
	r.Register("user.changeHeaderPreferences", s.changeHeaderPreferences)
	// user.changeDdgBangs (user.ts:832-849, deprecated).
	r.Register("user.changeDdgBangs", s.changeDdgBangs)
	// user.changeFirstDayOfWeek (user.ts:850-866).
	r.Register("user.changeFirstDayOfWeek", s.changeFirstDayOfWeek)
	// user.completeTour (user.ts:867-876).
	r.Register("user.completeTour", s.completeTour)
	// user.resetTours (user.ts:877-885).
	r.Register("user.resetTours", s.resetTours)
	// user.getTourStatus (user.ts:886-906).
	r.Register("user.getTourStatus", s.getTourStatus)
}

// --- shared validation (packages/validation/src/user.ts) ---------------------

// usernameSchema is usernameSchema (user.ts:48): trim + lowercase +
// min 3 + max 255.
func validateUsername(value string) (string, *trpc.Error) {
	username := strings.ToLower(strings.TrimSpace(value))
	if len(username) < 3 || len(username) > 255 {
		return "", admin.BadRequestf(`input key "username" must be between 3 and 255 characters`)
	}
	return username, nil
}

// userPasswordSchema is userPasswordSchema (user.ts:62): 8-255.
func validatePassword(value, key string) (string, *trpc.Error) {
	if len(value) < 8 || len(value) > 255 {
		return "", admin.BadRequestf(`input key "%s" must be between 8 and 255 characters`, key)
	}
	return value, nil
}

// optionalEmailSchema is optionalEmailSchema
// (packages/validation/src/email.ts:7): absent, "" or a valid email. The
// upstream z.email() pattern is the full unicode email regex; the port
// checks the structural minimum (local@domain) and documents the deviation
// in ADMIN.md.
func validateOptionalEmail(value, key string) (string, *trpc.Error) {
	if value == "" {
		return "", nil
	}
	if !isEmailAddress(value) {
		return "", admin.BadRequestf(`input key "%s" must be a valid email`, key)
	}
	return value, nil
}

// nullableEmailSchema is nullableEmailSchema (email.ts:10-13): "" becomes
// null.
func validateNullableEmail(value, key string) (string, *trpc.Error) {
	if value == "" {
		return "", nil
	}
	if !isEmailAddress(value) {
		return "", admin.BadRequestf(`input key "%s" must be a valid email`, key)
	}
	return value, nil
}

// isEmailAddress is the structural minimum of z.email(): exactly one @,
// non-empty local part, non-empty domain with a dot.
func isEmailAddress(value string) bool {
	at := strings.IndexByte(value, '@')
	if at <= 0 || at != strings.LastIndexByte(value, '@') {
		return false
	}
	local, domain := value[:at], value[at+1:]
	if local == "" || domain == "" || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// profileImagePattern is the setProfileImage regex (user.ts:243): a base64
// data URL of an allowed image type.
var profileImagePattern = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp);base64,[A-Za-z0-9/+]+=*$`)

// colorSchemes is colorSchemes (packages/definitions/src/user.ts:1).
var colorSchemes = []string{"auto", "light", "dark"}

// byteUnitSystems is userByteUnitSystemSchema (user.ts:157-159).
var byteUnitSystems = []string{"binary", "decimal"}

// --- shared guards -----------------------------------------------------------

// requireSelfOrAdmin is the pattern of getById/setProfileImage/editProfile/
// delete (user.ts:458-463,250-255,514-519,583-588): another user without
// the admin permission gets FORBIDDEN with the procedure's message.
func requireSelfOrAdmin(c admin.Caller, targetUserID, message string) *trpc.Error {
	if c.Session.UserID != targetUserID && !c.IsAdmin() {
		return trpc.NewError(trpc.CodeForbidden, message)
	}
	return nil
}

// requirePreferencesTarget is the getUserPreferencesAsync guard
// (packages/api/src/router/user/preferences.ts:27-29): another user without
// admin hides behind NOT_FOUND "User not found".
func requirePreferencesTarget(c admin.Caller, targetUserID string) *trpc.Error {
	if c.Session.UserID != targetUserID && !c.IsAdmin() {
		return trpc.NewError(trpc.CodeNotFound, "User not found")
	}
	return nil
}

// requireOnboardingStep is onboardingProcedure.requiresStep
// (packages/api/src/trpc.ts:209-223) for user.initUser.
func (s *service) requireOnboardingStep(ctx context.Context, step string) *trpc.Error {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return terr
	}
	allowed, err := admin.OnboardingAccessAllowed(ctx, s.db, c, s.opts.claimToken(ctx))
	if err != nil {
		return admin.InternalError(err)
	}
	if !allowed {
		return trpc.NewError(trpc.CodeForbidden, "This onboarding session is not claimed.")
	}
	state, err := admin.GetOnboardingState(ctx, s.db)
	if err != nil {
		return admin.InternalError(err)
	}
	if state.Current != step {
		return trpc.NewError(trpc.CodeForbidden, "Step denied")
	}
	return nil
}

// checkUsernameAlreadyTaken is checkUsernameAlreadyTakenAndThrowAsync
// (user.ts:922-939): CONFLICT "Username already taken" on a credentials
// match outside ignoreID.
func (s *service) checkUsernameAlreadyTaken(ctx context.Context, username, ignoreID string) *trpc.Error {
	taken, err := s.db.UsernameTaken(ctx, username, credentialsProvider, ignoreID)
	if err != nil {
		return admin.InternalError(err)
	}
	if taken {
		return trpc.NewError(trpc.CodeConflict, "Username already taken")
	}
	return nil
}

// throwIfCredentialsDisabled is throwIfCredentialsDisabled
// (packages/api/src/router/invite/checks.ts): the fork always ships the
// credentials provider, so the check is statically satisfied.
func throwIfCredentialsDisabled() *trpc.Error {
	return nil
}

// nullString renders "" as JSON null.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonToMap converts a marshaled struct into the generic map the tRPC layer
// re-serializes (keeps one output code path).
func jsonToMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(data, &out)
	return out
}
