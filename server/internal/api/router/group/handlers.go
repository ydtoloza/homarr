package group

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Handlers of the group router; upstream line references point to
// packages/api/src/router/group.ts.

// getAll is group.getAll: admin, every group with its members.
func (s *service) getAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	groups, err := s.db.ListGroups(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return s.listGroups(ctx, groups)
}

// getPaginated is group.getPaginated: admin, {search?, pageSize ≤ 100
// default 10, page default 1} → {items, totalCount}. The search string is
// trimmed and matched with LIKE '%q%' on the name.
func (s *service) getPaginated(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	search, _, terr := d.OptStr("search")
	if terr != nil {
		return nil, terr
	}
	pageSize, _, terr := d.OptInt64("pageSize", 1, 100)
	if terr != nil {
		return nil, terr
	}
	if pageSize == 0 {
		pageSize = 10 // zod default
	}
	page, _, terr := d.OptInt64("page", 1, 1<<62)
	if terr != nil {
		return nil, terr
	}
	if page == 0 {
		page = 1 // zod default
	}

	namePattern := ""
	if search != "" {
		namePattern = "%" + strings.TrimSpace(search) + "%"
	}
	totalCount, err := s.db.CountGroups(ctx, namePattern)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	groups, err := s.db.ListGroupsPaginated(ctx, namePattern, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	items, terr := s.listGroups(ctx, groups)
	if terr != nil {
		return nil, terr
	}
	return map[string]any{"items": items, "totalCount": totalCount}, nil
}

// getByID is group.getById: admin; NOT_FOUND "Group not found" for unknown
// ids; members include the provider column, permissions the raw keys and
// owner the {id, name, image, email} projection.
func (s *service) getByID(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	group, err := s.db.GetGroupByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "Group not found")
		}
		return nil, admin.InternalError(err)
	}
	members, err := s.db.ListGroupMemberUsers(ctx, group.ID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	memberList := make([]any, 0, len(members))
	for _, m := range members {
		memberList = append(memberList, memberShape(m, true))
	}
	permissions, err := s.db.ListGroupPermissionStrings(ctx, group.ID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	permList := make([]any, 0, len(permissions))
	for _, p := range permissions {
		permList = append(permList, p)
	}
	var owner any
	if group.OwnerID != "" {
		ownerUser, err := s.db.GetUserByID(ctx, group.OwnerID)
		if err == nil {
			owner = map[string]any{
				"id":    ownerUser.ID,
				"name":  nullString(ownerUser.Name),
				"image": nullString(ownerUser.Image),
				"email": nullString(ownerUser.Email),
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, admin.InternalError(err)
		}
	}
	out := groupShape(group)
	out["members"] = memberList
	out["permissions"] = permList
	out["owner"] = owner
	return out, nil
}

// selectable is group.selectable: protectedProcedure, used by the board /
// integration access forms. withPermissions only widens the output for
// administrators.
func (s *service) selectable(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	withPermissions := false
	if len(input) > 0 && string(input) != "null" {
		d, terr := admin.NewInputDecoder(input)
		if terr != nil {
			return nil, terr
		}
		requested, _, terr := d.OptBool("withPermissions")
		if terr != nil {
			return nil, terr
		}
		// group.ts:136: input?.withPermissions && admin permission.
		withPermissions = requested && c.IsAdmin()
	}

	groups, err := s.db.ListSelectableGroups(ctx, "", 0)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if !withPermissions {
		out := make([]any, 0, len(groups))
		for _, g := range groups {
			out = append(out, map[string]any{"id": g.ID, "name": g.Name})
		}
		return out, nil
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	rows, err := s.db.ListGroupPermissionsForGroups(ctx, ids)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	permsByGroup := map[string][]any{}
	for _, row := range rows {
		permsByGroup[row.GroupID] = append(permsByGroup[row.GroupID], row.Permission)
	}
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		permList := permsByGroup[g.ID]
		if permList == nil {
			permList = []any{}
		}
		out = append(out, map[string]any{"id": g.ID, "name": g.Name, "permissions": permList})
	}
	return out, nil
}

// search is group.search: admin, {query, limit 1-100 default 10}, LIKE
// '%query%' on the name, {id, name} rows.
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
	groups, err := s.db.ListSelectableGroups(ctx, "%"+query+"%", limit)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]any{"id": g.ID, "name": g.Name})
	}
	return out, nil
}

