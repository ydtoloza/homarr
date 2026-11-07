package board

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Read procedures of the board router. The output shapes replicate the
// upstream drizzle relational queries consumed by the frontend
// (getFullBoardWithWhereAsync, packages/api/src/router/board.ts:2941-3060 and
// the summary queries at :221-263 and :340-374).

// permissionEntry is {"permission": "view"|"modify"|"full"}.
type permissionEntry struct {
	Permission string `json:"permission"`
}

// creatorOutput is the creator relation: {"id", "name", "image"} with nulls
// preserved (the user columns are nullable upstream).
type creatorOutput struct {
	ID    string  `json:"id"`
	Name  *string `json:"name"`
	Image *string `json:"image"`
}

// boardSummaryOutput is the boardSummarySchema shape
// (packages/validation/src/board.ts:159-175) — the getAllBoards output.
type boardSummaryOutput struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	LogoImageUrl     *string           `json:"logoImageUrl"`
	IsPublic         bool              `json:"isPublic"`
	Creator          *creatorOutput    `json:"creator"`
	IsHome           bool              `json:"isHome"`
	IsMobileHome     bool              `json:"isMobileHome"`
	UserPermissions  []permissionEntry `json:"userPermissions"`
	GroupPermissions []permissionEntry `json:"groupPermissions"`
}

// boardPermissionsOutput is constructBoardPermissions' return value, emitted
// by board.catalog and board.search.
type boardPermissionsOutput struct {
	HasFullAccess   bool `json:"hasFullAccess"`
	HasChangeAccess bool `json:"hasChangeAccess"`
	HasViewAccess   bool `json:"hasViewAccess"`
}

// boardSearchOutput is the searchAccessibleBoardsAsync mapping
// (packages/api/src/router/board.ts:255-262).
type boardSearchOutput struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	LogoImageUrl *string                `json:"logoImageUrl"`
	Permissions  boardPermissionsOutput `json:"permissions"`
	IsHome       bool                   `json:"isHome"`
	IsMobileHome bool                   `json:"isMobileHome"`
}

// layoutOutput is a layout of the full board; resizeSource is omitted when
// unset, exactly like the upstream spread (board.ts:3017-3023).
type layoutOutput struct {
	ID                     string  `json:"id"`
	Name                   string  `json:"name"`
	ColumnCount            int64   `json:"columnCount"`
	LeftGutterColumnCount  int64   `json:"leftGutterColumnCount"`
	RightGutterColumnCount int64   `json:"rightGutterColumnCount"`
	Breakpoint             int64   `json:"breakpoint"`
	Role                   string  `json:"role"`
	ResizeSource           *string `json:"resizeSource,omitempty"`
}

// fullBoardOutput is the getFullBoardWithWhereAsync shape returned by
// board.getBoardByName and board.getHomeBoard.
type fullBoardOutput struct {
	ID                        string               `json:"id"`
	Name                      string               `json:"name"`
	IsPublic                  bool                 `json:"isPublic"`
	CreatorID                 *string              `json:"creatorId"`
	PageTitle                 *string              `json:"pageTitle"`
	MetaTitle                 *string              `json:"metaTitle"`
	LogoImageUrl              *string              `json:"logoImageUrl"`
	FaviconImageUrl           *string              `json:"faviconImageUrl"`
	BackgroundImageUrl        *string              `json:"backgroundImageUrl"`
	BackgroundImageAttachment string               `json:"backgroundImageAttachment"`
	BackgroundImageRepeat     string               `json:"backgroundImageRepeat"`
	BackgroundImageSize       string               `json:"backgroundImageSize"`
	PrimaryColor              string               `json:"primaryColor"`
	SecondaryColor            string               `json:"secondaryColor"`
	Opacity                   int64                `json:"opacity"`
	CustomCss                 *string              `json:"customCss"`
	IconColor                 *string              `json:"iconColor"`
	ItemRadius                string               `json:"itemRadius"`
	DisableStatus             bool                 `json:"disableStatus"`
	Creator                   *creatorOutput       `json:"creator"`
	UserPermissions           []permissionEntry    `json:"userPermissions"`
	GroupPermissions          []groupPermRowOutput `json:"groupPermissions"`
	Layouts                   []layoutOutput       `json:"layouts"`
	Sections                  []map[string]any     `json:"sections"`
	Items                     []map[string]any     `json:"items"`
}

