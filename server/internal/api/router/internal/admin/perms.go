package admin

import (
	"context"
	"sort"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Permission model of the administration routers.
//
// Upstream carries the caller's global permissions inside the session
// (session.user.permissions = getCurrentUserPermissionsAsync, expanded with
// getPermissionsWithChildren, packages/auth/callbacks.ts:10-21 and
// packages/definitions/src/permissions.ts). The homarr-go session
// (ctxauth.Session) intentionally stays minimal, so the admin routers
// resolve group permissions from the database with the same expansion and
// treat ctxauth.Session.IsAdministrator as the upstream "admin" permission.
// The logic mirrors package board (perms.go); it is duplicated here so the
// admin packages stay independent of the board package.

// groupPermissionParents mirrors groupPermissionParents
// (packages/definitions/src/permissions.ts:57-71): each key implies its
// children.
var groupPermissionParents = map[string][]string{
	"board-modify-all":         {"board-view-all"},
	"board-full-all":           {"board-modify-all", "board-create"},
	"app-modify-all":           {"app-create"},
	"app-full-all":             {"app-modify-all", "app-use-all"},
	"integration-interact-all": {"integration-use-all"},
	"integration-full-all":     {"integration-interact-all", "integration-create"},
	"search-engine-modify-all": {"search-engine-create"},
	"search-engine-full-all":   {"search-engine-modify-all"},
	"media-full-all":           {"media-upload", "media-view-all"},
	"admin": {
		"board-full-all", "app-full-all", "integration-full-all",
		"search-engine-full-all", "media-full-all", "other-view-logs",
	},
}

// ExpandPermissions is getPermissionsWithChildren
// (packages/definitions/src/permissions.ts:94-105): transitively add every
// implied key.
func ExpandPermissions(keys []string) map[string]bool {
	set := map[string]bool{}
	var add func(keys []string)
	add = func(keys []string) {
		for _, key := range keys {
			if set[key] {
				continue
			}
			set[key] = true
			add(groupPermissionParents[key])
		}
	}
	add(keys)
	return set
}

// caller is the resolved request identity for one procedure invocation.
type Caller struct {
	Session *ctxauth.Session
	// groupPermissions are the expanded permission keys of the caller's
	// groups (empty for anonymous callers).
	groupPermissions map[string]bool
}

// ResolveCaller loads the caller identity. Anonymous callers keep a nil
// session and no permissions; procedures decide whether that is an error.
func ResolveCaller(ctx context.Context, database *db.DB) (Caller, *trpc.Error) {
	session := ctxauth.FromContext(ctx)
	if session == nil {
		return Caller{groupPermissions: map[string]bool{}}, nil
	}
	keys, err := database.ListGroupPermissionsForUser(ctx, session.UserID)
	if err != nil {
		return Caller{}, InternalError(err)
	}
	return Caller{Session: session, groupPermissions: ExpandPermissions(keys)}, nil
}

// RequireSignedIn is the protectedProcedure gate
// (enforceUserIsAuthed, packages/api/src/trpc.ts:163-172): the upstream error
// carries no message, and tRPC v11 falls back to the code string
// (TRPCError constructor: message ?? cause?.message ?? opts.code).
func RequireSignedIn(c Caller) *trpc.Error {
	if c.Session == nil {
		return trpc.NewError(trpc.CodeUnauthorized, "UNAUTHORIZED")
	}
	return nil
}

// PermissionKeys returns the expanded permission keys of the caller — the
// upstream ctx.session.user.permissions array (session.user.permissions =
// getCurrentUserPermissionsAsync expanded with getPermissionsWithChildren).
// Administrators do NOT implicitly carry every key here: the array mirrors
// the expansion of the group permissions, and the fork's administrator flag
// is honored through HasPermission/IsAdmin.
func (c Caller) PermissionKeys() []string {
	keys := make([]string, 0, len(c.groupPermissions))
	for key := range c.groupPermissions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// HasPermission reports the permissionRequiredProcedure check
// (ctx.session.user.permissions.includes(permission)). Administrators
// inherit every permission because "admin" expands to the full parent chain.
func (c Caller) HasPermission(permission string) bool {
	if c.groupPermissions[permission] {
		return true
	}
	// Fork extension: ctxauth.Session.IsAdministrator marks the instance
	// administrator (upstream would resolve this through a group carrying the
	// "admin" permission key).
	return c.Session != nil && c.Session.IsAdministrator
}

// IsAdmin mirrors session.user.permissions.includes("admin").
func (c Caller) IsAdmin() bool {
	return c.HasPermission("admin")
}

// RequirePermission is permissionRequiredProcedure.requiresPermission
// (packages/api/src/trpc.ts:195-207): signed in first, then FORBIDDEN
// "Permission denied". permissionRequiredProcedure extends
// protectedProcedure, so an anonymous caller gets UNAUTHORIZED.
func RequirePermission(c Caller, permission string) *trpc.Error {
	if err := RequireSignedIn(c); err != nil {
		return err
	}
	if !c.HasPermission(permission) {
		return trpc.NewError(trpc.CodeForbidden, "Permission denied")
	}
	return nil
}

// CanViewBoard is the "view" slice of throwIfActionForbiddenAsync
// (packages/api/src/router/board/board-access.ts:45-64) for procedures
// outside the board package that must check board visibility
// (user.changeHomeBoards / user.updatePreferences reference boards). A
// missing board returns db.ErrNotFound so callers can hide its existence
// with NOT_FOUND like upstream.
func CanViewBoard(ctx context.Context, database *db.DB, boardID string, c Caller) (bool, error) {
	board, err := database.GetBoardByID(ctx, boardID)
	if err != nil {
		return false, err
	}
	if c.Session != nil && board.CreatorID == c.Session.UserID {
		return true, nil
	}
	if board.IsPublic {
		return true, nil
	}
	if c.Session != nil {
		// Global board-view-all (already expanded) grants view access,
		// mirroring constructBoardPermissions
		// (packages/auth/permissions/board-permissions.ts:24-47).
		if c.HasPermission("board-view-all") {
			return true, nil
		}
		direct, err := database.ListBoardUserPermissionsForUser(ctx, c.Session.UserID)
		if err != nil {
			return false, err
		}
		for _, p := range direct {
			if p.BoardID == boardID {
				return true, nil
			}
		}
		groups, err := database.ListGroupIDsForUser(ctx, c.Session.UserID)
		if err != nil {
			return false, err
		}
		groupPerms, err := database.ListBoardGroupPermissionsForGroups(ctx, groups)
		if err != nil {
			return false, err
		}
		for _, p := range groupPerms {
			if p.BoardID == boardID {
				return true, nil
			}
		}
	}
	return false, nil
}

// AccessibleBoardIDs is getAccessibleBoardIdsForUserAsync
// (packages/api/src/router/board.ts:2913-2921): the ids a caller can reach
// through public visibility, ownership, direct or group board permissions —
// or every board when the caller holds board-view-all. Used by the user
// preferences header-shortcut validation.
func AccessibleBoardIDs(ctx context.Context, database *db.DB, c Caller) (map[string]bool, error) {
	ids := map[string]bool{}
	if c.Session == nil {
		return ids, nil
	}
	if c.HasPermission("board-view-all") {
		boards, err := database.ListAllBoardsMinimal(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range boards {
			ids[b.ID] = true
		}
		return ids, nil
	}
	boards, err := database.ListBoardsForUser(ctx, c.Session.UserID)
	if err != nil {
		return nil, err
	}
	for _, b := range boards {
		ids[b.ID] = true
	}
	return ids, nil
}
