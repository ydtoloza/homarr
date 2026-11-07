// Package home is the Go port of the home tRPC router
// (packages/api/src/router/home.ts): the statistics cards of the management
// dashboard (home.getStats). One public procedure that counts what the
// caller may see, one card per reachable page:
//
//	board → user → invite → integration → app → group → searchEngine → media
//
// in exactly that insertion order (the frontend renders the array as-is).
// The visibility rules live upstream in @homarr/definitions
// (getAppManagementAccess / getIntegrationManagementAccess,
// permissions.ts:231-254) "so the navigation, the pages themselves and the
// management home statistics cannot drift apart".
//
// Fork notes: isProviderEnabled("credentials") is always true (the fork
// only ships the credentials provider, see AUTH docs), so the invite card
// appears for administrators exactly like upstream; env.NO_EXTERNAL_CONNECTION
// is not ported (same decision as the other routers).
package home

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Register registers the home router. Call once at startup.
func Register(r *trpc.Router, database *db.DB, _ Options) {
	s := &service{db: database}

	// home.getStats (home.ts:66-171): publicProcedure — anonymous callers
	// get the public-boards card only.
	r.Register("home.getStats", s.getStats)
}

// Options configures the home router. The zero value is valid; the type
// keeps the uniform Register signature of the other routers.
type Options struct{}

type service struct {
	db *db.DB
}

// statistic is the HomeStatistic interface of home.ts:53-58.
type statistic struct {
	TitleKey    string `json:"titleKey"`
	SubtitleKey string `json:"subtitleKey"`
	Count       int64  `json:"count"`
	Path        string `json:"path"`
}

// getStats mirrors the body of home.getStats (home.ts:67-170) card by card,
// in the upstream insertion order.
func (s *service) getStats(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	// ctx.session?.user.permissions.includes("admin") — the expanded group
	// permission set, administrators pass.
	isAdmin := caller.IsAdmin()

	userID := ""
	if caller.Session != nil {
		userID = caller.Session.UserID
	}

	// boardIds: direct board permissions + group board permissions of the
	// memberships (home.ts:71-101), collected only when the user cannot view
	// all boards.
	var boardIDs []string
	if caller.Session != nil && !caller.HasPermission("board-view-all") {
		direct, err := s.db.ListBoardIDsForUser(ctx, userID)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		groupIDs, err := s.db.ListGroupIDsForUser(ctx, userID)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		viaGroups, err := s.db.ListBoardIDsForGroups(ctx, groupIDs)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		boardIDs = append(append(boardIDs, direct...), viaGroups...)
	}

	statistics := []statistic{}

	// Card 1: board (home.ts:103-121).
	boardCount, err := s.countBoards(ctx, caller, userID, boardIDs)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	statistics = append(statistics, statistic{
		TitleKey: "board", SubtitleKey: "boards", Count: boardCount, Path: "/manage/boards",
	})

	// Card 2: user, administrators only (home.ts:122-129).
	if isAdmin {
		count, err := s.count(ctx, `SELECT COUNT(*) FROM "user"`)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		statistics = append(statistics, statistic{
			TitleKey: "user", SubtitleKey: "authentication", Count: count, Path: "/manage/users",
		})
	}

	// Card 3: invite, administrators only + credentials enabled (always true
	// in the fork) (home.ts:130-137).
	if isAdmin {
		count, err := s.count(ctx, `SELECT COUNT(*) FROM "invite"`)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		statistics = append(statistics, statistic{
			TitleKey: "invite", SubtitleKey: "authentication", Count: count, Path: "/manage/users/invites",
		})
	}

	// Card 4: integration (home.ts:139-161) — the management access rules of
	// getIntegrationManagementAccess (permissions.ts:242-254): full-all,
	// create or at least one delegated "full" permission.
	hasGlobalFull := caller.HasPermission("integration-full-all")
	fullAccessIDs := []string{}
	if !hasGlobalFull && caller.Session != nil {
		fullAccessIDs, err = s.listFullAccessIntegrationIDs(ctx, userID)
		if err != nil {
			return nil, admin.InternalError(err)
		}
	}
	integrationCanManageAll := hasGlobalFull
	integrationCanCreate := caller.HasPermission("integration-create")
	integrationCanAccess := integrationCanManageAll || integrationCanCreate || len(fullAccessIDs) > 0
	if integrationCanAccess {
		var count int64
		if integrationCanManageAll {
			count, err = s.count(ctx, `SELECT COUNT(*) FROM "integration"`)
			if err != nil {
				return nil, admin.InternalError(err)
			}
		} else {
			count = int64(len(fullAccessIDs))
		}
		statistics = append(statistics, statistic{
			TitleKey: "integration", SubtitleKey: "resources", Count: count, Path: "/manage/integrations",
		})
	}

	// Card 5: app (home.ts:162-171) — getAppManagementAccess
	// (permissions.ts:231-239): modify-all, create; the count only for
	// manage-all (0 otherwise, upstream: appAccess.canManageAll ? count : 0).
	appCanManageAll := caller.HasPermission("app-modify-all")
	appCanCreate := caller.HasPermission("app-create")
	if appCanManageAll || appCanCreate {
		var count int64
		if appCanManageAll {
			count, err = s.count(ctx, `SELECT COUNT(*) FROM "app"`)
			if err != nil {
				return nil, admin.InternalError(err)
			}
		}
		statistics = append(statistics, statistic{
			TitleKey: "app", SubtitleKey: "resources", Count: count, Path: "/manage/apps",
		})
	}

	// Card 6: group, administrators only (home.ts:172-179).
	if isAdmin {
		count, err := s.count(ctx, `SELECT COUNT(*) FROM "group"`)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		statistics = append(statistics, statistic{
			TitleKey: "group", SubtitleKey: "authorization", Count: count, Path: "/manage/users/groups",
		})
	}

	// Card 7: searchEngine, search-engine-create permission
	// (home.ts:180-187).
	if caller.HasPermission("search-engine-create") {
		count, err := s.count(ctx, `SELECT COUNT(*) FROM "search_engine"`)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		statistics = append(statistics, statistic{
			TitleKey: "searchEngine", SubtitleKey: "resources", Count: count, Path: "/manage/search-engines",
		})
	}

	// Card 8: media, media-upload permission; the count is filtered to the
	// caller unless media-view-all (home.ts:188-201).
	if caller.HasPermission("media-upload") {
		query := `SELECT COUNT(*) FROM "media"`
		if !caller.HasPermission("media-view-all") {
			query += ` WHERE creator_id = ?`
			count, err := s.count(ctx, query, userID)
			if err != nil {
				return nil, admin.InternalError(err)
			}
			statistics = append(statistics, statistic{
				TitleKey: "media", SubtitleKey: "resources", Count: count, Path: "/manage/medias",
			})
		} else {
			count, err := s.count(ctx, query)
			if err != nil {
				return nil, admin.InternalError(err)
			}
			statistics = append(statistics, statistic{
				TitleKey: "media", SubtitleKey: "resources", Count: count, Path: "/manage/medias",
			})
		}
	}

	return statistics, nil
}

