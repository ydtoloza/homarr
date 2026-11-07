package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Mutation procedures of the board router that only touch the board row and
// the user table. The content mutations (saveBoard / addItem /
// updateItemLayout) live in content.go.

// boardExists is board.exists (board.ts:266-279): true when a board with a
// similar name exists. The CONFLICT branch of noBoardWithSimilarNameAsync is
// converted into the boolean response.
func (s *service) boardExists(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "board-create"); terr != nil {
		return nil, terr
	}
	// input(z.string()): the input is the name itself, not an object, and
	// carries no boardNameSchema validation upstream.
	name, terr := decodeStringInput(input)
	if terr != nil {
		return nil, terr
	}
	taken, err := s.db.BoardNameTaken(ctx, name, nil)
	if err != nil {
		return nil, internalError(err)
	}
	return taken, nil
}

// decodeStringInput parses the z.string() inputs of board.exists.
func decodeStringInput(input json.RawMessage) (string, *trpc.Error) {
	raw, terr := unwrapSuperJSON(input, "input")
	if terr != nil {
		return "", terr
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", badRequestf("expected a string input")
	}
	return name, nil
}

// boardCreateBoard is board.createBoard (board.ts:652-730): inserts the
// board, its root section and the Mobile + Base layouts, and selects the
// creator's home board when unset.
func (s *service) boardCreateBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "board-create"); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	name, terr := dec.str("name")
	if terr != nil {
		return nil, terr
	}
	if !isBoardName(name) {
		return nil, badRequestf("invalid board name")
	}
	columnCount, terr := dec.int64("columnCount", 1, 24)
	if terr != nil {
		return nil, terr
	}
	isPublic, terr := dec.bool("isPublic")
	if terr != nil {
		return nil, terr
	}

	// The caller always exists here: upstream reads ctx.session.user.id.
	boardID := createID()
	baseLayoutID := createID()
	mobileLayoutID := createID()
	rootSectionID := createID()

	newBoard := db.Board{ID: boardID, Name: name, IsPublic: isPublic, CreatorID: c.session.UserID}
	err := s.db.CreateBoard(ctx, newBoard)
	if err != nil {
		return nil, internalError(err)
	}
	err = s.db.InsertSection(ctx, db.Section{
		ID:      rootSectionID,
		BoardID: boardID,
		Kind:    "empty",
		XOffset: nullInt64(0),
		YOffset: nullInt64(0),
		Options: sql.NullString{String: `{"json": {}}`, Valid: true},
	})
	if err != nil {
		return nil, internalError(err)
	}
	err = s.db.InsertLayout(ctx, db.Layout{
		ID: mobileLayoutID, Name: "Mobile", BoardID: boardID,
		ColumnCount: 3, Breakpoint: 0, Role: "mobile",
	})
	if err != nil {
		return nil, internalError(err)
	}
	err = s.db.InsertLayout(ctx, db.Layout{
		ID: baseLayoutID, Name: "Base", BoardID: boardID,
		ColumnCount: columnCount, Breakpoint: 768, Role: "base",
	})
	if err != nil {
		return nil, internalError(err)
	}

	// The first board becomes the creator's home board
	// (board.ts:725-727: if (!user?.homeBoardId)).
	homeBoards, err := s.db.GetUserHomeBoards(ctx, c.session.UserID)
	if err == nil && homeBoards.HomeBoardID == "" {
		if err := s.db.SetUserHomeBoard(ctx, c.session.UserID, boardID); err != nil {
			return nil, internalError(err)
		}
	} else if err != nil && err != db.ErrNotFound {
		return nil, internalError(err)
	}

	return map[string]any{
		"boardId":  boardID,
		"name":     name,
		"layoutId": baseLayoutID,
	}, nil
}

// boardRenameBoard is board.renameBoard (board.ts:1039-1063).
func (s *service) boardRenameBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	name, terr := dec.str("name")
	if terr != nil {
		return nil, terr
	}
	if !isBoardName(name) {
		return nil, badRequestf("invalid board name")
	}
	if terr := s.requireBoardAccess(ctx, id, c, permFull); terr != nil {
		return nil, terr
	}
	taken, err := s.db.BoardNameTaken(ctx, name, []string{id})
	if err != nil {
		return nil, internalError(err)
	}
	if taken {
		return nil, trpc.NewError(trpc.CodeConflict, "Board with similar name already exists")
	}
	if err := s.db.UpdateBoardName(ctx, id, name); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// boardChangeBoardVisibility is board.changeBoardVisibility
