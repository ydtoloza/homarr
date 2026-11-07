package user

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/auth"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Account handlers of the user router; upstream line references point to
// packages/api/src/router/user.ts.

// initUser is user.initUser (user.ts:70-129): the onboarding "user" step
// creates the first administrator, the credentials-admin group with the
// admin permission and the membership, and transitions the wizard.
func (s *service) initUser(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.requireOnboardingStep(ctx, admin.OnboardingStepUser); terr != nil {
		return nil, terr
	}
	if terr := throwIfCredentialsDisabled(); terr != nil {
		return nil, terr
	}
	username, password, email, terr := s.parseUserCreateInput(input, false)
	if terr != nil {
		return nil, terr
	}
	if terr := s.checkUsernameAlreadyTaken(ctx, username, ""); terr != nil {
		return nil, terr
	}

	maxPosition, err := s.db.MaxGroupPosition(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	// Fork: no ldap/oidc providers, so the next step is always "setup"
	// (user.ts:93).
	transitioned, err := s.db.CreateUserWithAdminGroup(ctx, db.User{
		ID:       admin.CreateID(),
		Name:     username,
		Email:    email,
		Password: hashedPassword,
	}, db.Group{
		ID:       admin.CreateID(),
		Name:     credentialsAdminGroup,
		Position: maxPosition + 1,
	}, admin.OnboardingStepSetup)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if transitioned != 1 {
		return nil, trpc.NewError(trpc.CodeConflict, "The initial administrator was already created.")
	}
	return nil, nil
}

// register is user.register (user.ts:130-193): publicProcedure; the invite
// (id + token + unexpired) is consumed inside the same transaction that
// inserts the user.
func (s *service) register(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := throwIfCredentialsDisabled(); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	username, password, _, terr := s.parseUserCreateInput(input, false)
	if terr != nil {
		return nil, terr
	}
	inviteID, terr := d.Str("inviteId")
	if terr != nil {
		return nil, terr
	}
	token, terr := d.Str("token")
	if terr != nil {
		return nil, terr
	}

	// The invite lookup (user.ts:135-148): id + token equality and
	// expirationDate >= now; any miss is FORBIDDEN "Invalid invite".
	invite, err := s.db.GetInviteByIDAndToken(ctx, inviteID, token)
	if err != nil {
		return nil, trpc.NewError(trpc.CodeForbidden, "Invalid invite")
	}
	if invite.ExpirationDate.Before(time.Now()) {
		return nil, trpc.NewError(trpc.CodeForbidden, "Invalid invite")
	}
	if terr := s.checkUsernameAlreadyTaken(ctx, username, ""); terr != nil {
		return nil, terr
	}

	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	err = s.db.CreateUserWithInvite(ctx, db.User{
		ID:       admin.CreateID(),
		Name:     username,
		Password: hashedPassword,
	}, inviteID, token)
	if err == db.ErrInviteInvalid {
		return nil, trpc.NewError(trpc.CodeForbidden, "Invalid invite")
	}
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// create is user.create (user.ts:194-224): admin; creates the credentials
// account and assigns the supplied groups.
func (s *service) create(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	if terr := throwIfCredentialsDisabled(); terr != nil {
		return nil, terr
	}
	username, password, email, terr := s.parseUserCreateInput(input, true)
	if terr != nil {
		return nil, terr
	}
	if terr := s.checkUsernameAlreadyTaken(ctx, username, ""); terr != nil {
		return nil, terr
	}

	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	userID := admin.CreateID()
	if err := s.db.CreateUser(ctx, db.User{
		ID:       userID,
		Name:     username,
		Email:    email,
		Password: hashedPassword,
	}); err != nil {
		return nil, admin.InternalError(err)
	}
	d, _ := admin.NewInputDecoder(input)
	groupIDs, _, terr := d.OptStrList("groupIds")
	if terr != nil {
		return nil, terr
	}
	for _, groupID := range groupIDs {
		if err := s.db.AddGroupMember(ctx, groupID, userID); err != nil {
			return nil, admin.InternalError(err)
		}
	}
	return nil, nil
}

// parseUserCreateInput is userBaseCreateSchema (+ groupIds for create):
// username (trim/lower 3-255), password (8-255) and confirmPassword
// equality, optional email.
func (s *service) parseUserCreateInput(input json.RawMessage, withGroupIDs bool) (string, string, string, *trpc.Error) {
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return "", "", "", terr
	}
	rawUsername, terr := d.Str("username")
	if terr != nil {
		return "", "", "", terr
	}
	username, terr := validateUsername(rawUsername)
	if terr != nil {
		return "", "", "", terr
	}
	rawPassword, terr := d.Str("password")
	if terr != nil {
		return "", "", "", terr
	}
	password, terr := validatePassword(rawPassword, "password")
	if terr != nil {
		return "", "", "", terr
	}
	confirmPassword, terr := d.Str("confirmPassword")
	if terr != nil {
		return "", "", "", terr
	}
	if confirmPassword != password {
		return "", "", "", admin.BadRequestf("password and confirmPassword do not match")
	}
	email := ""
	if rawEmail, ok, terr := d.OptStr("email"); terr != nil {
		return "", "", "", terr
	} else if ok {
		email, terr = validateOptionalEmail(rawEmail, "email")
		if terr != nil {
			return "", "", "", terr
		}
	}
	_ = withGroupIDs
	return username, password, email, nil
}

// setProfileImage is user.setProfileImage (user.ts:225-279): protected;
// self-or-admin FORBIDDEN, NOT_FOUND for unknown users, image is a
// data-URL string (max 350000 chars) or null to clear.
func (s *service) setProfileImage(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := requireSelfOrAdmin(c, userID, "You are not allowed to change other users profile images"); terr != nil {
		return nil, terr
	}
	var image *string
	if raw, ok := d.Raw("image"); !ok || raw == nil {
		image = nil
	} else if str, isString := raw.(string); isString {
		if len(str) > 350000 {
			return nil, admin.BadRequestf(`input key "image" must be at most 350000 characters`)
		}
		if !profileImagePattern.MatchString(str) {
			return nil, admin.BadRequestf(`input key "image" has an invalid value`)
		}
		image = &str
	} else {
		return nil, admin.BadRequestf(`expected a string or null for input key "image"`)
	}
	if _, err := s.db.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	if err := s.db.UpdateUserImage(ctx, userID, image); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// getAll is user.getAll (user.ts:280-316): admin; every user with the
// {id, name, email, emailVerified, image} projection.
func (s *service) getAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	users, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id": u.ID,
			// name/email/image keep null; emailVerified is a Date upstream
			// and an ISO string here (trpc/superjson.go).
			"name":          nullString(u.Name),
			"email":         nullString(u.Email),
			"emailVerified": admin.ISOStringOrNull(u.EmailVerified),
			"image":         nullString(u.Image),
		})
	}
	return out, nil
}