// countBoards mirrors the board card filter (home.ts:103-120): users with
// board-view-all count every board; others count public + owned + explicitly
// permitted; anonymous callers count public boards (the drizzle or() with
// creatorId = "" matches nothing else).
func (s *service) countBoards(ctx context.Context, caller admin.Caller, userID string, boardIDs []string) (int64, error) {
	if caller.HasPermission("board-view-all") {
		// undefined where: no filter.
		return s.count(ctx, `SELECT COUNT(*) FROM "board"`)
	}
	query := `SELECT COUNT(*) FROM "board" b WHERE `
	var args []any
	args = append(args, userID)
	query += `(b.is_public = 1 OR b.creator_id = ?`
	if len(boardIDs) > 0 {
		placeholders := make([]string, len(boardIDs))
		for i, id := range boardIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` OR b.id IN (` + strings.Join(placeholders, ", ") + `)`
	}
	query += `)`
	return s.count(ctx, query, args...)
}

// listFullAccessIntegrationIDs is getFullAccessIntegrationIdsAsync
// (home.ts:23-46): the integrations where the user's direct or group
// permission rows include "full" (constructIntegrationPermissions with a
// null session reduces to permissions.includes("full"),
// integration-permissions.ts:16-30).
func (s *service) listFullAccessIntegrationIDs(ctx context.Context, userID string) ([]string, error) {
	groupIDs, err := s.db.ListGroupIDsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	query := `SELECT DISTINCT i.id FROM "integration" i WHERE i.id IN (
		SELECT integration_id FROM "integrationUserPermission" WHERE user_id = ? AND permission = 'full'`
	args := []any{userID}
	if len(groupIDs) > 0 {
		placeholders := make([]string, len(groupIDs))
		for i, id := range groupIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` UNION SELECT integration_id FROM "integrationGroupPermissions" WHERE permission = 'full'
			AND group_id IN (` + strings.Join(placeholders, ", ") + `)`
	}
	query += `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *service) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}