// (board.ts:1064-1101).
func (s *service) boardChangeBoardVisibility(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	visibility, terr := dec.enum("visibility", []string{"public", "private"})
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permFull); terr != nil {
		return nil, terr
	}
	settings, err := s.db.GetBoardServerSettings(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	if visibility != "public" && (settings.HomeBoardID == id || settings.MobileHomeBoardID == id) {
		return nil, badRequestf("Cannot make home board private")
	}
	if err := s.db.UpdateBoardVisibility(ctx, id, visibility == "public"); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// boardDeleteBoard is board.deleteBoard (board.ts:1102-1125).
func (s *service) boardDeleteBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permFull); terr != nil {
		return nil, terr
	}
	if err := s.db.DeleteBoardByID(ctx, id); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// boardSetHomeBoard is board.setHomeBoard (board.ts:1126-1148).
func (s *service) boardSetHomeBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.setHomeBoardColumn(ctx, input, false)
}

// boardSetMobileHomeBoard is board.setMobileHomeBoard (board.ts:1149-1171).
func (s *service) boardSetMobileHomeBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.setHomeBoardColumn(ctx, input, true)
}

func (s *service) setHomeBoardColumn(ctx context.Context, input json.RawMessage, mobile bool) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permView); terr != nil {
		return nil, terr
	}
	var err error
	if mobile {
		err = s.db.SetUserMobileHomeBoard(ctx, c.session.UserID, id)
	} else {
		err = s.db.SetUserHomeBoard(ctx, c.session.UserID, id)
	}
	if err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// boardGetBoardByName is board.getBoardByName (board.ts:1195-1209).
func (s *service) boardGetBoardByName(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	name, terr := dec.str("name")
	if terr != nil {
		return nil, terr
	}
	if !isBoardName(name) {
		return nil, badRequestf("invalid board name")
	}
	boardID, err := s.db.GetBoardIDByNameInsensitive(ctx, name)
	if err != nil {
		return nil, notFoundBoard()
	}
	if terr := s.requireBoardAccess(ctx, boardID, c, permView); terr != nil {
		return nil, terr
	}
	return s.getFullBoard(ctx, boardID, sessionUserID(c))
}

// boardGetBoardSettings is board.getBoardSettings (board.ts:1210-1268): the
// editable settings, with nullable columns rendered as "" like upstream.
func (s *service) boardGetBoardSettings(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permModify); terr != nil {
		return nil, terr
	}
	board, err := s.db.GetBoardFullByID(ctx, id)
	if err != nil {
		return nil, notFoundBoard()
	}
	// boardSettingsSchema (packages/validation/src/board.ts:78-81): every
	// nullable string is replaced by "".
	return map[string]any{
		"id":                        board.ID,
		"name":                      board.Name,
		"pageTitle":                 board.PageTitle,
		"metaTitle":                 board.MetaTitle,
		"logoImageUrl":              board.LogoImageUrl,
		"faviconImageUrl":           board.FaviconImageUrl,
		"backgroundImageUrl":        board.BackgroundImageUrl,
		"backgroundImageAttachment": board.BackgroundImageAttachment,
		"backgroundImageRepeat":     board.BackgroundImageRepeat,
		"backgroundImageSize":       board.BackgroundImageSize,
		"primaryColor":              board.PrimaryColor,
		"secondaryColor":            board.SecondaryColor,
		"opacity":                   int64(board.Opacity),
		"customCss":                 board.CustomCss,
		"iconColor":                 board.IconColor,
		"itemRadius":                board.ItemRadius,
		"disableStatus":             board.DisableStatus,
	}, nil
}