// selectable is user.selectable (user.ts:318-351): protected; the board /
// integration access forms' user picker, optionally filtered by provider.
func (s *service) selectable(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	var providers []string
	if len(input) > 0 && string(input) != "null" {
		d, terr := admin.NewInputDecoder(input)
		if terr != nil {
			return nil, terr
		}
		list, ok, terr := d.OptStrList("providers")
		if terr != nil {
			return nil, terr
		}
		if ok {
			for _, provider := range list {
				switch provider {
				case "credentials", "oidc", "ldap":
					providers = append(providers, provider)
				default:
					return nil, admin.BadRequestf(`input key "providers" has an invalid value`)
				}
			}
		}
	}
	users, err := s.db.ListSelectableUsers(ctx, providers)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":    u.ID,
			"name":  nullString(u.Name),
			"image": nullString(u.Image),
			"email": nullString(u.Email),
		})
	}
	return out, nil
}

// search is user.search (user.ts:352-398): admin; LIKE '%query%' with a
// 1-100 limit (default 10); a null name renders as "".
func (s *service) search(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := d.Str("query")
	if terr != nil {
		return nil, terr
	}
	limit, _, terr := d.OptInt64("limit", 1, 100)
	if terr != nil {
		return nil, terr
	}
	if limit == 0 {
		limit = 10 // zod default
	}
	users, err := s.db.SearchUsers(ctx, "%"+query+"%", limit)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(users))
	for _, u := range users {
		name := u.Name
		if name == "" {
			name = "" // user.ts:394: name ?? ""
		}
		out = append(out, map[string]any{
			"id":    u.ID,
			"name":  name,
			"image": nullString(u.Image),
			"email": nullString(u.Email),
		})
	}
	return out, nil
}