// groupPermRowOutput is the full boardGroupPermission row kept by the full
// board (the relation has no column restriction, board.ts:2988-2990).
type groupPermRowOutput struct {
	BoardID    string `json:"boardId"`
	GroupID    string `json:"groupId"`
	Permission string `json:"permission"`
}

func nullableStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// containerSectionOptionsDefaults mirrors containerSectionOptionsDefaults
// (packages/validation/src/shared.ts:57-65).
var containerSectionOptionsDefaults = map[string]any{
	"title":            "",
	"customCssClasses": []any{},
	"borderColor":      "",
	"showLabel":        true,
	"collapsible":      false,
	"showOpenAll":      false,
	"scrollable":       false,
}

// getFullBoard is getFullBoardWithWhereAsync for a board id: assembles the
// complete board JSON the frontend renders, including the caller-scoped
// collapse states and permission rows. userID is nil for anonymous callers.
func (s *service) getFullBoard(ctx context.Context, boardID string, userID *string) (any, *trpc.Error) {
	board, err := s.db.GetBoardFullByID(ctx, boardID)
	if err != nil {
		return nil, notFoundBoard()
	}

	layoutRows, err := s.db.ListLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	// Repair of protected layouts (board.ts:3001-3011): legacy boards from
	// before the layouts migration are seeded on first read.
	if boardLayoutsNeedRepair(layoutRows) {
		if terr := s.repairProtectedLayouts(ctx, boardID, layoutRows); terr != nil {
			return nil, terr
		}
		if layoutRows, err = s.db.ListLayoutsByBoard(ctx, boardID); err != nil {
			return nil, internalError(err)
		}
	}
	db.SortLayoutsByBreakpoint(layoutRows)

	sections, err := s.db.ListSectionsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	sectionLayouts, err := s.db.ListSectionLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	items, err := s.db.ListItemsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	itemLayouts, err := s.db.ListItemLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	integrationItems, err := s.db.ListIntegrationItemsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}

	collapsedBySection := map[string]bool{}
	if userID != nil {
		states, err := s.db.ListSectionCollapseStatesForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, st := range states {
			collapsedBySection[st.SectionID] = st.Collapsed
		}
	}

	out := fullBoardOutput{
		ID:                        board.ID,
		Name:                      board.Name,
		IsPublic:                  board.IsPublic,
		CreatorID:                 nullableStr(board.CreatorID),
		PageTitle:                 nullableStr(board.PageTitle),
		MetaTitle:                 nullableStr(board.MetaTitle),
		LogoImageUrl:              nullableStr(board.LogoImageUrl),
		FaviconImageUrl:           nullableStr(board.FaviconImageUrl),
		BackgroundImageUrl:        nullableStr(board.BackgroundImageUrl),
		BackgroundImageAttachment: board.BackgroundImageAttachment,
		BackgroundImageRepeat:     board.BackgroundImageRepeat,
		BackgroundImageSize:       board.BackgroundImageSize,
		PrimaryColor:              board.PrimaryColor,
		SecondaryColor:            board.SecondaryColor,
		Opacity:                   int64(board.Opacity),
		CustomCss:                 nullableStr(board.CustomCss),
		IconColor:                 nullableStr(board.IconColor),
		ItemRadius:                board.ItemRadius,
		DisableStatus:             board.DisableStatus,
		UserPermissions:           []permissionEntry{},
		GroupPermissions:          []groupPermRowOutput{},
	}

	if board.CreatorID != "" {
		creator, err := s.db.GetUserByID(ctx, board.CreatorID)
		if err == nil {
			out.Creator = &creatorOutput{ID: creator.ID, Name: nullableStr(creator.Name), Image: nullableStr(creator.Image)}
		} else if err != db.ErrNotFound {
			return nil, internalError(err)
		}
		// A creator id without a user row renders creator: null upstream (the
		// drizzle relation is a left join).
	}

	if userID != nil {
		direct, err := s.db.ListBoardUserPermissionsForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range direct {
			if p.BoardID == boardID {
				out.UserPermissions = append(out.UserPermissions, permissionEntry{Permission: p.Permission})
			}
		}
		groups, err := s.db.ListBoardGroupPermissionsForUser(ctx, boardID, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range groups {
			out.GroupPermissions = append(out.GroupPermissions,
				groupPermRowOutput{BoardID: p.BoardID, GroupID: p.GroupID, Permission: p.Permission})
		}
	}

	out.Layouts = make([]layoutOutput, 0, len(layoutRows))
	for _, l := range layoutRows {
		entry := layoutOutput{
			ID:                     l.ID,
			Name:                   l.Name,
			ColumnCount:            l.ColumnCount,
			LeftGutterColumnCount:  l.LeftGutterColumnCount,
			RightGutterColumnCount: l.RightGutterColumnCount,
			Breakpoint:             l.Breakpoint,
			Role:                   l.Role,
		}
		if l.ResizeSource != "" {
			src := l.ResizeSource
			entry.ResizeSource = &src
		}
		out.Layouts = append(out.Layouts, entry)
	}

	out.Sections = make([]map[string]any, 0, len(sections))
	for _, sec := range sections {
		entry, terr := s.sectionOutput(sec, sectionLayouts, collapsedBySection)
		if terr != nil {
			return nil, terr
		}
		out.Sections = append(out.Sections, entry)
	}

	out.Items = make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := s.itemOutput(item, itemLayouts, integrationItems)
		if !ok {
			// parseItem skips items that fail the output schema with a warning
			// (board.ts:3086-3094).
			continue
		}
		out.Items = append(out.Items, entry)
	}

	return out, nil
}