// boardSavePartialBoardSettings is board.savePartialBoardSettings
// (board.ts:1636-1687): only the provided keys change.
func (s *service) boardSavePartialBoardSettings(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permModify); terr != nil {
		return nil, terr
	}

	settings := db.BoardPartialSettings{}
	// trimmedNullableString fields: string or null, "" becomes null.
	for _, key := range []struct {
		name   string
		target **string
	}{
		{"pageTitle", &settings.PageTitle},
		{"metaTitle", &settings.MetaTitle},
		{"logoImageUrl", &settings.LogoImageUrl},
		{"faviconImageUrl", &settings.FaviconImageUrl},
		{"backgroundImageUrl", &settings.BackgroundImageUrl},
	} {
		if !dec.has(key.name) {
			continue
		}
		value, ok, terr := dec.optStr(key.name)
		if terr != nil {
			return nil, terr
		}
		if ok {
			trimmed := trimSpace(value)
			*key.target = &trimmed
		} else {
			empty := ""
			*key.target = &empty
		}
	}

	optionalEnum := func(key string, allowed []string, target **string) *trpc.Error {
		if !dec.has(key) {
			return nil
		}
		v, _, terr := dec.optStr(key)
		if terr != nil {
			return terr
		}
		for _, a := range allowed {
			if a == v {
				*target = &v
				return nil
			}
		}
		return badRequestf(`input key "%s" has an invalid value`, key)
	}
	if terr := optionalEnum("backgroundImageAttachment", backgroundImageAttachments, &settings.BackgroundImageAttachment); terr != nil {
		return nil, terr
	}
	if terr := optionalEnum("backgroundImageRepeat", backgroundImageRepeats, &settings.BackgroundImageRepeat); terr != nil {
		return nil, terr
	}
	if terr := optionalEnum("backgroundImageSize", backgroundImageSizes, &settings.BackgroundImageSize); terr != nil {
		return nil, terr
	}
	if terr := optionalEnum("itemRadius", cornerStyleValues, &settings.ItemRadius); terr != nil {
		return nil, terr
	}

	// hexColorSchema fields: present implies #RRGGBB.
	for _, key := range []struct {
		name   string
		target **string
	}{
		{"primaryColor", &settings.PrimaryColor},
		{"secondaryColor", &settings.SecondaryColor},
	} {
		if !dec.has(key.name) {
			continue
		}
		v, _, terr := dec.optStr(key.name)
		if terr != nil {
			return nil, terr
		}
		if !isHexColor(v) {
			return nil, badRequestf(`input key "%s" must be a #RRGGBB color`, key.name)
		}
		*key.target = &v
	}
	// iconColor: hexColorNullableSchema — hex color, "" (null) or null.
	if dec.has("iconColor") {
		if value, ok, terr := dec.optStr("iconColor"); terr != nil {
			return nil, terr
		} else if ok {
			if value != "" && !isHexColor(value) {
				return nil, badRequestf(`input key "iconColor" must be a #RRGGBB color`)
			}
			settings.IconColor = &value
		} else {
			empty := ""
			settings.IconColor = &empty
		}
	}
	// opacity: z.number().min(0).max(100).
	if dec.has("opacity") {
		if n, ok := numberToFloat64(dec.obj["opacity"]); ok && n >= 0 && n <= 100 {
			opacity := int(n)
			settings.Opacity = &opacity
		} else {
			return nil, badRequestf(`input key "opacity" must be a number between 0 and 100`)
		}
	}
	// customCss: z.string().max(16384).
	if dec.has("customCss") {
		v, _, terr := dec.optStr("customCss")
		if terr != nil {
			return nil, terr
		}
		if len(v) > 16384 {
			return nil, badRequestf(`input key "customCss" is too long`)
		}
		settings.CustomCss = &v
	}
	// disableStatus: boolean.
	if dec.has("disableStatus") {
		v, _, terr := dec.optBool("disableStatus")
		if terr != nil {
			return nil, terr
		}
		settings.DisableStatus = &v
	}

	if err := s.db.UpdateBoardPartialSettings(ctx, id, settings); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