// createInitialExternalGroup is group.createInitialExternalGroup: the
// onboarding "group" step (onboardingProcedure.requiresStep("group")). It
// transitions the wizard to "setup" and creates the first administrator
// group with the admin permission; the transition row count decides CONFLICT.
func (s *service) createInitialExternalGroup(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.requireOnboardingStep(ctx, admin.OnboardingStepGroup); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	name, terr := validateGroupName(d)
	if terr != nil {
		return nil, terr
	}
	if terr := s.checkSimilarNameAndThrow(ctx, name, ""); terr != nil {
		return nil, terr
	}
	maxPosition, err := s.db.MaxGroupPosition(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	transitioned, err := s.db.CreateGroupDuringOnboarding(ctx, db.Group{
		ID:       admin.CreateID(),
		Name:     name,
		Position: maxPosition + 1,
	})
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if transitioned != 1 {
		return nil, trpc.NewError(trpc.CodeConflict, "The initial external group was already created.")
	}
	return nil, nil
}

// createGroup is group.createGroup: admin; inserts the group with position
// max+1 and the caller as owner; returns the raw id string.
func (s *service) createGroup(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	name, terr := validateGroupName(d)
	if terr != nil {
		return nil, terr
	}
	if terr := s.checkSimilarNameAndThrow(ctx, name, ""); terr != nil {
		return nil, terr
	}
	maxPosition, err := s.db.MaxGroupPosition(ctx)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	id := admin.CreateID()
	if err := s.db.CreateGroup(ctx, db.Group{
		ID:       id,
		Name:     name,
		OwnerID:  c.Session.UserID,
		Position: maxPosition + 1,
	}); err != nil {
		return nil, admin.InternalError(err)
	}
	return id, nil
}

// updateGroup is group.updateGroup: admin; guards + rename.
func (s *service) updateGroup(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	name, terr := validateGroupName(d)
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, id); terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNameIsReserved(ctx, id); terr != nil {
		return nil, terr
	}
	if terr := s.checkSimilarNameAndThrow(ctx, name, id); terr != nil {
		return nil, terr
	}
	if err := s.db.UpdateGroupName(ctx, id, name); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// savePartialSettings is group.savePartialSettings: admin; writes only the
// supplied homeBoardId / mobileHomeBoardId keys (drizzle skips undefined
// keys in .set()).
func (s *service) savePartialSettings(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, id); terr != nil {
		return nil, terr
	}
	settings, terr := d.Object("settings")
	if terr != nil {
		return nil, terr
	}
	update := db.GroupSettingsUpdate{}
	if home, ok, terr := settings.StringPtr("homeBoardId"); terr != nil {
		return nil, terr
	} else if ok {
		update.SetHomeBoard = true
		update.HomeBoardID = deref(home)
	}
	if mobile, ok, terr := settings.StringPtr("mobileHomeBoardId"); terr != nil {
		return nil, terr
	} else if ok {
		update.SetMobileHomeBoard = true
		update.MobileHomeBoardID = deref(mobile)
	}
	if err := s.db.UpdateGroupSettings(ctx, id, update); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// savePositions is group.savePositions: admin; positions[i] gets
// position = i + 1 inside a transaction.
func (s *service) savePositions(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	positions, terr := d.StrList("positions")
	if terr != nil {
		return nil, terr
	}
	rows := make([]db.GroupPosition, 0, len(positions))
	for index, id := range positions {
		rows = append(rows, db.GroupPosition{ID: id, Position: int64(index + 1)})
	}
	if err := s.db.UpdateGroupPositions(ctx, rows); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// groupPermissionKeys is zodEnumFromArray(groupPermissionKeys)
// (packages/definitions/src/permissions.ts:111-119): the flat list of
// assignable permission keys.
var groupPermissionKeys = []string{
	"board-create", "board-view-all", "board-modify-all", "board-full-all",
	"app-create", "app-use-all", "app-modify-all", "app-full-all",
	"integration-create", "integration-use-all", "integration-interact-all", "integration-full-all",
	"search-engine-create", "search-engine-modify-all", "search-engine-full-all",
	"media-upload", "media-view-all", "media-full-all",
	"other-view-logs",
	"admin",
}

// savePermissions is group.savePermissions: admin; replaces the group's
// permission rows with the deduplicated input (stored exactly as selected —
// implied permissions resolve at read time, group.ts:301-303).
func (s *service) savePermissions(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	groupID, terr := d.Str("groupId")
	if terr != nil {
		return nil, terr
	}
	permissions, terr := d.StrList("permissions")
	if terr != nil {
		return nil, terr
	}
	for _, permission := range permissions {
		if !admin.ContainsString(groupPermissionKeys, permission) {
			return nil, admin.BadRequestf(`input key "permissions" has an invalid value`)
		}
	}
	if terr := s.throwIfGroupNotFound(ctx, groupID); terr != nil {
		return nil, terr
	}
	if err := s.db.ReplaceGroupPermissions(ctx, groupID, admin.Unique(permissions)); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// transferOwnership is group.transferOwnership: admin; guards + owner
// update.
func (s *service) transferOwnership(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	groupID, terr := d.Str("groupId")
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, groupID); terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNameIsReserved(ctx, groupID); terr != nil {
		return nil, terr
	}
	if err := s.db.UpdateGroupOwner(ctx, groupID, userID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// deleteGroup is group.deleteGroup: admin; guards + delete.
func (s *service) deleteGroup(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, id); terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNameIsReserved(ctx, id); terr != nil {
		return nil, terr
	}
	if err := s.db.DeleteGroup(ctx, id); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// addMember is group.addMember: admin; guards + user checks + insert. A
// duplicate membership surfaces the SQLite UNIQUE constraint error like the
// upstream raw insert (→ INTERNAL_SERVER_ERROR).
func (s *service) addMember(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	groupID, terr := d.Str("groupId")
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, groupID); terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNameIsReserved(ctx, groupID); terr != nil {
		return nil, terr
	}
	if terr := throwIfGroupMembersCannotBeManagedLocally(); terr != nil {
		return nil, terr
	}
	user, err := s.db.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	if !isGroupMembershipManagedLocally(user.Provider) {
		return nil, trpc.NewError(trpc.CodeForbidden, "User's provider is not managed locally")
	}
	if err := s.db.AddGroupMember(ctx, groupID, userID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// removeMember is group.removeMember: admin; guards + delete (removing a
// non-member is a no-op).
func (s *service) removeMember(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	groupID, terr := d.Str("groupId")
	if terr != nil {
		return nil, terr
	}
	userID, terr := d.Str("userId")
	if terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNotFound(ctx, groupID); terr != nil {
		return nil, terr
	}
	if terr := s.throwIfGroupNameIsReserved(ctx, groupID); terr != nil {
		return nil, terr
	}
	if terr := throwIfGroupMembersCannotBeManagedLocally(); terr != nil {
		return nil, terr
	}
	if err := s.db.RemoveGroupMember(ctx, groupID, userID); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// requireOnboardingStep is onboardingProcedure.requiresStep
// (packages/api/src/trpc.ts:209-223) composed with the
// onboardingClaimedProcedure gate (trpc.ts:148-159): administrators pass,
// anonymous callers only with a valid claim while no user exists; the
// current step must match.
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

// deref returns the pointed string, or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