// sectionOutput is parseSection (board.ts:3024-3040, 3096-3103): the section
// schema is a union and unknown keys are stripped, so empty and container
// sections carry different keys.
func (s *service) sectionOutput(sec db.Section, sectionLayouts []db.SectionLayout, collapsed map[string]bool) (map[string]any, *trpc.Error) {
	if sec.Kind == "empty" {
		if !sec.XOffset.Valid || !sec.YOffset.Valid {
			// emptySectionSchema requires both offsets; a null offset would
			// throw in upstream parseSection (an Error -> HTTP 500).
			return nil, trpc.NewError(trpc.CodeInternalServerError, "Section "+sec.ID+" is missing its canvas offsets")
		}
		return map[string]any{
			"id":      sec.ID,
			"kind":    "empty",
			"yOffset": sec.YOffset.Int64,
			"xOffset": sec.XOffset.Int64,
		}, nil
	}
	// "container" (and the legacy "dynamic" kind, transformed upstream by
	// legacyDynamicSectionSchema) carry options, collapsed state and layouts.
	options, terr := parseContainerSectionOptions(sec.Options)
	if terr != nil {
		return nil, terr
	}
	layouts := make([]map[string]any, 0)
	for _, l := range sectionLayouts {
		if l.SectionID != sec.ID {
			continue
		}
		layouts = append(layouts, map[string]any{
			"layoutId":        l.LayoutID,
			"yOffset":         l.YOffset,
			"xOffset":         l.XOffset,
			"width":           l.Width,
			"height":          l.Height,
			"parentSectionId": nullableStr(l.ParentSectionID),
		})
	}
	return map[string]any{
		"id":        sec.ID,
		"kind":      "container",
		"options":   options,
		"collapsed": collapsed[sec.ID],
		"layouts":   layouts,
	}, nil
}