func trimSpace(v string) string {
	start, end := 0, len(v)
	for start < end && isSpaceByte(v[start]) {
		start++
	}
	for end > start && isSpaceByte(v[end-1]) {
		end--
	}
	return v[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// nullInt64 builds a non-null sql.NullInt64.
func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}

// --- layouts (board.saveLayouts / board.resetLayout) ----------------------------

// layoutInput is one entry of boardSaveLayoutsSchema.layouts
// (packages/validation/src/board.ts:83-134).
type layoutInput struct {
	ID                     string
	Name                   string
	ColumnCount            int64
	LeftGutterColumnCount  int64
	RightGutterColumnCount int64
	Breakpoint             int64
	Role                   string
}

// parseLayoutsInput validates the responsiveBoardLayoutsSchema array
// (packages/validation/src/board.ts:83-134). columnCount accepts integers
// only (BOARDS.md decision 1: upstream z.number() would accept floats, which
// would break the int64 grid scans).
func parseLayoutsInput(dec *inputDecoder) ([]layoutInput, *trpc.Error) {
	layoutsRaw, ok := dec.obj["layouts"]
	if !ok || layoutsRaw == nil {
		return nil, badRequestf(`expected a layouts array`)
	}
	list, ok := layoutsRaw.([]any)
	if !ok {
		return nil, badRequestf("layouts must be an array")
	}
	if len(list) < 2 {
		// responsiveBoardLayoutsSchema .min(2).
		return nil, badRequestf("Boards require exactly one Mobile and one Base layout")
	}
	out := make([]layoutInput, 0, len(list))
	for _, element := range list {
		entry, ok := element.(map[string]any)
		if !ok {
			return nil, badRequestf("invalid layout entry")
		}
		layDec := &inputDecoder{obj: entry}
		layout, terr := parseLayoutEntry(layDec)
		if terr != nil {
			return nil, terr
		}
		out = append(out, layout)
	}

	// superRefine (packages/validation/src/board.ts:108-129): exactly one
	// Mobile and one Base layout, Mobile breakpoint 0, unique breakpoints
	// and unique ids.
	mobiles, bases := 0, 0
	var mobileLayout *layoutInput
	for i := range out {
		switch out[i].Role {
		case "mobile":
			mobiles++
			mobileLayout = &out[i]
		case "base":
			bases++
		}
	}
	if mobiles != 1 || bases != 1 || mobileLayout == nil {
		return nil, badRequestf("Boards require exactly one Mobile and one Base layout")
	}
	if mobileLayout.Breakpoint != 0 {
		return nil, badRequestf("The Mobile layout breakpoint must be 0")
	}
	breakpoints := map[int64]bool{}
	ids := map[string]bool{}
	for _, layout := range out {
		if breakpoints[layout.Breakpoint] {
			return nil, badRequestf("Layout breakpoints must be unique")
		}
		breakpoints[layout.Breakpoint] = true
		if ids[layout.ID] {
			return nil, badRequestf("Layout IDs must be unique")
		}
		ids[layout.ID] = true
	}
	return out, nil
}

// parseLayoutEntry validates one boardLayoutSchema entry
// (packages/validation/src/board.ts:83-103).
func parseLayoutEntry(dec *inputDecoder) (layoutInput, *trpc.Error) {
	id, terr := dec.str("id")
	if terr != nil {
		return layoutInput{}, terr
	}
	name, terr := dec.str("name")
	if terr != nil {
		return layoutInput{}, terr
	}
	// z.string().trim().nonempty().max(32): the trimmed value is stored.
	name = trimSpace(name)
	if len([]rune(name)) < 1 || len([]rune(name)) > 32 {
		return layoutInput{}, badRequestf("layout name must be 1-32 characters")
	}
	columnCount, terr := dec.int64("columnCount", 1, 24)
	if terr != nil {
		return layoutInput{}, terr
	}
	// boardGutterColumnCountSchema.default(0).
	leftGutter, _, terr := dec.optInt64("leftGutterColumnCount", 0, 3)
	if terr != nil {
		return layoutInput{}, terr
	}
	rightGutter, _, terr := dec.optInt64("rightGutterColumnCount", 0, 3)
	if terr != nil {
		return layoutInput{}, terr
	}
	breakpoint, terr := dec.int64("breakpoint", 0, 32767)
	if terr != nil {
		return layoutInput{}, terr
	}
	role, terr := dec.enum("role", layoutRoles)
	if terr != nil {
		return layoutInput{}, terr
	}
	if leftGutter+rightGutter >= columnCount {
		return layoutInput{}, badRequestf("Gutters must leave at least one dashboard column")
	}
	if role == "mobile" && (leftGutter != 0 || rightGutter != 0) {
		return layoutInput{}, badRequestf("Mobile layouts cannot have sidebars")
	}
	return layoutInput{
		ID: id, Name: name, ColumnCount: columnCount,
		LeftGutterColumnCount: leftGutter, RightGutterColumnCount: rightGutter,
		Breakpoint: breakpoint, Role: role,
	}, nil
}

// boardSaveLayouts is board.saveLayouts (board.ts:1269-1554): replaces the
// board's layouts array, resizing existing layouts through the responsive
// grid projection and adding custom layouts projected from the Base layout.
func (s *service) boardSaveLayouts(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	layoutInputs, terr := parseLayoutsInput(dec)
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, id, c, permModify); terr != nil {
		return nil, terr
	}

	// ensureGutterRootSectionsAsync (board.ts:1290-1293): lanes that any
	// submitted layout uses get their canvas root before the projection reads
	// the board.
	var requiredGutterRoots []string
	for _, lane := range []string{laneLeft, laneRight} {
		for _, layout := range layoutInputs {
			geometry := layoutGeometry{
				role:                   layout.Role,
				columnCount:            layout.ColumnCount,
				leftGutterColumnCount:  layout.LeftGutterColumnCount,
				rightGutterColumnCount: layout.RightGutterColumnCount,
			}
			if getBoardLaneColumnCount(geometry, lane) > 0 {
				requiredGutterRoots = append(requiredGutterRoots, lane)
				break
			}
		}
	}
	if terr := s.ensureGutterRootSections(ctx, id, requiredGutterRoots); terr != nil {
		return nil, terr
	}

	board, terr := s.loadBoardForProjection(ctx, id)
	if terr != nil {
		return nil, terr
	}
	existingLayoutsByID := map[string]db.Layout{}
	for _, layout := range board.Layouts {
		existingLayoutsByID[layout.ID] = layout
	}
	submittedLayoutIDs := map[string]bool{}
	for _, layout := range layoutInputs {
		submittedLayoutIDs[layout.ID] = true
	}

	var addedLayouts []layoutInput
	for _, layout := range layoutInputs {
		if _, exists := existingLayoutsByID[layout.ID]; !exists {
			addedLayouts = append(addedLayouts, layout)
		}
	}
	var removedLayouts []db.Layout
	for _, layout := range board.Layouts {
		if !submittedLayoutIDs[layout.ID] {
			removedLayouts = append(removedLayouts, layout)
		}
	}

	// Role invariants (board.ts:1301-1314).
	for _, layout := range addedLayouts {
		if layout.Role != "custom" {
			return nil, badRequestf("New layouts must use the custom role")
		}
	}
	for _, layout := range removedLayouts {
		if layout.Role != "custom" {
			return nil, badRequestf("Mobile and Base layouts cannot be removed")
		}
	}
	for _, layout := range layoutInputs {
		if existing, ok := existingLayoutsByID[layout.ID]; ok && existing.Role != layout.Role {
			return nil, badRequestf("Layout roles cannot be changed")
		}
	}

	baseLayout, found := db.Layout{}, false
	for _, layout := range board.Layouts {
		if layout.Role == "base" {
			baseLayout, found = layout, true
			break
		}
	}
	if !found {
		return nil, trpc.NewError(trpc.CodeInternalServerError, "Board must have a Base layout")
	}

	// The Base resize is computed once and reused: added custom layouts
	// project from the resized arrangement and the Base update reuses the
	// same projection (board.ts:1325-1418).
	requestedBase, requestedBaseFound := layoutInput{}, false
	for _, layout := range layoutInputs {
		if layout.ID == baseLayout.ID {
			requestedBase, requestedBaseFound = layout, true
			break
		}
	}
	var resizedBase *updatedBoardLayout
	if requestedBaseFound &&
		(requestedBase.ColumnCount != baseLayout.ColumnCount ||
			requestedBase.LeftGutterColumnCount != baseLayout.LeftGutterColumnCount ||
			requestedBase.RightGutterColumnCount != baseLayout.RightGutterColumnCount) {
		updated, err := getUpdatedBoardLayout(board, updatedLayoutOptions{
			PreviousGeometry: layoutGeometryValueOf(baseLayout),
			CurrentGeometry:  layoutGeometryValueOfInput(requestedBase),
		})
		if err != nil {
			return nil, internalError(err)
		}
		resizedBase = &updated
	}
	var baseSourceElements []gridItem
	if resizedBase != nil {
		baseSourceElements = getElementsForProjectedLayout(*resizedBase)
	}
	baseSourceGeometry := layoutGeometryValue{
		LayoutID:               baseLayout.ID,
		ColumnCount:            baseLayout.ColumnCount,
		LeftGutterColumnCount:  baseLayout.LeftGutterColumnCount,
		RightGutterColumnCount: baseLayout.RightGutterColumnCount,
	}
	if requestedBaseFound {
		baseSourceGeometry.ColumnCount = requestedBase.ColumnCount
		baseSourceGeometry.LeftGutterColumnCount = requestedBase.LeftGutterColumnCount
		baseSourceGeometry.RightGutterColumnCount = requestedBase.RightGutterColumnCount
	}

	// Added layouts (board.ts:1354-1382): inserted with role "custom" and
	// content projected from the (possibly resized) Base geometry.
	var layoutsToInsert []db.Layout
	var itemLayoutsToInsert []projectedItemLayout
	var sectionLayoutsToInsert []projectedSectionLayout
	savedLayoutIDs := map[string]string{}
	for _, addedLayout := range addedLayouts {
		layoutID := createID()
		savedLayoutIDs[addedLayout.ID] = layoutID
		layoutsToInsert = append(layoutsToInsert, db.Layout{
			ID: layoutID, Name: addedLayout.Name, BoardID: board.ID,
			ColumnCount:            addedLayout.ColumnCount,
			LeftGutterColumnCount:  addedLayout.LeftGutterColumnCount,
			RightGutterColumnCount: addedLayout.RightGutterColumnCount,
			Breakpoint:             addedLayout.Breakpoint, Role: "custom",
		})
		projected, err := getUpdatedBoardLayout(board, updatedLayoutOptions{
			PreviousGeometry: baseSourceGeometry,
			PreviousElements: baseSourceElements,
			CurrentGeometry: layoutGeometryValue{
				LayoutID:               layoutID,
				ColumnCount:            addedLayout.ColumnCount,
				LeftGutterColumnCount:  addedLayout.LeftGutterColumnCount,
				RightGutterColumnCount: addedLayout.RightGutterColumnCount,
			},
		})
		if err != nil {
			return nil, internalError(err)
		}
		itemLayoutsToInsert = append(itemLayoutsToInsert, projected.ItemSectionLayouts...)
		sectionLayoutsToInsert = append(sectionLayoutsToInsert, projected.SectionLayouts...)
	}

	// Resized layouts (board.ts:1384-1419): unchanged geometries keep their
	// placements untouched (and their stored resize source).
	var itemLayoutsToUpdate []projectedItemLayout
	var sectionLayoutsToUpdate []projectedSectionLayout
	resizeSourcesToUpdate := map[string]string{}
	for _, updatedLayout := range layoutInputs {
		dbLayout, ok := existingLayoutsByID[updatedLayout.ID]
		if !ok || (dbLayout.ColumnCount == updatedLayout.ColumnCount &&
			dbLayout.LeftGutterColumnCount == updatedLayout.LeftGutterColumnCount &&
			dbLayout.RightGutterColumnCount == updatedLayout.RightGutterColumnCount) {
			continue
		}
		projected := *resizedBase
		if !(updatedLayout.Role == "base" && resizedBase != nil) {
			result, err := getUpdatedBoardLayout(board, updatedLayoutOptions{
				PreviousGeometry: layoutGeometryValueOf(dbLayout),
				CurrentGeometry:  layoutGeometryValueOfInput(updatedLayout),
			})
			if err != nil {
				return nil, internalError(err)
			}
			projected = result
		}
		resizeSourcesToUpdate[updatedLayout.ID] = projected.ResizeSource
		itemLayoutsToUpdate = append(itemLayoutsToUpdate, projected.ItemSectionLayouts...)
		sectionLayoutsToUpdate = append(sectionLayoutsToUpdate, projected.SectionLayouts...)
	}

	removedLayoutIDs := make([]string, 0, len(removedLayouts))
	for _, layout := range removedLayouts {
		removedLayoutIDs = append(removedLayoutIDs, layout.ID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalError(err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, layout := range layoutsToInsert {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "layout" (id, name, board_id, column_count,
			left_gutter_column_count, right_gutter_column_count, breakpoint, role, resize_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			layout.ID, layout.Name, layout.BoardID, layout.ColumnCount,
			layout.LeftGutterColumnCount, layout.RightGutterColumnCount,
			layout.Breakpoint, layout.Role); err != nil {
			return nil, internalError(err)
		}
	}
	for _, row := range itemLayoutsToInsert {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.ItemID, row.SectionID, row.LayoutID, row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
			return nil, internalError(err)
		}
	}
	for _, row := range sectionLayoutsToInsert {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.SectionID, row.LayoutID, nullableStr(row.ParentSectionID),
			row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
			return nil, internalError(err)
		}
	}
	for _, row := range itemLayoutsToUpdate {
		if _, err := tx.ExecContext(ctx, `UPDATE "item_layout" SET height = ?, width = ?, x_offset = ?,
			y_offset = ?, section_id = ? WHERE item_id = ? AND layout_id = ?`,
			row.Height, row.Width, row.XOffset, row.YOffset, row.SectionID, row.ItemID, row.LayoutID); err != nil {
			return nil, internalError(err)
		}
	}
	for _, row := range sectionLayoutsToUpdate {
		if _, err := tx.ExecContext(ctx, `UPDATE "section_layout" SET height = ?, width = ?, x_offset = ?,
			y_offset = ?, parent_section_id = ? WHERE section_id = ? AND layout_id = ?`,
			row.Height, row.Width, row.XOffset, row.YOffset, nullableStr(row.ParentSectionID),
			row.SectionID, row.LayoutID); err != nil {
			return nil, internalError(err)
		}
	}
	// getResizeSourceUpdate (board.ts:2568-2571): only layouts that were
	// resized carry a resize source change; name/breakpoint updates apply to
	// every submitted layout that already exists.
	for _, layout := range layoutInputs {
		if _, exists := existingLayoutsByID[layout.ID]; !exists {
			continue
		}
		if resizeSource, has := resizeSourcesToUpdate[layout.ID]; has {
			if _, err := tx.ExecContext(ctx, `UPDATE "layout" SET resize_source = ?, name = ?, column_count = ?,
				left_gutter_column_count = ?, right_gutter_column_count = ?, breakpoint = ? WHERE id = ?`,
				nullableString(resizeSource), layout.Name, layout.ColumnCount,
				layout.LeftGutterColumnCount, layout.RightGutterColumnCount,
				layout.Breakpoint, layout.ID); err != nil {
				return nil, internalError(err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "layout" SET name = ?, column_count = ?,
			left_gutter_column_count = ?, right_gutter_column_count = ?, breakpoint = ? WHERE id = ?`,
			layout.Name, layout.ColumnCount, layout.LeftGutterColumnCount,
			layout.RightGutterColumnCount, layout.Breakpoint, layout.ID); err != nil {
			return nil, internalError(err)
		}
	}
	for _, layoutID := range removedLayoutIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "layout" WHERE id = ?`, layoutID); err != nil {
			return nil, internalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError(err)
	}

	// The output echoes the submitted layouts with the generated ids of added
	// layouts and the stored roles, sorted by breakpoint
	// (board.ts:1547-1553).
	out := make([]layoutOutput, 0, len(layoutInputs))
	for _, layout := range layoutInputs {
		entry := layoutOutput{
			ID: layout.ID, Name: layout.Name,
			ColumnCount:            layout.ColumnCount,
			LeftGutterColumnCount:  layout.LeftGutterColumnCount,
			RightGutterColumnCount: layout.RightGutterColumnCount,
			Breakpoint:             layout.Breakpoint,
			Role:                   "custom",
		}
		if newID, ok := savedLayoutIDs[layout.ID]; ok {
			entry.ID = newID
		}
		if existing, ok := existingLayoutsByID[layout.ID]; ok {
			entry.Role = existing.Role
		}
		out = append(out, entry)
	}
	sortLayoutOutputsByBreakpoint(out)
	return out, nil
}

// boardResetLayout is board.resetLayout (board.ts:1555-1635): regenerates a
// Mobile or custom layout from the Base layout through the responsive grid
// projection.
func (s *service) boardResetLayout(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	boardID, terr := dec.str("boardId")
	if terr != nil {
		return nil, terr
	}
	layoutID, terr := dec.str("layoutId")
	if terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, boardID, c, permModify); terr != nil {
		return nil, terr
	}

	board, terr := s.loadBoardForProjection(ctx, boardID)
	if terr != nil {
		return nil, terr
	}
	var targetLayout, baseLayout *db.Layout
	for i := range board.Layouts {
		if board.Layouts[i].ID == layoutID {
			targetLayout = &board.Layouts[i]
		}
		if board.Layouts[i].Role == "base" {
			baseLayout = &board.Layouts[i]
		}
	}
	if targetLayout == nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "Layout not found")
	}
	if baseLayout == nil {
		return nil, trpc.NewError(trpc.CodeInternalServerError, "Board must have a Base layout")
	}
	if targetLayout.Role == "base" {
		return nil, badRequestf("The Base layout cannot be reset")
	}

	projected, err := getUpdatedBoardLayout(board, updatedLayoutOptions{
		PreviousGeometry: layoutGeometryValueOf(*baseLayout),
		CurrentGeometry:  layoutGeometryValueOf(*targetLayout),
	})
	if err != nil {
		return nil, internalError(err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalError(err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE "layout" SET resize_source = NULL WHERE id = ?`, targetLayout.ID); err != nil {
		return nil, internalError(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM "item_layout" WHERE layout_id = ?`, targetLayout.ID); err != nil {
		return nil, internalError(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM "section_layout" WHERE layout_id = ?`, targetLayout.ID); err != nil {
		return nil, internalError(err)
	}
	for _, row := range projected.ItemSectionLayouts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.ItemID, row.SectionID, row.LayoutID, row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
			return nil, internalError(err)
		}
	}
	for _, row := range projected.SectionLayouts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.SectionID, row.LayoutID, nullableStr(row.ParentSectionID),
			row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
			return nil, internalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// layoutGeometryValueOfInput converts a submitted layout entry.
func layoutGeometryValueOfInput(l layoutInput) layoutGeometryValue {
	return layoutGeometryValue{
		LayoutID:               l.ID,
		ColumnCount:            l.ColumnCount,
		LeftGutterColumnCount:  l.LeftGutterColumnCount,
		RightGutterColumnCount: l.RightGutterColumnCount,
	}
}

// sortLayoutOutputsByBreakpoint is the toSorted by breakpoint of the
// saveLayouts output (board.ts:1553).
func sortLayoutOutputsByBreakpoint(layouts []layoutOutput) {
	sort.SliceStable(layouts, func(i, j int) bool { return layouts[i].Breakpoint < layouts[j].Breakpoint })
}

// ensureGutterRootSections ports ensureGutterRootSectionsAsync
// (board.ts:2755-2824): every lane used by a submitted layout gets its canvas
// root section (empty section at the lane offset, yOffset 0). The check and
// the inserts run in one transaction (upstream locks the board row with
// SELECT ... FOR UPDATE). More than one root at an offset is a corrupted
// board — an uncaught Error upstream, hence an internal error here.
func (s *service) ensureGutterRootSections(ctx context.Context, boardID string, lanes []string) *trpc.Error {
	if len(lanes) == 0 {
		return nil
	}
	offsets := map[string]int64{laneLeft: rootSectionOffsetLeft, laneRight: rootSectionOffsetRight}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return internalError(err)
	}
	defer func() { _ = tx.Rollback() }()
	args := []any{boardID}
	placeholders := repeatPlaceholders(len(lanes))
	for _, lane := range lanes {
		args = append(args, offsets[lane])
	}
	rows, err := tx.QueryContext(ctx, `SELECT x_offset FROM "section"
		WHERE board_id = ? AND kind = 'empty' AND x_offset IN (`+placeholders+`)`, args...)
	if err != nil {
		return internalError(err)
	}
	existing := map[int64]int{}
	for rows.Next() {
		var offset sql.NullInt64
		if err := rows.Scan(&offset); err != nil {
			_ = rows.Close()
			return internalError(err)
		}
		existing[offset.Int64]++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return internalError(err)
	}
	rows.Close()
	for _, lane := range lanes {
		if existing[offsets[lane]] > 1 {
			return internalError(fmt.Errorf(`Board "%s" has multiple %s canvas roots`, boardID, lane))
		}
		if existing[offsets[lane]] > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section" (id, board_id, kind, x_offset, y_offset, name, options)
			VALUES (?, ?, 'empty', ?, 0, NULL, '{"json": {}}')`,
			createID(), boardID, offsets[lane]); err != nil {
			return internalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internalError(err)
	}
	return nil
}

// nullableString maps "" to SQL NULL (the db package helper, mirrored here to
// keep this file's SQL self-contained).
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
