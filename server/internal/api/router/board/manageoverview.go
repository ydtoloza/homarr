package board

// board.getManageOverview — the read-model of the manage boards page
// (board.ts:375-594): every board the TARGET user may see with the raw
// material of its grid preview (the base layout, the root empty sections per
// lane, the visible containers and the items inside the preview row limit,
// plus the app icons), flattened for the management cards.
//
// The procedure is publicProcedure but viewing ANOTHER user's overview
// requires the admin permission (board.ts:377-381): "You are not allowed to
// view boards for this user". The access context is resolved for the target
// user (getBoardAccessContextAsync, board.ts:2841-2866, is keyed by userId —
// unlike the caller-keyed context the other procedures use), so an
// administrator previewing someone else sees THAT user's boards and home
// flags.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

const (
	// defaultManageOverviewPreviewRows / maxManageOverviewPreviewRows
	// (board.ts:95-97).
	defaultManageOverviewPreviewRows = int64(12)
	maxManageOverviewPreviewRows     = int64(48)
)

// manageOverviewInput is manageOverviewInputSchema (board.ts:98-105): all
// keys optional; fullPreview forces the 48-row limit.
type manageOverviewInput struct {
	FullPreview     bool
	PreviewRowLimit int64 // 0 = default
	UserID          *string
}

// manageOverviewOutput is one element of the response.
type manageOverviewOutput struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	LogoImageUrl     *string           `json:"logoImageUrl"`
	IsPublic         bool              `json:"isPublic"`
	PrimaryColor     string            `json:"primaryColor"`
	Creator          *creatorOutput    `json:"creator"`
	UserPermissions  []permissionEntry `json:"userPermissions"`
	GroupPermissions []permissionEntry `json:"groupPermissions"`
	IsHome           bool              `json:"isHome"`
	IsMobileHome     bool              `json:"isMobileHome"`
	Preview          *previewOutput    `json:"preview"`
}

// previewOutput is the preview object (board.ts:582-592).
type previewOutput struct {
	PrimaryColor string           `json:"primaryColor"`
	Layouts      []previewLayout  `json:"layouts"`
	Sections     []previewSection `json:"sections"`
	Items        []previewItem    `json:"items"`
}

// previewLayout is the layout column set of the query (board.ts:421-428).
type previewLayout struct {
	ID                     string `json:"id"`
	ColumnCount            int64  `json:"columnCount"`
	LeftGutterColumnCount  int64  `json:"leftGutterColumnCount"`
	RightGutterColumnCount int64  `json:"rightGutterColumnCount"`
	Breakpoint             int64  `json:"breakpoint"`
	Role                   string `json:"role"`
}

// previewSection is one element of preview.sections: root empty sections
// carry their xOffset and no layouts; containers carry their placement and a
// null xOffset (board.ts:557-586).
type previewSection struct {
	ID      string              `json:"id"`
	Kind    string              `json:"kind"`
	XOffset *int64              `json:"xOffset"`
	Layouts []sectionLayoutWire `json:"layouts"`
}

// sectionLayoutWire is the sectionLayouts column set (board.ts:452-460).
type sectionLayoutWire struct {
	SectionID       string  `json:"sectionId"`
	LayoutID        string  `json:"layoutId"`
	ParentSectionID *string `json:"parentSectionId"`
	XOffset         int64   `json:"xOffset"`
	YOffset         int64   `json:"yOffset"`
	Width           int64   `json:"width"`
	Height          int64   `json:"height"`
}