// parseContainerSectionOptions unwraps the stored superjson options blob and
// applies containerSectionOptionsSchema (defaults for missing keys, unknown
// keys stripped).
func parseContainerSectionOptions(raw sql.NullString) (map[string]any, *trpc.Error) {
	payload := json.RawMessage(`{}`)
	if raw.Valid && raw.String != "" {
		unwrapped, terr := unwrapSuperJSON(json.RawMessage(raw.String), "section options")
		if terr != nil {
			return nil, trpc.NewError(trpc.CodeInternalServerError, "Invalid section options stored: "+terr.Message)
		}
		payload = unwrapped
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil || parsed == nil {
		// containerSectionOptionsSchema falls back to its default when the
		// stored value is not an object.
		parsed = map[string]any{}
	}
	out := make(map[string]any, len(containerSectionOptionsDefaults))
	for key, def := range containerSectionOptionsDefaults {
		if v, present := parsed[key]; present && v != nil {
			out[key] = v
		} else {
			out[key] = def
		}
	}
	return out, nil
}

// itemOutput is parseItem (board.ts:3041-3058, 3086-3094): items that fail
// the output union are skipped (ok=false).
func (s *service) itemOutput(item db.Item, itemLayouts []db.ItemLayout, integrationItems []db.IntegrationItem) (map[string]any, bool) {
	if !widgetKindSet[item.Kind] {
		return nil, false
	}
	optionsRaw, terr := unwrapSuperJSON(json.RawMessage(item.Options), "item options")
	if terr != nil {
		return nil, false
	}
	var options map[string]any
	if err := json.Unmarshal(optionsRaw, &options); err != nil || options == nil {
		// sharedItemSchema requires options to be a record.
		return nil, false
	}

	advancedRaw, terr := unwrapSuperJSON(json.RawMessage(item.AdvancedOptions), "item advanced options")
	if terr != nil {
		return nil, false
	}
	var advancedStored map[string]any
	if err := json.Unmarshal(advancedRaw, &advancedStored); err != nil || advancedStored == nil {
		return nil, false
	}
	// itemAdvancedOptionsSchema (packages/validation/src/shared.ts:16-20).
	advanced := map[string]any{"title": nil, "customCssClasses": []any{}, "borderColor": ""}
	if title, ok := advancedStored["title"]; ok && (title == nil || isString(title)) {
		advanced["title"] = title
	}
	if classes, ok := advancedStored["customCssClasses"].([]any); ok {
		advanced["customCssClasses"] = classes
	}
	if border, ok := advancedStored["borderColor"].(string); ok {
		advanced["borderColor"] = border
	}

	layoutEntries := make([]map[string]any, 0)
	for _, l := range itemLayouts {
		if l.ItemID != item.ID {
			continue
		}
		layoutEntries = append(layoutEntries, map[string]any{
			"layoutId":  l.LayoutID,
			"yOffset":   l.YOffset,
			"xOffset":   l.XOffset,
			"width":     l.Width,
			"height":    l.Height,
			"sectionId": l.SectionID,
		})
	}
	integrationIDs := make([]string, 0)
	for _, rel := range integrationItems {
		if rel.ItemID == item.ID {
			integrationIDs = append(integrationIDs, rel.IntegrationID)
		}
	}

	return map[string]any{
		"id":              item.ID,
		"kind":            item.Kind,
		"options":         options,
		"layouts":         layoutEntries,
		"integrationIds":  integrationIDs,
		"advancedOptions": advanced,
	}, true
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

// --- list procedures ---------------------------------------------------------

// boardGetPublicBoards is board.getPublicBoards (board.ts:280-289).
func (s *service) boardGetPublicBoards(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	type publicBoard struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		LogoImageUrl *string `json:"logoImageUrl"`
	}
	boards, err := s.db.ListPublicBoards(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	out := make([]publicBoard, 0, len(boards))
	for _, b := range boards {
		out = append(out, publicBoard{ID: b.ID, Name: b.Name, LogoImageUrl: nullableStr(b.LogoImageUrl)})
	}
	return out, nil
}

// boardGetAllBoards is board.getAllBoards (board.ts:321-374).
func (s *service) boardGetAllBoards(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	access, terr := s.getBoardAccessContext(ctx, c)
	if terr != nil {
		return nil, terr
	}

	userID := sessionUserID(c)

	boards, terr := s.listAccessibleBoardRows(ctx, c, access, "")
	if terr != nil {
		return nil, terr
	}

	// Caller-scoped permission rows for the summaries.
	directByBoard := map[string][]permissionEntry{}
	groupByBoard := map[string][]permissionEntry{}
	if userID != nil {
		direct, err := s.db.ListBoardUserPermissionsForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range direct {
			directByBoard[p.BoardID] = append(directByBoard[p.BoardID], permissionEntry{Permission: p.Permission})
		}
		groupIDs, err := s.db.ListGroupIDsForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		groupRows, err := s.db.ListBoardGroupPermissionsForGroups(ctx, groupIDs)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range groupRows {
			groupByBoard[p.BoardID] = append(groupByBoard[p.BoardID], permissionEntry{Permission: p.Permission})
		}
	}

	out := make([]boardSummaryOutput, 0, len(boards))
	for _, b := range boards {
		entry := boardSummaryOutput{
			ID:               b.ID,
			Name:             b.Name,
			LogoImageUrl:     nullableStr(b.LogoImageUrl),
			IsPublic:         b.IsPublic,
			UserPermissions:  directByBoard[b.ID],
			GroupPermissions: groupByBoard[b.ID],
		}
		if entry.UserPermissions == nil {
			entry.UserPermissions = []permissionEntry{}
		}
		if entry.GroupPermissions == nil {
			entry.GroupPermissions = []permissionEntry{}
		}
		if b.CreatorID != "" {
			creator, err := s.db.GetUserByID(ctx, b.CreatorID)
			if err == nil {
				entry.Creator = &creatorOutput{ID: creator.ID, Name: nullableStr(creator.Name), Image: nullableStr(creator.Image)}
			} else if err != db.ErrNotFound {
				return nil, internalError(err)
			}
		}
		if access.hasHomeBoards {
			entry.IsHome = access.homeBoards.HomeBoardID == b.ID
			entry.IsMobileHome = access.homeBoards.MobileHomeBoardID == b.ID
		}
		out = append(out, entry)
	}
	return out, nil
}

// sessionUserID returns the caller's user id, nil for anonymous callers.
func sessionUserID(c caller) *string {
	if c.session == nil {
		return nil
	}
	id := c.session.UserID
	return &id
}

// accessibleBoardRow is the summary row set of getAllBoards: id, name,
// logoImageUrl, isPublic and the creator id.
type accessibleBoardRow struct {
	ID           string
	Name         string
	LogoImageUrl string
	IsPublic     bool
	CreatorID    string
}

// listAccessibleBoardRows loads the boards visible to the caller with the
// getAccessibleBoardsWhere filter (board.ts:2932-2939). nameLike optionally
// restricts to a LIKE pattern (catalog/search).
func (s *service) listAccessibleBoardRows(ctx context.Context, c caller, access accessContext, nameLike string) ([]accessibleBoardRow, *trpc.Error) {
	query := `SELECT b.id, b.name, b.logo_image_url, b.is_public, b.creator_id FROM "board" b`
	var args []any
	if !c.canViewAll() {
		// or(isPublic, eq(creatorId, userId), inArray(id, boardIds)); an empty
		// boardIds array contributes no condition (board.ts:2938).
		if c.session != nil {
			args = append(args, c.session.UserID)
		} else {
			args = append(args, "")
		}
		if len(access.boardIDs) > 0 {
			for _, id := range access.boardIDs {
				args = append(args, id)
			}
			query += ` WHERE (b.is_public = 1 OR b.creator_id = ? OR b.id IN (` +
				repeatPlaceholders(len(access.boardIDs)) + `))`
		} else {
			query += ` WHERE (b.is_public = 1 OR b.creator_id = ?)`
		}
	}
	if nameLike != "" {
		if len(args) == 0 && c.canViewAll() {
			query += ` WHERE`
		} else {
			query += ` AND`
		}
		query += ` b.name LIKE ?`
		args = append(args, nameLike)
	}
	query += ` ORDER BY b.name`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	out := []accessibleBoardRow{}
	for rows.Next() {
		var (
			r    accessibleBoardRow
			logo sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Name, &logo, &r.IsPublic, &r.CreatorID); err != nil {
			return nil, internalError(err)
		}
		r.LogoImageUrl = logo.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}
	return out, nil
}

// searchAccessibleBoards is searchAccessibleBoardsAsync (board.ts:221-263),
// shared by board.catalog (query "") and board.search.
func (s *service) searchAccessibleBoards(ctx context.Context, c caller, query string, limit int64) (any, *trpc.Error) {
	access, terr := s.getBoardAccessContext(ctx, c)
	if terr != nil {
		return nil, terr
	}

	userID := sessionUserID(c)

	boards, terr := s.listAccessibleBoardRows(ctx, c, access, "%"+query+"%")
	if terr != nil {
		return nil, terr
	}
	if limit > 0 && len(boards) > int(limit) {
		boards = boards[:limit]
	}

	// Caller-scoped permission rows to build constructBoardPermissions.
	directByBoard := map[string][]string{}
	groupByBoard := map[string][]string{}
	if userID != nil {
		direct, err := s.db.ListBoardUserPermissionsForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range direct {
			directByBoard[p.BoardID] = append(directByBoard[p.BoardID], p.Permission)
		}
		groupIDs, err := s.db.ListGroupIDsForUser(ctx, *userID)
		if err != nil {
			return nil, internalError(err)
		}
		groupRows, err := s.db.ListBoardGroupPermissionsForGroups(ctx, groupIDs)
		if err != nil {
			return nil, internalError(err)
		}
		for _, p := range groupRows {
			groupByBoard[p.BoardID] = append(groupByBoard[p.BoardID], p.Permission)
		}
	}

	out := make([]boardSearchOutput, 0, len(boards))
	for _, b := range boards {
		entry := boardSearchOutput{
			ID:           b.ID,
			Name:         b.Name,
			LogoImageUrl: nullableStr(b.LogoImageUrl),
			Permissions: boardPermissionsOutput{
				HasFullAccess:   searchPermCheck(b.CreatorID, directByBoard[b.ID], groupByBoard[b.ID], c, "full"),
				HasChangeAccess: searchPermCheck(b.CreatorID, directByBoard[b.ID], groupByBoard[b.ID], c, "change"),
				HasViewAccess:   searchPermCheck(b.CreatorID, directByBoard[b.ID], groupByBoard[b.ID], c, "view"),
			},
		}
		if access.hasHomeBoards {
			entry.IsHome = access.homeBoards.HomeBoardID == b.ID
			entry.IsMobileHome = access.homeBoards.MobileHomeBoardID == b.ID
		}
		out = append(out, entry)
	}
	return out, nil
}

// searchPermCheck evaluates one access level of constructBoardPermissions for
// the catalog/search rows (the board object carries creatorId there, unlike
// getAllBoards where the creator relation is loaded).
func searchPermCheck(creatorID string, userPerms, groupPerms []string, c caller, level string) bool {
	isCreator := c.session != nil && c.session.UserID == creatorID
	switch level {
	case "full":
		if isCreator {
			return true
		}
		if containsString(userPerms, "full") || containsString(groupPerms, "full") {
			return true
		}
		return c.hasPermission("board-full-all")
	case "change":
		if isCreator {
			return true
		}
		if containsString(userPerms, "modify") || containsString(userPerms, "full") ||
			containsString(groupPerms, "modify") || containsString(groupPerms, "full") {
			return true
		}
		return c.hasPermission("board-modify-all") || c.hasPermission("board-full-all")
	default: // view
		if isCreator {
			return true
		}
		if len(userPerms) >= 1 || len(groupPerms) >= 1 {
			return true
		}
		return c.hasPermission("board-view-all")
	}
}

// boardCatalog is board.catalog (board.ts:640-642): every accessible board.
func (s *service) boardCatalog(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	return s.searchAccessibleBoards(ctx, c, "", 0)
}

// boardSearch is board.search (board.ts:643-651).
func (s *service) boardSearch(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := dec.str("query")
	if terr != nil {
		return nil, terr
	}
	// z.number().min(1).max(100).default(10).
	limit := int64(10)
	if dec.has("limit") {
		limit, terr = dec.int64("limit", 1, 100)
		if terr != nil {
			return nil, terr
		}
	}
	return s.searchAccessibleBoards(ctx, c, query, limit)
}

// boardGetBoardsForGroup is board.getBoardsForGroup (board.ts:290-320).
func (s *service) boardGetBoardsForGroup(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	groupID, terr := dec.str("groupId")
	if terr != nil {
		return nil, terr
	}

	// everyone + the requested group (board.ts:294-300).
	groupIDs := []string{groupID}
	everyone, err := s.db.GetGroupByName(ctx, "everyone")
	if err == nil {
		groupIDs = append(groupIDs, everyone.ID)
	} else if err != db.ErrNotFound {
		return nil, internalError(err)
	}

	groupPerms, err := s.db.ListGroupPermissionsForGroups(ctx, groupIDs)
	if err != nil {
		return nil, internalError(err)
	}
	keys := make([]string, 0, len(groupPerms))
	for _, p := range groupPerms {
		keys = append(keys, p.Permission)
	}
	canViewAll := expandPermissions(keys)["board-view-all"]

	boardIDs, err := s.db.ListBoardIDsForGroups(ctx, groupIDs)
	if err != nil {
		return nil, internalError(err)
	}

	type groupBoard struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		LogoImageUrl *string `json:"logoImageUrl"`
	}
	out := []groupBoard{}
	if canViewAll {
		boards, err := s.db.ListAllBoardsMinimal(ctx)
		if err != nil {
			return nil, internalError(err)
		}
		for _, b := range boards {
			out = append(out, groupBoard{ID: b.ID, Name: b.Name, LogoImageUrl: nullableStr(b.LogoImageUrl)})
		}
		return out, nil
	}

	// or(eq(boards.isPublic, true), inArray(boards.id, boardIds))
	query := `SELECT id, name, logo_image_url FROM "board" WHERE is_public = 1`
	var args []any
	if len(boardIDs) > 0 {
		query += ` OR id IN (` + repeatPlaceholders(len(boardIDs)) + `)`
		for _, id := range boardIDs {
			args = append(args, id)
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, internalError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			b    groupBoard
			logo sql.NullString
		)
		if err := rows.Scan(&b.ID, &b.Name, &logo); err != nil {
			return nil, internalError(err)
		}
		b.LogoImageUrl = nullableStr(logo.String)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError(err)
	}
	return out, nil
}

// boardGetHomeBoard is board.getHomeBoard (board.ts:1172-1194).
func (s *service) boardGetHomeBoard(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}

	userID := sessionUserID(c)

	homeBoardID, terr := s.homeBoardID(ctx, c, userID, s.opts.deviceType(ctx))
	if terr != nil {
		return nil, terr
	}
	if homeBoardID == "" {
		return nil, trpc.NewError(trpc.CodeNotFound, "No home board found")
	}
	if terr := s.requireBoardAccess(ctx, homeBoardID, c, permView); terr != nil {
		return nil, terr
	}
	return s.getFullBoard(ctx, homeBoardID, userID)
}

