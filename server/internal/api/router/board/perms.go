package board

import (
	"context"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Permission model of the board domain.
//
// Upstream carries the caller's global permissions inside the session
// (session.user.permissions = getCurrentUserPermissionsAsync, expanded with
// getPermissionsWithChildren, packages/auth/callbacks.ts:10-21 and
// packages/definitions/src/permissions.ts). The homarr-go session
// (ctxauth.Session) intentionally stays minimal, so the board package
// resolves group permissions from the database with the same expansion and
// treats ctxauth.Session.IsAdministrator as the upstream "admin" permission.

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

// expandPermissions is getPermissionsWithChildren
// (packages/definitions/src/permissions.ts:94-105): transitively add every
// implied key.
func expandPermissions(keys []string) map[string]bool {
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
type caller struct {
	session *ctxauth.Session
	// groupPermissions are the expanded permission keys of the caller's
	// groups (empty for anonymous callers).
	groupPermissions map[string]bool
}

// resolveCaller loads the caller identity. Anonymous callers keep a nil
// session and no permissions; procedures decide whether that is an error.
func (s *service) resolveCaller(ctx context.Context) (caller, *trpc.Error) {
	session := ctxauth.FromContext(ctx)
	if session == nil {
		return caller{groupPermissions: map[string]bool{}}, nil
	}
	keys, err := s.db.ListGroupPermissionsForUser(ctx, session.UserID)
	if err != nil {
		return caller{}, internalError(err)
	}
	return caller{session: session, groupPermissions: expandPermissions(keys)}, nil
}

// requireSignedIn is the protectedProcedure gate
// (enforceUserIsAuthed, packages/api/src/trpc.ts:163-172): the upstream error
// carries no message, and tRPC v11 falls back to the code string
// (TRPCError constructor: message ?? cause?.message ?? opts.code).
func requireSignedIn(c caller) *trpc.Error {
	if c.session == nil {
		return trpc.NewError(trpc.CodeUnauthorized, "UNAUTHORIZED")
	}
	return nil
}

// hasPermission reports the permissionRequiredProcedure check
// (ctx.session.user.permissions.includes(permission)). Administrators
// inherit every permission because "admin" expands to the full parent chain.
func (c caller) hasPermission(permission string) bool {
	if c.groupPermissions[permission] {
		return true
	}
	// Fork extension: ctxauth.Session.IsAdministrator marks the instance
	// administrator (upstream would resolve this through a group carrying the
	// "admin" permission key).
	return c.session != nil && c.session.IsAdministrator
}

// isAdmin mirrors session.user.permissions.includes("admin").
func (c caller) isAdmin() bool {
	return c.hasPermission("admin")
}

// requirePermission is permissionRequiredProcedure.requiresPermission
// (packages/api/src/trpc.ts:195-207): signed in first, then FORBIDDEN
// "Permission denied".
func requirePermission(c caller, permission string) *trpc.Error {
	if err := requireSignedIn(c); err != nil {
		return err
	}
	if !c.hasPermission(permission) {
		return trpc.NewError(trpc.CodeForbidden, "Permission denied")
	}
	return nil
}

// boardAccess is the loaded board state of throwIfActionForbiddenAsync
// (packages/api/src/router/board/board-access.ts:16-64): the board row plus
// the caller's direct user permissions and group permissions on it.
type boardAccess struct {
	board           db.BoardFull
	userPermissions []string // caller's direct permissions on this board
	groupPerms      []string // permissions of the caller's groups on this board
}

// hasFullAccess / hasChangeAccess / hasViewAccess mirror
// constructBoardPermissions (packages/auth/permissions/board-permissions.ts:24-47).
func (a boardAccess) hasFullAccess(c caller) bool {
	if c.session != nil && a.board.CreatorID == c.session.UserID {
		return true
	}
	for _, p := range a.userPermissions {
		if p == "full" {
			return true
		}
	}
	for _, p := range a.groupPerms {
		if p == "full" {
			return true
		}
	}
	return c.hasPermission("board-full-all")
}

func (a boardAccess) hasChangeAccess(c caller) bool {
	if c.session != nil && a.board.CreatorID == c.session.UserID {
		return true
	}
	for _, p := range a.userPermissions {
		if p == "modify" || p == "full" {
			return true
		}
	}
	for _, p := range a.groupPerms {
		if p == "modify" || p == "full" {
			return true
		}
	}
	return c.hasPermission("board-modify-all") || c.hasPermission("board-full-all")
}

func (a boardAccess) hasViewAccess(c caller) bool {
	if c.session != nil && a.board.CreatorID == c.session.UserID {
		return true
	}
	if len(a.userPermissions) >= 1 || len(a.groupPerms) >= 1 || a.board.IsPublic {
		return true
	}
	return c.hasPermission("board-view-all")
}

// boardPermissionLevel is the upstream BoardPermission union.
type boardPermissionLevel string

const (
	permView   boardPermissionLevel = "view"
	permModify boardPermissionLevel = "modify"
	permFull   boardPermissionLevel = "full"
)

// loadBoardAccess resolves the board and the caller's permission rows on it.
// missing boards return db.ErrNotFound.
func (s *service) loadBoardAccess(ctx context.Context, boardID string, c caller) (boardAccess, error) {
	board, err := s.db.GetBoardFullByID(ctx, boardID)
	if err != nil {
		return boardAccess{}, err
	}
	access := boardAccess{board: board}
	if c.session != nil {
		direct, err := s.db.ListBoardUserPermissionsForUser(ctx, c.session.UserID)
		if err != nil {
			return boardAccess{}, err
		}
		for _, p := range direct {
			if p.BoardID == boardID {
				access.userPermissions = append(access.userPermissions, p.Permission)
			}
		}
		groups, err := s.db.ListBoardGroupPermissionsForUser(ctx, boardID, c.session.UserID)
		if err != nil {
			return boardAccess{}, err
		}
		for _, p := range groups {
			access.groupPerms = append(access.groupPerms, p.Permission)
		}
	}
	return access, nil
}

// requireBoardAccess is throwIfActionForbiddenAsync: every rejection (unknown
// board, insufficient permission, anonymous caller) becomes NOT_FOUND
// "Board not found" to prevent snooping on board existence
// (board-access.ts:45-64,84-89).
func (s *service) requireBoardAccess(ctx context.Context, boardID string, c caller, level boardPermissionLevel) *trpc.Error {
	access, err := s.loadBoardAccess(ctx, boardID, c)
	if err != nil {
		return notFoundBoard()
	}
	// The escalation ladder of board-access.ts:49-63: full access allows
	// everything, change access covers modify and view, view access only
	// covers view.
	if access.hasFullAccess(c) {
		return nil
	}
	if (level == permModify || level == permView) && access.hasChangeAccess(c) {
		return nil
	}
	if level == permView && access.hasViewAccess(c) {
		return nil
	}
	return notFoundBoard()
}

// notFoundBoard is the notAllowed() helper of board-access.ts:84-89.
func notFoundBoard() *trpc.Error {
	return trpc.NewError(trpc.CodeNotFound, "Board not found")
}

// accessContext is getBoardAccessContextAsync
// (packages/api/src/router/board.ts:2868-2902): the ids a caller can reach
// through direct or group board permissions, its home board selection and the
// memberships with their group permissions.
type accessContext struct {
	// boardIDs are the boards shared with the caller (direct user permission
	// rows plus group permission rows of its groups).
	boardIDs []string
	// homeBoards is nil for anonymous callers and users that no longer exist.
	homeBoards db.UserHomeBoards
	// hasHomeBoards distinguishes "anonymous / unknown user" from "user with
	// no home board".
	hasHomeBoards bool
	// canViewAllBoards is getCanViewAllBoards: any group permission expands
	// to board-view-all.
	canViewAllBoards bool
}

// getBoardAccessContext resolves the visibility context of the caller.
func (s *service) getBoardAccessContext(ctx context.Context, c caller) (accessContext, *trpc.Error) {
	out := accessContext{}
	if c.session == nil {
		return out, nil
	}
	homeBoards, err := s.db.GetUserHomeBoards(ctx, c.session.UserID)
	if err == nil {
		out.homeBoards = homeBoards
		out.hasHomeBoards = true
	} else if err != db.ErrNotFound {
		return accessContext{}, internalError(err)
	}

	groupIDs, err := s.db.ListGroupIDsForUser(ctx, c.session.UserID)
	if err != nil {
		return accessContext{}, internalError(err)
	}
	directBoardIDs, err := s.db.ListBoardIDsForUser(ctx, c.session.UserID)
	if err != nil {
		return accessContext{}, internalError(err)
	}
	groupBoardIDs, err := s.db.ListBoardIDsForGroups(ctx, groupIDs)
	if err != nil {
		return accessContext{}, internalError(err)
	}
	out.boardIDs = append(append(out.boardIDs, directBoardIDs...), groupBoardIDs...)

	groupPerms, err := s.db.ListGroupPermissionsForGroups(ctx, groupIDs)
	if err != nil {
		return accessContext{}, internalError(err)
	}
	keys := make([]string, 0, len(groupPerms))
	for _, p := range groupPerms {
		keys = append(keys, p.Permission)
	}
	out.canViewAllBoards = expandPermissions(keys)["board-view-all"]
	return out, nil
}

// canViewAll mirrors ctx.session.user.permissions.includes("board-view-all")
// in searchAccessibleBoardsAsync — the session permissions already carry the
// group expansion.
func (c caller) canViewAll() bool {
	return c.hasPermission("board-view-all")
}
