// Package group is the Go port of the group tRPC router
// (packages/api/src/router/group.ts): group CRUD, membership and global
// permission management. All write paths are admin-only except
// group.selectable (protectedProcedure: it also feeds the board/integration
// access forms).
//
// The local-membership checks mirror @homarr/auth/server for the fork:
// homarr-go ships the credentials provider only, so
// canManageGroupMembersLocally() is always true and
// isGroupMembershipManagedLocally(provider) is true exactly for
// "credentials" users (packages/auth/providers/check-provider.ts:18-29).
package group

import (
	"context"
	"errors"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// everyoneGroup is the reserved group name (packages/definitions/src/group.ts:1).
const everyoneGroup = "everyone"

// Options configures the group router.
type Options struct {
	// OnboardingClaimTokenFromContext resolves the onboarding claim token
	// (cookie "homarr-onboarding-claim") for
	// group.createInitialExternalGroup, which runs behind
	// onboardingProcedure. The httpserver middleware injects the cookie
	// value; nil or empty means "no claim".
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

// Register registers the group router under its upstream paths. Call once
// at startup (Register panics on duplicate paths).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	s := &service{db: database, opts: &opts}

	// group.getAll (group.ts:24-46).
	r.Register("group.getAll", s.getAll)
	// group.getPaginated (group.ts:48-82).
	r.Register("group.getPaginated", s.getPaginated)
	// group.getById (group.ts:83-131).
	r.Register("group.getById", s.getByID)
	// group.selectable (group.ts:133-159).
	r.Register("group.selectable", s.selectable)
	// group.search (group.ts:160-177).
	r.Register("group.search", s.search)
	// group.createInitialExternalGroup (group.ts:178-223).
	r.Register("group.createInitialExternalGroup", s.createInitialExternalGroup)
	// group.createGroup (group.ts:224-241).
	r.Register("group.createGroup", s.createGroup)
	// group.updateGroup (group.ts:242-257).
	r.Register("group.updateGroup", s.updateGroup)
	// group.savePartialSettings (group.ts:258-271).
	r.Register("group.savePartialSettings", s.savePartialSettings)
	// group.savePositions (group.ts:272-294).
	r.Register("group.savePositions", s.savePositions)
	// group.savePermissions (group.ts:295-338).
	r.Register("group.savePermissions", s.savePermissions)
	// group.transferOwnership (group.ts:339-352).
	r.Register("group.transferOwnership", s.transferOwnership)
	// group.deleteGroup (group.ts:353-361).
	r.Register("group.deleteGroup", s.deleteGroup)
	// group.addMember (group.ts:362-392).
	r.Register("group.addMember", s.addMember)
	// group.removeMember (group.ts:393-404).
	r.Register("group.removeMember", s.removeMember)
}

// --- output shapes -----------------------------------------------------------

// groupShape is the drizzle selectGroupSchema projection shared by
// getAll/getPaginated: {id, name, ownerId, homeBoardId, mobileHomeBoardId,
// position} with nulls preserved.
func groupShape(g db.Group) map[string]any {
	return map[string]any{
		"id":                g.ID,
		"name":              g.Name,
		"ownerId":           nullString(g.OwnerID),
		"homeBoardId":       nullString(g.HomeBoardID),
		"mobileHomeBoardId": nullString(g.MobileHomeBoardID),
		"position":          g.Position,
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// memberShape is the {id, name, email, image} member projection of
// getAll/getPaginated (group.ts:26-38).
func memberShape(u db.GroupMemberUser, withProvider bool) map[string]any {
	out := map[string]any{
		"id":    u.ID,
		"name":  nullString(u.Name),
		"email": nullString(u.Email),
		"image": nullString(u.Image),
	}
	if withProvider {
		out["provider"] = u.Provider
	}
	return out
}

// listGroups renders the getAll/getPaginated rows with their members
// stitched in (members arrive through the groupMember → user relation).
func (s *service) listGroups(ctx context.Context, groups []db.Group) ([]any, *trpc.Error) {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	members, err := s.db.ListGroupMembersForGroups(ctx, ids)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	membersByGroup := map[string][]any{}
	for _, m := range members {
		membersByGroup[m.GroupID] = append(membersByGroup[m.GroupID], memberShape(m.User, false))
	}
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		memberList := membersByGroup[g.ID]
		if memberList == nil {
			memberList = []any{}
		}
		row := groupShape(g)
		row["members"] = memberList
		out = append(out, row)
	}
	return out, nil
}

// --- guards ------------------------------------------------------------------

// throwIfGroupNotFoundAsync (group.ts:440-451): NOT_FOUND "Group not found".
func (s *service) throwIfGroupNotFound(ctx context.Context, id string) *trpc.Error {
	if _, err := s.db.GetGroupByID(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return trpc.NewError(trpc.CodeNotFound, "Group not found")
		}
		return admin.InternalError(err)
	}
	return nil
}

// throwIfGroupNameIsReservedAsync (group.ts:420-429): the "everyone" group
// cannot be renamed, transferred, deleted or receive members.
func (s *service) throwIfGroupNameIsReserved(ctx context.Context, id string) *trpc.Error {
	reserved, err := s.db.GroupIsEveryone(ctx, id)
	if err != nil {
		return admin.InternalError(err)
	}
	if reserved {
		return trpc.NewError(trpc.CodeForbidden, "Action is forbidden for reserved group names")
	}
	return nil
}

// checkSimilarNameAndThrowAsync (group.ts:407-418): CONFLICT
// "Found group with similar name" when a LIKE match exists.
func (s *service) checkSimilarNameAndThrow(ctx context.Context, name, ignoreID string) *trpc.Error {
	similar, err := s.db.GroupNameSimilarExists(ctx, name, ignoreID)
	if err != nil {
		return admin.InternalError(err)
	}
	if similar {
		return trpc.NewError(trpc.CodeConflict, "Found group with similar name")
	}
	return nil
}

// throwIfGroupMembersCannotBeManagedLocally (group.ts:431-438): always
// passes in the fork (credentials is the only provider and it manages its
// members locally). Kept as a documented seam for future providers.
func throwIfGroupMembersCannotBeManagedLocally() *trpc.Error {
	return nil
}

// isGroupMembershipManagedLocally is isGroupMembershipManagedLocally
// (packages/auth/providers/check-provider.ts:18-29) for the fork: only
// credentials users are locally managed.
func isGroupMembershipManagedLocally(provider string) bool {
	return provider == "credentials"
}

// validateGroupName is groupCreateSchema (packages/validation/src/group.ts:
// 8-17): trim, 1-64 chars, not the reserved "everyone".
func validateGroupName(d *admin.InputDecoder) (string, *trpc.Error) {
	name, terr := d.Str("name")
	if terr != nil {
		return "", terr
	}
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 64 {
		return "", admin.BadRequestf(`input key "name" must be between 1 and 64 characters`)
	}
	if name == everyoneGroup {
		return "", admin.BadRequestf("'everyone' is a reserved group name")
	}
	return name, nil
}