// homeBoardID is getHomeIdBoardAsync (board.ts:2500-2546): the user's device
// home board, the user's desktop home board, the lowest positioned group,
// the everyone group and finally the server settings.
func (s *service) homeBoardID(ctx context.Context, c caller, userID *string, deviceType string) (string, *trpc.Error) {
	mobile := deviceType == "mobile"

	settings, err := s.db.GetBoardServerSettings(ctx)
	if err != nil {
		return "", internalError(err)
	}

	// The device-specific setting key (board.ts:2505): homeBoardId or
	// mobileHomeBoardId.
	if userID == nil {
		// Anonymous callers start at the server settings chain.
		if mobile && settings.MobileHomeBoardID != "" {
			return settings.MobileHomeBoardID, nil
		}
		return settings.HomeBoardID, nil
	}

	homeBoards, err := s.db.GetUserHomeBoards(ctx, *userID)
	if err == db.ErrNotFound {
		// The session user no longer exists: upstream finds no user row and
		// falls through to the group/settings chain with user=null semantics.
		if mobile && settings.MobileHomeBoardID != "" {
			return settings.MobileHomeBoardID, nil
		}
		return settings.HomeBoardID, nil
	}
	if err != nil {
		return "", internalError(err)
	}

	// user[settingKey] (board.ts:2512) — for mobile this is the mobile home
	// board, for desktop the home board itself.
	if mobile && homeBoards.MobileHomeBoardID != "" {
		return homeBoards.MobileHomeBoardID, nil
	}
	if homeBoards.HomeBoardID != "" {
		return homeBoards.HomeBoardID, nil
	}

	// The lowest positioned group (except everyone) with both home columns
	// set (board.ts:2515-2535).
	settingColumn := "home_board_id"
	if mobile {
		settingColumn = "mobile_home_board_id"
	}
	lowest, err := s.db.LowestGroupHomeBoardForUser(ctx, *userID, settingColumn)
	if err == nil {
		if mobile && lowest.MobileHomeBoardID != "" {
			return lowest.MobileHomeBoardID, nil
		}
		if lowest.HomeBoardID != "" {
			return lowest.HomeBoardID, nil
		}
	} else if err != db.ErrNotFound {
		return "", internalError(err)
	}

	// The everyone group (board.ts:2537-2542).
	everyone, err := s.db.GetGroupByName(ctx, "everyone")
	if err == nil {
		if mobile && everyone.MobileHomeBoardID != "" {
			return everyone.MobileHomeBoardID, nil
		}
		if everyone.HomeBoardID != "" {
			return everyone.HomeBoardID, nil
		}
	} else if err != db.ErrNotFound {
		return "", internalError(err)
	}

	// Server settings fallback (board.ts:2544-2545): boardSettings[settingKey]
	// ?? boardSettings.homeBoardId.
	if mobile && settings.MobileHomeBoardID != "" {
		return settings.MobileHomeBoardID, nil
	}
	return settings.HomeBoardID, nil
}

// repeatPlaceholders builds a "?,?,..." fragment.
func repeatPlaceholders(n int) string {
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}
