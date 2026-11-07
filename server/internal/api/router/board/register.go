// Package board is the Go port of the board-domain tRPC routers of
// packages/api/src: boardRouter (router/board.ts), sectionRouter
// (router/section/section-router.ts), appRouter (router/app.ts) and
// userItemStateRouter (router/user-item-state.ts). Procedures are registered
// under their exact upstream dotted paths so the Homarr frontend keeps
// calling the same endpoints.
//
// Permission semantics mirror the upstream middlewares and access helpers:
//   - publicProcedure: anonymous callers allowed (ctxauth.FromContext may
//     return nil)
//   - protectedProcedure: anonymous callers get UNAUTHORIZED (the exact
//     upstream error of enforceUserIsAuthed, packages/api/src/trpc.ts:163-172)
//   - permissionRequiredProcedure: FORBIDDEN "Permission denied" unless the
//     caller's expanded group permissions contain the key
//   - board mutation guards: the throwIfActionForbiddenAsync contract
//     (packages/api/src/router/board/board-access.ts) — a caller without the
//     required board permission gets NOT_FOUND "Board not found" so board
//     existence cannot be snooped
package board

import (
	"context"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options configures the board-domain routers. The zero value is valid.
type Options struct {
	// DeviceTypeFromContext reports the caller device type ("desktop" or
	// "mobile") used by board.getHomeBoard. Upstream derives it from the
	// User-Agent header (ctx.deviceType = userAgent(headers).device.type,
	// packages/api/src/trpc.ts:65); the Go httpserver middleware can inject
	// the resolved value into the request context. When nil (or when the
	// hook returns anything else), "desktop" is assumed.
	DeviceTypeFromContext func(ctx context.Context) string
}

func (o *Options) deviceType(ctx context.Context) string {
	if o != nil && o.DeviceTypeFromContext != nil {
		if dt := o.DeviceTypeFromContext(ctx); dt == "mobile" || dt == "desktop" {
			return dt
		}
	}
	return "desktop"
}

// service carries the dependencies shared by all procedures of this package.
type service struct {
	db   *db.DB
	opts *Options
	// placementLocks serializes board item placements per board, mirroring
	// the boardItemPlacementTails promise chain of upstream
	// (packages/api/src/router/board.ts:95-119).
	placementLocks *keyedMutex
}

// Register registers every procedure of the four routers under their exact
// upstream paths. The signature is the integration seam for the httpserver
// package: after chi mounts trpc.Router.Handler() at /api/trpc/*, call
//
//	board.Register(r, database, board.Options{})
//
// exactly once at startup (Register panics on duplicate paths, like
// trpc.Router.Register).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	s := &service{db: database, opts: &opts, placementLocks: newKeyedMutex()}
	registerBoardRouter(r, s)
	registerSectionRouter(r, s)
	registerAppRouter(r, s)
	registerUserItemStateRouter(r, s)
}

// registerBoardRouter mirrors boardRouter (packages/api/src/router/board.ts:265).
// Every upstream procedure of the router is ported; mutations are wrapped
// with admin.Writable (the upstream credentialProcedure base middleware,
// packages/api/src/trpc.ts:130-135).
func registerBoardRouter(r *trpc.Router, s *service) {
	r.Register("board.exists", s.boardExists)
	r.Register("board.getPublicBoards", s.boardGetPublicBoards)
	r.Register("board.getBoardsForGroup", s.boardGetBoardsForGroup)
	r.Register("board.getAllBoards", s.boardGetAllBoards)
	r.Register("board.getManageOverview", s.boardGetManageOverview)
	r.Register("board.catalog", s.boardCatalog)
	r.Register("board.search", s.boardSearch)
	r.Register("board.createBoard", admin.Writable(s.boardCreateBoard))
	r.Register("board.duplicateBoard", admin.Writable(s.boardDuplicateBoard))
	r.Register("board.renameBoard", admin.Writable(s.boardRenameBoard))
	r.Register("board.changeBoardVisibility", admin.Writable(s.boardChangeBoardVisibility))
	r.Register("board.deleteBoard", admin.Writable(s.boardDeleteBoard))
	r.Register("board.setHomeBoard", admin.Writable(s.boardSetHomeBoard))
	r.Register("board.setMobileHomeBoard", admin.Writable(s.boardSetMobileHomeBoard))
	r.Register("board.getHomeBoard", s.boardGetHomeBoard)
	r.Register("board.getBoardByName", s.boardGetBoardByName)
	r.Register("board.getBoardSettings", s.boardGetBoardSettings)
	r.Register("board.saveLayouts", admin.Writable(s.boardSaveLayouts))
	r.Register("board.resetLayout", admin.Writable(s.boardResetLayout))
	r.Register("board.savePartialBoardSettings", admin.Writable(s.boardSavePartialBoardSettings))
	r.Register("board.saveBoard", admin.Writable(s.boardSaveBoard))
	r.Register("board.getBoardPermissions", s.boardGetBoardPermissions)
	r.Register("board.saveUserBoardPermissions", admin.Writable(s.boardSaveUserBoardPermissions))
	r.Register("board.saveGroupBoardPermissions", admin.Writable(s.boardSaveGroupBoardPermissions))
	r.Register("board.addItem", admin.Writable(s.boardAddItem))
	r.Register("board.updateItemLayout", admin.Writable(s.boardUpdateItemLayout))
}

// registerSectionRouter mirrors sectionRouter (section-router.ts:12-72).
func registerSectionRouter(r *trpc.Router, s *service) {
	r.Register("section.changeCollapsed", admin.Writable(s.sectionChangeCollapsed))
}

// registerAppRouter mirrors appRouter (packages/api/src/router/app.ts:23-330)
// minus app.detectMetadata, which the appmeta package registers (it drags
// the guarded-fetch machinery of the metadata detection with it).
func registerAppRouter(r *trpc.Router, s *service) {
	r.Register("app.getPaginated", s.appGetPaginated)
	r.Register("app.all", s.appAll)
	r.Register("app.search", s.appSearch)
	r.Register("app.selectable", s.appSelectable)
	r.Register("app.byId", s.appByID)
	r.Register("app.byIds", s.appByIDs)
	r.Register("app.create", admin.Writable(s.appCreate))
	r.Register("app.createMany", admin.Writable(s.appCreateMany))
	r.Register("app.update", admin.Writable(s.appUpdate))
	r.Register("app.delete", admin.Writable(s.appDelete))
}

// registerUserItemStateRouter mirrors userItemStateRouter
// (packages/api/src/router/user-item-state.ts:22-76).
func registerUserItemStateRouter(r *trpc.Router, s *service) {
	r.Register("userItemState.get", s.userItemStateGet)
	r.Register("userItemState.set", admin.Writable(s.userItemStateSet))
}