// previewItem is one element of preview.items; iconUrl is omitted unless the
// item is an app widget with a resolvable app (board.ts:588-598).
type previewItem struct {
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`
	IconURL *string          `json:"iconUrl,omitempty"`
	Layouts []itemLayoutWire `json:"layouts"`
}

// itemLayoutWire is the itemLayouts column set (board.ts:439-447).
type itemLayoutWire struct {
	ItemID    string `json:"itemId"`
	LayoutID  string `json:"layoutId"`
	SectionID string `json:"sectionId"`
	XOffset   int64  `json:"xOffset"`
	YOffset   int64  `json:"yOffset"`
	Width     int64  `json:"width"`
	Height    int64  `json:"height"`
}

// boardGetManageOverview is board.getManageOverview (board.ts:375-594).
func (s *service) boardGetManageOverview(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	caller, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}

	in, terr := parseManageOverviewInput(input)
	if terr != nil {
		return nil, terr
	}

	// The target user: input.userId ?? sessionUserId (board.ts:378).
	sessionUserID := ""
	if c := caller.session; c != nil {
		sessionUserID = c.UserID
	}
	targetUserID := sessionUserID
	if in.UserID != nil {
		targetUserID = *in.UserID
	}
	// Viewing another user's overview requires the admin permission
	// (board.ts:379-381).
	if targetUserID != sessionUserID && !caller.isAdmin() {
		return nil, trpc.NewError(trpc.CodeForbidden, "You are not allowed to view boards for this user")
	}

	// getBoardAccessContextAsync for the target user: direct + group board
	// ids, home boards and the expanded group permission keys. An unknown
	// user resolves like an anonymous one (the findFirst with id "" matches
	// nothing upstream).
	access, terr := s.manageAccessContextForUser(ctx, targetUserID)
	if terr != nil {
		return nil, terr
	}

	previewRowLimit := defaultManageOverviewPreviewRows
	if in.PreviewRowLimit > 0 {
		previewRowLimit = in.PreviewRowLimit
	}
	if in.FullPreview {
		previewRowLimit = maxManageOverviewPreviewRows
	}

	boards, terr := s.listManageOverviewBoards(ctx, targetUserID, access)
	if terr != nil {
		return nil, terr
	}

	// The preview material of every board (board.ts:432-539).
	layoutIDs := make([]string, 0, len(boards))
	rootSectionIDs := make([]string, 0, len(boards))
	previewLayoutByBoard := map[string]previewLayout{}
	for _, b := range boards {
		if b.Layout.ID != "" {
			layoutIDs = append(layoutIDs, b.Layout.ID)
			previewLayoutByBoard[b.ID] = b.Layout
		}
		for _, sec := range b.Sections {
			rootSectionIDs = append(rootSectionIDs, sec.ID)
		}
	}

	rootItemLayouts := []itemLayoutWithItem{}
	sectionLayouts := []sectionLayoutWithSection{}
	if len(layoutIDs) > 0 && len(rootSectionIDs) > 0 {
		rootItemLayouts, terr = s.listPreviewItemLayouts(ctx, layoutIDs, rootSectionIDs, previewRowLimit)
		if terr != nil {
			return nil, terr
		}
		sectionLayouts, terr = s.listPreviewSectionLayouts(ctx, layoutIDs, rootSectionIDs, previewRowLimit)
		if terr != nil {
			return nil, terr
		}
	}

	// Nested items inside VISIBLE containers (board.ts:498-521).
	containerSectionIDs := []string{}
	seenContainer := map[string]bool{}
	for _, sl := range sectionLayouts {
		if sl.SectionKind == "container" && !seenContainer[sl.SectionID] {
			seenContainer[sl.SectionID] = true
			containerSectionIDs = append(containerSectionIDs, sl.SectionID)
		}
	}
	nestedItemLayouts := []itemLayoutWithItem{}
	if len(layoutIDs) > 0 && len(containerSectionIDs) > 0 {
		nestedItemLayouts, terr = s.listPreviewItemLayouts(ctx, layoutIDs, containerSectionIDs, previewRowLimit)
		if terr != nil {
			return nil, terr
		}
	}
	previewItemLayouts := make([]itemLayoutWithItem, 0, len(rootItemLayouts)+len(nestedItemLayouts))
	previewItemLayouts = append(previewItemLayouts, rootItemLayouts...)
	previewItemLayouts = append(previewItemLayouts, nestedItemLayouts...)

	// The app icons of app widgets (board.ts:523-541): item kind "app" +
	// options {appId: string}.
	appIDByItemID := map[string]string{}
	for _, l := range previewItemLayouts {
		if l.ItemKind != "app" {
			continue
		}
		if appID, ok := appIDOfItemOptions(l.ItemOptions); ok {
			appIDByItemID[l.ItemID] = appID
		}
	}
	appIDs := make([]string, 0, len(appIDByItemID))
	seen := map[string]bool{}
	for _, id := range appIDByItemID {
		if !seen[id] {
			seen[id] = true
			appIDs = append(appIDs, id)
		}
	}
	appIconURLByID := map[string]string{}
	if len(appIDs) > 0 {
		appIconURLByID, terr = s.listAppIcons(ctx, appIDs)
		if terr != nil {
			return nil, terr
		}
	}

	return assembleManageOverview(boards, previewLayoutByBoard, sectionLayouts,
		previewItemLayouts, appIDByItemID, appIconURLByID, access), nil
}

// parseManageOverviewInput mirrors manageOverviewInputSchema (board.ts:98-105):
// an absent input is the zero input; the row limit is 1-48.
func parseManageOverviewInput(input json.RawMessage) (*manageOverviewInput, *trpc.Error) {
	in := &manageOverviewInput{}
	if len(input) == 0 || string(input) == "null" {
		return in, nil
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	if b, ok, terr := dec.optBool("fullPreview"); terr != nil {
		return nil, terr
	} else if ok {
		in.FullPreview = b
	}
	if n, ok, terr := dec.optInt64("previewRowLimit", 1, maxManageOverviewPreviewRows); terr != nil {
		return nil, terr
	} else if ok {
		in.PreviewRowLimit = n
	}
	if s, ok, terr := dec.optStr("userId"); terr != nil {
		return nil, terr
	} else if ok {
		in.UserID = &s
	}
	return in, nil
}

// manageAccessContext is the userId-keyed access context of
// getBoardAccessContextAsync (board.ts:2841-2866).
type manageAccessContext struct {
	boardIDs        []string
	homeBoardID     string // empty = none
	mobileHomeBoard string // empty = none
	canViewAll      bool
}

// manageAccessContextForUser resolves the target user's visibility context.
func (s *service) manageAccessContextForUser(ctx context.Context, userID string) (*manageAccessContext, *trpc.Error) {
	out := &manageAccessContext{}
	if userID == "" {
		return out, nil
	}
	// currentUser: home boards (a findFirst miss leaves the defaults, like
	// the upstream currentUser?.homeBoardId chains).
	if homeBoards, err := s.db.GetUserHomeBoards(ctx, userID); err == nil {
		out.homeBoardID = homeBoards.HomeBoardID
		out.mobileHomeBoard = homeBoards.MobileHomeBoardID
	} else if err != db.ErrNotFound {
		return nil, internalError(err)
	}

	groupIDs, err := s.db.ListGroupIDsForUser(ctx, userID)
	if err != nil {
		return nil, internalError(err)
	}
	directBoardIDs, err := s.db.ListBoardIDsForUser(ctx, userID)
	if err != nil {
		return nil, internalError(err)
	}
	groupBoardIDs, err := s.db.ListBoardIDsForGroups(ctx, groupIDs)
	if err != nil {
		return nil, internalError(err)
	}
	out.boardIDs = append(append(out.boardIDs, directBoardIDs...), groupBoardIDs...)

	groupPerms, err := s.db.ListGroupPermissionsForGroups(ctx, groupIDs)
	if err != nil {
		return nil, internalError(err)
	}
	keys := make([]string, 0, len(groupPerms))
	for _, p := range groupPerms {
		keys = append(keys, p.Permission)
	}
	out.canViewAll = expandPermissions(keys)["board-view-all"]
	return out, nil
}

// manageOverviewBoardRow is one board row of the findMany (board.ts:404-438):
// the selected columns plus the creator and the target user's permission
// lists.
type manageOverviewBoardRow struct {
	ID           string
	Name         string
	LogoImageURL string
	IsPublic     bool
	PrimaryColor string
	CreatorID    string
	Creator      *creatorOutput

	UserPermissions  []permissionEntry
	GroupPermissions []permissionEntry

	Layout   previewLayout // ID empty = none
	Sections []manageOverviewSection
}

// manageOverviewSection is one root empty section (board.ts:429-437).
type manageOverviewSection struct {
	ID      string
	XOffset int64
}

// listManageOverviewBoards loads the board rows with the
// getAccessibleBoardsWhere filter (board.ts:439) plus the creator and the
// target user's permission rows.
func (s *service) listManageOverviewBoards(ctx context.Context, targetUserID string, access *manageAccessContext) ([]manageOverviewBoardRow, *trpc.Error) {
	query := `SELECT b.id, b.name, b.logo_image_url, b.is_public, b.primary_color, b.creator_id FROM "board" b`
	var args []any
	if !access.canViewAll {
		args = append(args, targetUserID)
		query += ` WHERE (b.is_public = 1 OR b.creator_id = ?`
		if len(access.boardIDs) > 0 {
			placeholders := make([]string, len(access.boardIDs))
			for i, id := range access.boardIDs {
				placeholders[i] = "?"
				args = append(args, id)
			}
			query += ` OR b.id IN (` + strings.Join(placeholders, ", ") + `)`
		}
		query += `)`
	}
	// No ORDER BY: the drizzle findMany without one returns rowid order.
	query += ` ORDER BY b.rowid`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	out := []manageOverviewBoardRow{}
	boardIDs := []string{}
	for rows.Next() {
		var (
			r    manageOverviewBoardRow
			logo sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Name, &logo, &r.IsPublic, &r.PrimaryColor, &r.CreatorID); err != nil {
			return nil, internalError(err)
		}
		r.LogoImageURL = logo.String
		out = append(out, r)
		boardIDs = append(boardIDs, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}

	// The per-board relations: creator, the target user's direct permission
	// rows and the group permission rows (board.ts:408-423).
	directByBoard := map[string][]permissionEntry{}
	if targetUserID != "" {
		direct, err := s.db.ListBoardUserPermissionsForUser(ctx, targetUserID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range direct {
			directByBoard[p.BoardID] = append(directByBoard[p.BoardID], permissionEntry{Permission: p.Permission})
		}
	}
	groupPermsByBoard := map[string][]permissionEntry{}
	if targetUserID != "" {
		groupIDs, err := s.db.ListGroupIDsForUser(ctx, targetUserID)
		if err != nil {
			return nil, internalError(err)
		}
		if len(groupIDs) > 0 {
			groupRows, err := s.db.ListBoardGroupPermissionsForGroups(ctx, groupIDs)
			if err != nil {
				return nil, internalError(err)
			}
			for _, p := range groupRows {
				groupPermsByBoard[p.BoardID] = append(groupPermsByBoard[p.BoardID], permissionEntry{Permission: p.Permission})
			}
		}
	}

	for i := range out {
		b := &out[i]
		b.UserPermissions = orEmptyPerms(directByBoard[b.ID])
		b.GroupPermissions = orEmptyPerms(groupPermsByBoard[b.ID])
		if b.CreatorID != "" {
			creator, err := s.db.GetUserByID(ctx, b.CreatorID)
			if err == nil {
				b.Creator = &creatorOutput{ID: creator.ID, Name: nullableStr(creator.Name), Image: nullableStr(creator.Image)}
			} else if err != db.ErrNotFound {
				return nil, internalError(err)
			}
		}
		// The base-role, highest-breakpoint layout (board.ts:421-428: the
		// CASE/breakpoint DESC/limit 1 selection), and the root empty
		// sections (board.ts:429-437).
		if terr := s.loadManageOverviewLayoutAndSections(ctx, b); terr != nil {
			return nil, terr
		}
	}
	return out, nil
}

// loadManageOverviewLayoutAndSections loads the preview layout and the root
// empty sections of one board.
func (s *service) loadManageOverviewLayoutAndSections(ctx context.Context, b *manageOverviewBoardRow) *trpc.Error {
	layoutRow := s.db.QueryRowContext(ctx, `SELECT id, column_count, left_gutter_column_count, right_gutter_column_count,
		breakpoint, role FROM "layout" WHERE board_id = ?
		ORDER BY CASE WHEN role = 'base' THEN 0 ELSE 1 END, breakpoint DESC LIMIT 1`, b.ID)
	var l previewLayout
	err := layoutRow.Scan(&l.ID, &l.ColumnCount, &l.LeftGutterColumnCount, &l.RightGutterColumnCount, &l.Breakpoint, &l.Role)
	switch {
	case err == nil:
		b.Layout = l
	case err == sql.ErrNoRows:
		// no layout: preview null
	default:
		return internalError(err)
	}

	secRows, err := s.db.QueryContext(ctx, `SELECT id, x_offset FROM "section"
		WHERE board_id = ? AND kind = 'empty' ORDER BY x_offset, id`, b.ID)
	if err != nil {
		return internalError(err)
	}
	defer secRows.Close()
	b.Sections = []manageOverviewSection{}
	for secRows.Next() {
		var (
			sec    manageOverviewSection
			offset sql.NullInt64
		)
		if err := secRows.Scan(&sec.ID, &offset); err != nil {
			return internalError(err)
		}
		sec.XOffset = offset.Int64
		b.Sections = append(b.Sections, sec)
	}
	if err := secRows.Err(); err != nil {
		return internalError(err)
	}
	return nil
}

// itemLayoutWithItem is one preview item layout plus the item columns the
// query carries (board.ts:439-448).
type itemLayoutWithItem struct {
	itemLayoutWire
	ItemKind    string
	ItemOptions string
}

// listPreviewItemLayouts is the itemLayouts query of board.ts:432-450 and
// board.ts:498-521: the layouts of the given sections inside the preview row
// limit, joined with the item kind and options.
func (s *service) listPreviewItemLayouts(ctx context.Context, layoutIDs, sectionIDs []string, previewRowLimit int64) ([]itemLayoutWithItem, *trpc.Error) {
	query := `SELECT il.item_id, il.layout_id, il.section_id, il.x_offset, il.y_offset, il.width, il.height,
		i.kind, i.options
		FROM "item_layout" il JOIN "item" i ON i.id = il.item_id
		WHERE il.layout_id IN (` + placeholders(len(layoutIDs)) + `)
		AND il.section_id IN (` + placeholders(len(sectionIDs)) + `)
		AND il.x_offset >= 0 AND il.y_offset >= 0 AND il.y_offset < ?
		AND il.width > 0 AND il.height > 0`
	args := []any{}
	for _, id := range layoutIDs {
		args = append(args, id)
	}
	for _, id := range sectionIDs {
		args = append(args, id)
	}
	args = append(args, previewRowLimit)
	return scanPreviewItemLayouts(s.db.QueryContext(ctx, query, args...))
}

// listPreviewSectionLayouts is the sectionLayouts query of board.ts:451-478:
// the container placements of the root sections inside the preview row limit,
// joined with the section kind.
func (s *service) listPreviewSectionLayouts(ctx context.Context, layoutIDs, rootSectionIDs []string, previewRowLimit int64) ([]sectionLayoutWithSection, *trpc.Error) {
	query := `SELECT sl.section_id, sl.layout_id, sl.parent_section_id, sl.x_offset, sl.y_offset, sl.width, sl.height,
		sc.kind
		FROM "section_layout" sl JOIN "section" sc ON sc.id = sl.section_id
		WHERE sl.layout_id IN (` + placeholders(len(layoutIDs)) + `)
		AND sl.parent_section_id IN (` + placeholders(len(rootSectionIDs)) + `)
		AND sl.x_offset >= 0 AND sl.y_offset >= 0 AND sl.y_offset < ?
		AND sl.width > 0 AND sl.height > 0`
	args := []any{}
	for _, id := range layoutIDs {
		args = append(args, id)
	}
	for _, id := range rootSectionIDs {
		args = append(args, id)
	}
	args = append(args, previewRowLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	out := []sectionLayoutWithSection{}
	for rows.Next() {
		var (
			sl     sectionLayoutWithSection
			parent sql.NullString
		)
		if err := rows.Scan(&sl.SectionID, &sl.LayoutID, &parent, &sl.XOffset, &sl.YOffset, &sl.Width, &sl.Height, &sl.SectionKind); err != nil {
			return nil, internalError(err)
		}
		if parent.Valid {
			sl.ParentSectionID = &parent.String
		}
		out = append(out, sl)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}
	return out, nil
}

// sectionLayoutWithSection is one preview section layout plus the section
// kind (board.ts:452-466).
type sectionLayoutWithSection struct {
	sectionLayoutWire
	SectionKind string
}

// listAppIcons is the preview apps query (board.ts:533-541): {id, iconUrl}
// of the referenced apps.
func (s *service) listAppIcons(ctx context.Context, appIDs []string) (map[string]string, *trpc.Error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, icon_url FROM "app" WHERE id IN (`+placeholders(len(appIDs))+`)`, toArgs(appIDs)...)
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			id   string
			icon sql.NullString
		)
		if err := rows.Scan(&id, &icon); err != nil {
			return nil, internalError(err)
		}
		if icon.Valid {
			out[id] = icon.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}
	return out, nil
}