// getById is user.getById (user.ts:419-497): protected; self-or-admin
// FORBIDDEN, NOT_FOUND for unknown users; the full profile shape.
func (s *service) getById(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := requireSelfOrAdmin(c, userID, "You are not allowed to view other users details"); terr != nil {
		return nil, terr
	}
	detail, err := s.db.GetUserDetailByID(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	return map[string]any{
		"id":                        detail.ID,
		"name":                      nullString(detail.Name),
		"email":                     nullString(detail.Email),
		"emailVerified":             admin.ISOStringOrNull(detail.EmailVerified),
		"image":                     nullString(detail.Image),
		"provider":                  detail.Provider,
		"homeBoardId":               nullString(detail.HomeBoardID),
		"mobileHomeBoardId":         nullString(detail.MobileHomeBoardID),
		"colorScheme":               detail.ColorScheme,
		"byteUnitSystem":            detail.ByteUnitSystem,
		"firstDayOfWeek":            detail.FirstDayOfWeek,
		"pingIconsEnabled":          detail.PingIconsEnabled,
		"enableRightClickOnWidgets": detail.EnableRightClickOnWidgets,
		// headerPreferences stays the stored serialized string, exactly like
		// the upstream text column (selectUserSchema.pick).
		"headerPreferences":     detail.HeaderPreferences,
		"defaultSearchEngineId": nullString(detail.DefaultSearchEngineID),
		"openSearchInNewTab":    detail.OpenSearchInNewTab,
		"ddgBangs":              detail.DdgBangs,
		"completedManageTour":   detail.CompletedManageTour,
		"completedBoardTour":    detail.CompletedBoardTour,
	}, nil
}

// editProfile is user.editProfile (user.ts:498-558): protected;
// self-or-admin, credentials-only, username-conflict CONFLICT; an email
// change resets emailVerified.
func (s *service) editProfile(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := requireSelfOrAdmin(c, id, "You are not allowed to edit other users details"); terr != nil {
		return nil, terr
	}
	rawName, terr := d.Str("name")
	if terr != nil {
		return nil, terr
	}
	name, terr := validateUsername(rawName)
	if terr != nil {
		return nil, terr
	}
	email := ""
	if rawEmail, ok, terr := d.OptStr("email"); terr != nil {
		return nil, terr
	} else if ok {
		email, terr = validateNullableEmail(rawEmail, "email")
		if terr != nil {
			return nil, terr
		}
	}

	user, err := s.db.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	if user.Provider != credentialsProvider {
		return nil, trpc.NewError(trpc.CodeForbidden,
			"Username and email can not be changed for users with external providers")
	}
	// Demo mode branch (user.ts:540-545) is compiled out: the fork runs
	// with DEMO_MODE off.
	if terr := s.checkUsernameAlreadyTaken(ctx, name, id); terr != nil {
		return nil, terr
	}

	// emailDirty is `input.email && user.email !== input.email`
	// (user.ts:549): a null email never counts as dirty.
	emailDirty := false
	if email != "" && user.Email != email {
		emailDirty = true
	}
	if err := s.db.UpdateUserProfile(ctx, id, name, email, emailDirty); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// delete is user.delete (user.ts:559-591): protected; self-or-admin
// FORBIDDEN ("You are not allowed to delete other users"); demo mode off.
func (s *service) delete(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := requireSelfOrAdmin(c, userID, "You are not allowed to delete other users"); terr != nil {
		return nil, terr
	}
	if err := s.db.DeleteUser(ctx, userID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// changePassword is user.changePassword (user.ts:592-673): protected;
// another user without admin hides behind NOT_FOUND; changing your own
// password requires the previous one (bcrypt verify).
func (s *service) changePassword(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	previousPassword, terr := d.Str("previousPassword")
	if terr != nil {
		return nil, terr
	}
	rawPassword, terr := d.Str("password")
	if terr != nil {
		return nil, terr
	}
	password, terr := validatePassword(rawPassword, "password")
	if terr != nil {
		return nil, terr
	}
	confirmPassword, terr := d.Str("confirmPassword")
	if terr != nil {
		return nil, terr
	}
	if confirmPassword != password {
		return nil, admin.BadRequestf("password and confirmPassword do not match")
	}

	// Non-admins changing someone else's password get NOT_FOUND
	// (user.ts:616-621).
	if !c.IsAdmin() && c.Session.UserID != userID {
		return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
	}
	dbUser, err := s.db.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	if dbUser.Provider != credentialsProvider {
		return nil, trpc.NewError(trpc.CodeForbidden,
			"Password can not be changed for users with external providers")
	}

	// Admins reset other users' passwords without the previous one
	// (user.ts:647-664).
	if c.Session.UserID == userID {
		if !auth.ComparePassword(previousPassword, dbUser.Password) {
			return nil, trpc.NewError(trpc.CodeForbidden, "Invalid password")
		}
	}
	hashedPassword, err := auth.HashPassword(password)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if err := s.db.UpdateUserPassword(ctx, userID, hashedPassword); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// completeTour is user.completeTour (user.ts:867-876): marks one tour flag
// for the caller.
func (s *service) completeTour(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	tour, terr := d.Enum("tour", []string{"manage", "board"})
	if terr != nil {
		return nil, terr
	}
	if err := s.db.MarkUserTourCompleted(ctx, c.Session.UserID, tour); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// resetTours is user.resetTours (user.ts:877-885).
func (s *service) resetTours(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	if err := s.db.ResetUserTours(ctx, c.Session.UserID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// getTourStatus is user.getTourStatus (user.ts:886-906): demo mode reports
// both tours complete; a missing user row falls back to false/false.
func (s *service) getTourStatus(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	if s.opts.DemoMode {
		return map[string]any{"completedManageTour": true, "completedBoardTour": true}, nil
	}
	manage, board, err := s.db.GetUserTourStatus(ctx, c.Session.UserID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return map[string]any{"completedManageTour": manage, "completedBoardTour": board}, nil
}
