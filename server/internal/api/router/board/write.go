package board

import (
	"context"
	"database/sql"
	"encoding/json"

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