// appIDOfItemOptions extracts the appId of an app widget's options (the
// superjson payload the item stores, board.ts:524-536): a missing/invalid
// envelope or a non-string appId contributes nothing.
func appIDOfItemOptions(options string) (string, bool) {
	if options == "" {
		return "", false
	}
	var envelope struct {
		JSON struct {
			AppID any `json:"appId"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(options), &envelope); err != nil {
		return "", false
	}
	if id, ok := envelope.JSON.AppID.(string); ok {
		return id, true
	}
	return "", false
}

// assembleManageOverview renders the response mapping (board.ts:542-596):
// per board, the lane roots, the visible containers and the items filtered to
// the preview layout.
func assembleManageOverview(
	boards []manageOverviewBoardRow,
	previewLayoutByBoard map[string]previewLayout,
	sectionLayouts []sectionLayoutWithSection,
	previewItemLayouts []itemLayoutWithItem,
	appIDByItemID map[string]string,
	appIconURLByID map[string]string,
	access *manageAccessContext,
) []manageOverviewOutput {
	out := make([]manageOverviewOutput, 0, len(boards))
	for _, b := range boards {
		entry := manageOverviewOutput{
			ID:               b.ID,
			Name:             b.Name,
			LogoImageUrl:     nullableStr(b.LogoImageURL),
			IsPublic:         b.IsPublic,
			PrimaryColor:     b.PrimaryColor,
			Creator:          b.Creator,
			UserPermissions:  b.UserPermissions,
			GroupPermissions: b.GroupPermissions,
		}
		entry.IsHome = access.homeBoardID == b.ID
		entry.IsMobileHome = access.mobileHomeBoard == b.ID

		layout, hasLayout := previewLayoutByBoard[b.ID]
		if !hasLayout {
			out = append(out, entry) // preview: null
			continue
		}

		// rootsByLane (board.ts:547-556): the first empty section per lane in
		// the section order (x_offset, id).
		rootsByLane := map[string]manageOverviewSection{}
		for _, sec := range b.Sections {
			lane := getRootSectionLane(sec.XOffset)
			if _, ok := rootsByLane[lane]; !ok {
				rootsByLane[lane] = sec
			}
		}
		previewRoots := []previewSection{}
		rootColumnCountByID := map[string]int64{}
		for _, lane := range boardLanes {
			root, ok := rootsByLane[lane]
			if !ok {
				continue
			}
			offset := root.XOffset
			previewRoots = append(previewRoots, previewSection{
				ID: root.ID, Kind: "empty", XOffset: &offset, Layouts: []sectionLayoutWire{},
			})
			rootColumnCountByID[root.ID] = getBoardLaneColumnCount(layoutGeometryOf(layoutFromPreview(layout)), lane)
		}

		isInsideRootLane := func(sectionID string, xOffset int64) bool {
			columnCount, ok := rootColumnCountByID[sectionID]
			return ok && columnCount > 0 && xOffset < columnCount
		}
		isInsideParentRootLane := func(parentSectionID *string, xOffset int64) bool {
			if parentSectionID == nil {
				return false
			}
			columnCount, ok := rootColumnCountByID[*parentSectionID]
			return ok && columnCount > 0 && xOffset < columnCount
		}

		// containerPreview (board.ts:567-586): the container placements of
		// THIS layout that fit inside their root lane.
		containerPreview := []previewSection{}
		visibleContainerSize := map[string]sectionLayoutWire{}
		for _, sl := range sectionLayouts {
			if sl.LayoutID != layout.ID || sl.SectionKind != "container" || !isInsideParentRootLane(sl.ParentSectionID, sl.XOffset) {
				continue
			}
			containerPreview = append(containerPreview, previewSection{
				ID: sl.SectionID, Kind: "container", XOffset: nil, Layouts: []sectionLayoutWire{sl.sectionLayoutWire},
			})
			visibleContainerSize[sl.SectionID] = sl.sectionLayoutWire
		}
		isInsideVisibleContainer := func(sectionID string, xOffset, yOffset int64) bool {
			container, ok := visibleContainerSize[sectionID]
			return ok && xOffset < container.Width && yOffset < container.Height
		}

		// itemPreview (board.ts:588-598): the items of THIS layout inside a
		// root lane or a visible container.
		itemPreview := []previewItem{}
		for _, l := range previewItemLayouts {
			if l.LayoutID != layout.ID {
				continue
			}
			if !isInsideRootLane(l.SectionID, l.XOffset) && !isInsideVisibleContainer(l.SectionID, l.XOffset, l.YOffset) {
				continue
			}
			item := previewItem{
				ID:      l.ItemID,
				Kind:    l.ItemKind,
				Layouts: []itemLayoutWire{l.itemLayoutWire},
			}
			if appID, ok := appIDByItemID[l.ItemID]; ok {
				if icon, ok := appIconURLByID[appID]; ok {
					item.IconURL = &icon
				}
			}
			itemPreview = append(itemPreview, item)
		}

		entry.Preview = &previewOutput{
			PrimaryColor: b.PrimaryColor,
			Layouts:      []previewLayout{layout},
			Sections:     append(previewRoots, containerPreview...),
			Items:        itemPreview,
		}
		out = append(out, entry)
	}
	return out
}

// layoutFromPreview adapts a previewLayout to the lane math geometry.
func layoutFromPreview(l previewLayout) db.Layout {
	return db.Layout{
		Role:                   l.Role,
		ColumnCount:            l.ColumnCount,
		LeftGutterColumnCount:  l.LeftGutterColumnCount,
		RightGutterColumnCount: l.RightGutterColumnCount,
	}
}

func orEmptyPerms(list []permissionEntry) []permissionEntry {
	if list == nil {
		return []permissionEntry{}
	}
	return list
}

// placeholders renders n SQL placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func toArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func scanPreviewItemLayouts(rows *sql.Rows, err error) ([]itemLayoutWithItem, *trpc.Error) {
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	out := []itemLayoutWithItem{}
	for rows.Next() {
		var (
			l       itemLayoutWithItem
			options sql.NullString
		)
		if err := rows.Scan(&l.ItemID, &l.LayoutID, &l.SectionID, &l.XOffset, &l.YOffset, &l.Width, &l.Height, &l.ItemKind, &options); err != nil {
			return nil, internalError(err)
		}
		l.ItemOptions = options.String
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}
	return out, nil
}
