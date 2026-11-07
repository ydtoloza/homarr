package user

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Preference handlers, ported from packages/api/src/router/user/preferences.ts
// (getUserPreferencesAsync / updateUserPreferencesAsync) and the change*
// procedures of packages/api/src/router/user.ts:674-866.

// nullablePatch models the three states of a nullable preference: absent
// (keep), null (clear, Value "") or a value.
type nullablePatch struct {
	Set   bool
	Value string // empty = NULL
}

// prefsPatch is the parsed userPreferencesPatchSchema input (plus the
// header patch carried separately).
type prefsPatch struct {
	colorScheme               string
	hasColorScheme            bool
	byteUnitSystem            string
	hasByteUnitSystem         bool
	firstDayOfWeek            int64
	hasFirstDayOfWeek         bool
	pingIconsEnabled          bool
	hasPingIconsEnabled       bool
	enableRightClickOnWidgets bool
	hasEnableRightClick       bool
	homeBoardID               nullablePatch
	mobileHomeBoardID         nullablePatch
	defaultSearchEngineID     nullablePatch
	openSearchInNewTab        bool
	hasOpenSearchInNewTab     bool
	ddgBangs                  bool
	hasDdgBangs               bool
	header                    *headerPatch
}

// headerPatch is headerPreferencesPatchSchema (header-preferences.ts:
// 114-126): optional scalars and per-zone arrays that replace the zone.
type headerPatch struct {
	version       int
	hasVersion    bool
	visible       bool
	hasVisible    bool
	searchDisplay string
	hasSearch     bool
	logoDisplay   string
	hasLogo       bool
	zones         headerZones
	zonesSet      [3]bool // left, center, right supplied
}

// preferencesOutput renders userPreferencesResponseSchema
// (user.ts:51-67): the preference fields, userId and the parsed header
// preferences.
func preferencesOutput(userID string, prefs db.UserPreferencesRow, header headerPreferences) map[string]any {
	out := map[string]any{
		"userId":                    userID,
		"colorScheme":               prefs.ColorScheme,
		"byteUnitSystem":            prefs.ByteUnitSystem,
		"firstDayOfWeek":            prefs.FirstDayOfWeek,
		"pingIconsEnabled":          prefs.PingIconsEnabled,
		"enableRightClickOnWidgets": prefs.EnableRightClickOnWidgets,
		"homeBoardId":               nullString(prefs.HomeBoardID),
		"mobileHomeBoardId":         nullString(prefs.MobileHomeBoardID),
		"defaultSearchEngineId":     nullString(prefs.DefaultSearchEngineID),
		"openSearchInNewTab":        prefs.OpenSearchInNewTab,
		"ddgBangs":                  prefs.DdgBangs,
		"headerPreferences":         jsonToMap(header),
	}
	return out
}

// getPreferences is user.getPreferences (user.ts:399-418): protected;
// reading another user requires admin (hidden as NOT_FOUND).
func (s *service) getPreferences(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	userID := c.Session.UserID
	if raw, ok, terr := d.OptStr("userId"); terr != nil {
		return nil, terr
	} else if ok && raw != "" {
		userID = raw
	}
	if terr := requirePreferencesTarget(c, userID); terr != nil {
		return nil, terr
	}
	prefs, err := s.db.GetUserPreferences(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	return preferencesOutput(userID, prefs, parseHeaderPreferences(prefs.HeaderPreferences)), nil
}

// updatePreferences is user.updatePreferences (user.ts:674-704): a strict
// patch with at least one preference; merges over the current values and
// returns the complete result.
func (s *service) updatePreferences(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	// strictObject: unknown keys reject (user.ts:693).
	allowed := []string{"userId", "colorScheme", "byteUnitSystem", "firstDayOfWeek",
		"pingIconsEnabled", "enableRightClickOnWidgets", "homeBoardId", "mobileHomeBoardId",
		"defaultSearchEngineId", "openSearchInNewTab", "ddgBangs", "headerPreferences"}
	if unknown := d.UnknownKeys(allowed); len(unknown) > 0 {
		return nil, admin.BadRequestf("input contains unknown keys")
	}
	// .refine: at least one non-userId key (user.ts:694).
	hasPreference := false
	for _, key := range allowed[1:] {
		if d.Has(key) {
			hasPreference = true
			break
		}
	}
	if !hasPreference {
		return nil, admin.BadRequestf("Supply at least one preference")
	}
	userID := c.Session.UserID
	if raw, ok, terr := d.OptStr("userId"); terr != nil {
		return nil, terr
	} else if ok && raw != "" {
		userID = raw
	}
	patch, terr := parsePrefsPatch(d)
	if terr != nil {
		return nil, terr
	}
	return s.applyPreferencesPatch(ctx, c, userID, patch)
}

// applyPreferencesPatch is updateUserPreferencesAsync
// (preferences.ts:49-103): read current, merge, validate references, one
// write, return the merged preferences.
func (s *service) applyPreferencesPatch(ctx context.Context, c admin.Caller, userID string, patch prefsPatch) (map[string]any, *trpc.Error) {
	current, err := s.db.GetUserPreferences(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "User not found")
		}
		return nil, admin.InternalError(err)
	}
	header := parseHeaderPreferences(current.HeaderPreferences)

	// Header merge (preferences.ts:59-63): scalars replace, zones merge
	// per zone (a supplied zone array replaces that zone).
	if patch.header != nil {
		if patch.header.hasVersion {
			header.Version = patch.header.version
		}
		if patch.header.hasVisible {
			header.Visible = patch.header.visible
		}
		if patch.header.hasSearch {
			header.SearchDisplay = patch.header.searchDisplay
		}
		if patch.header.hasLogo {
			header.LogoDisplay = patch.header.logoDisplay
		}
		if patch.header.zonesSet[0] {
			header.Zones.Left = patch.header.zones.Left
		}
		if patch.header.zonesSet[1] {
			header.Zones.Center = patch.header.zones.Center
		}
		if patch.header.zonesSet[2] {
			header.Zones.Right = patch.header.zones.Right
		}
		// The merged header must satisfy headerPreferencesSchema
		// (preferences.ts:65-67).
		if header.Version != headerPreferencesVersion ||
			!admin.ContainsString(headerSearchDisplayValues, header.SearchDisplay) ||
			!admin.ContainsString(headerLogoDisplayValues, header.LogoDisplay) ||
			!validateHeaderZones(header.Zones) {
			return nil, admin.BadRequestf("invalid merged header preferences")
		}
	}

	// New board shortcuts must be accessible (preferences.ts:80-96).
	if patch.header != nil {
		existing := map[string]bool{}
		for _, item := range headerItems(parseHeaderPreferences(current.HeaderPreferences).Zones) {
			if item.Type == "board" {
				existing[item.BoardID] = true
			}
		}
		newBoardIDs := []string{}
		for _, item := range headerItems(header.Zones) {
			if item.Type == "board" && !existing[item.BoardID] {
				newBoardIDs = append(newBoardIDs, item.BoardID)
			}
		}
		if len(newBoardIDs) > 0 {
			accessible, err := admin.AccessibleBoardIDs(ctx, s.db, c)
			if err != nil {
				return nil, admin.InternalError(err)
			}
			for _, boardID := range newBoardIDs {
				if !accessible[boardID] {
					return nil, admin.BadRequestf("One or more board shortcuts are unavailable to this user")
				}
			}
		}
	}

	// Home board references need view access
	// (preferences.ts:70-72 via throwIfActionForbiddenAsync).
	for _, boardPatch := range []nullablePatch{patch.homeBoardID, patch.mobileHomeBoardID} {
		if boardPatch.Set && boardPatch.Value != "" {
			if terr := s.throwIfBoardViewForbidden(ctx, boardPatch.Value, c); terr != nil {
				return nil, terr
			}
		}
	}
	// The default search engine must exist (preferences.ts:73-79).
	if patch.defaultSearchEngineID.Set && patch.defaultSearchEngineID.Value != "" {
		if _, err := s.db.GetSearchEngineByID(ctx, patch.defaultSearchEngineID.Value); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, trpc.NewError(trpc.CodeBadRequest, "Search engine not found")
			}
			return nil, admin.InternalError(err)
		}
	}

	dbPatch := db.UserPreferencesPatch{}
	if patch.hasColorScheme {
		if !admin.ContainsString(colorSchemes, patch.colorScheme) {
			return nil, admin.BadRequestf(`input key "colorScheme" has an invalid value`)
		}
		dbPatch.ColorScheme = &patch.colorScheme
	}
	if patch.hasByteUnitSystem {
		if !admin.ContainsString(byteUnitSystems, patch.byteUnitSystem) {
			return nil, admin.BadRequestf(`input key "byteUnitSystem" has an invalid value`)
		}
		dbPatch.ByteUnitSystem = &patch.byteUnitSystem
	}
	if patch.hasFirstDayOfWeek {
		if patch.firstDayOfWeek < 0 || patch.firstDayOfWeek > 6 {
			return nil, admin.BadRequestf(`input key "firstDayOfWeek" must be between 0 and 6`)
		}
		dbPatch.FirstDayOfWeek = &patch.firstDayOfWeek
	}
	if patch.hasPingIconsEnabled {
		dbPatch.PingIconsEnabled = &patch.pingIconsEnabled
	}
	if patch.hasEnableRightClick {
		dbPatch.EnableRightClickOnWidgets = &patch.enableRightClickOnWidgets
	}
	if patch.homeBoardID.Set {
		dbPatch.HomeBoardID = &patch.homeBoardID.Value
	}
	if patch.mobileHomeBoardID.Set {
		dbPatch.MobileHomeBoardID = &patch.mobileHomeBoardID.Value
	}
	if patch.defaultSearchEngineID.Set {
		dbPatch.DefaultSearchEngineID = &patch.defaultSearchEngineID.Value
	}
	if patch.hasOpenSearchInNewTab {
		dbPatch.OpenSearchInNewTab = &patch.openSearchInNewTab
	}
	if patch.hasDdgBangs {
		dbPatch.DdgBangs = &patch.ddgBangs
	}
	if patch.header != nil {
		serialized := serializeHeaderPreferences(header)
		dbPatch.HeaderPreferences = &serialized
	}
	if err := s.db.UpdateUserPreferences(ctx, userID, dbPatch); err != nil {
		return nil, admin.InternalError(err)
	}

	// Re-read for the output (the merged result, preferences.ts:102).
	updated, err := s.db.GetUserPreferences(ctx, userID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return preferencesOutput(userID, updated, parseHeaderPreferences(updated.HeaderPreferences)), nil
}

// throwIfBoardViewForbidden is throwIfActionForbiddenAsync(..., "view")
// (packages/api/src/router/board/board-access.ts): every rejection —
// unknown board, insufficient access, anonymous — becomes NOT_FOUND
// "Board not found" so board existence cannot be snooped.
func (s *service) throwIfBoardViewForbidden(ctx context.Context, boardID string, c admin.Caller) *trpc.Error {
	allowed, err := admin.CanViewBoard(ctx, s.db, boardID, c)
	if errors.Is(err, db.ErrNotFound) {
		return trpc.NewError(trpc.CodeNotFound, "Board not found")
	}
	if err != nil {
		return admin.InternalError(err)
	}
	if !allowed {
		return trpc.NewError(trpc.CodeNotFound, "Board not found")
	}
	return nil
}

// parsePrefsPatch reads the userPreferencesPatchSchema fields.
func parsePrefsPatch(d *admin.InputDecoder) (prefsPatch, *trpc.Error) {
	patch := prefsPatch{}
	if v, ok, terr := d.OptStr("colorScheme"); terr != nil {
		return patch, terr
	} else if ok {
		patch.colorScheme, patch.hasColorScheme = v, true
	}
	if v, ok, terr := d.OptStr("byteUnitSystem"); terr != nil {
		return patch, terr
	} else if ok {
		patch.byteUnitSystem, patch.hasByteUnitSystem = v, true
	}
	if v, ok, terr := d.OptInt64("firstDayOfWeek", 0, 6); terr != nil {
		return patch, terr
	} else if ok {
		patch.firstDayOfWeek, patch.hasFirstDayOfWeek = v, true
	}
	if v, ok, terr := d.OptBool("pingIconsEnabled"); terr != nil {
		return patch, terr
	} else if ok {
		patch.pingIconsEnabled, patch.hasPingIconsEnabled = v, true
	}
	if v, ok, terr := d.OptBool("enableRightClickOnWidgets"); terr != nil {
		return patch, terr
	} else if ok {
		patch.enableRightClickOnWidgets, patch.hasEnableRightClick = v, true
	}
	if v, ok, terr := d.StringPtr("homeBoardId"); terr != nil {
		return patch, terr
	} else if ok {
		patch.homeBoardID = nullablePatch{Set: true, Value: deref(v)}
	}
	if v, ok, terr := d.StringPtr("mobileHomeBoardId"); terr != nil {
		return patch, terr
	} else if ok {
		patch.mobileHomeBoardID = nullablePatch{Set: true, Value: deref(v)}
	}
	if v, ok, terr := d.StringPtr("defaultSearchEngineId"); terr != nil {
		return patch, terr
	} else if ok {
		patch.defaultSearchEngineID = nullablePatch{Set: true, Value: deref(v)}
	}
	if v, ok, terr := d.OptBool("openSearchInNewTab"); terr != nil {
		return patch, terr
	} else if ok {
		patch.openSearchInNewTab, patch.hasOpenSearchInNewTab = v, true
	}
	if v, ok, terr := d.OptBool("ddgBangs"); terr != nil {
		return patch, terr
	} else if ok {
		patch.ddgBangs, patch.hasDdgBangs = v, true
	}
	if d.Has("headerPreferences") {
		header, terr := parseHeaderPatch(d)
		if terr != nil {
			return patch, terr
		}
		patch.header = header
	}
	return patch, nil
}

// parseHeaderPatch is headerPreferencesPatchSchema read field-wise.
func parseHeaderPatch(d *admin.InputDecoder) (*headerPatch, *trpc.Error) {
	obj, terr := d.Object("headerPreferences")
	if terr != nil {
		return nil, terr
	}
	patch := &headerPatch{}
	// strictObject: unknown keys reject.
	if unknown := obj.UnknownKeys([]string{"version", "visible", "searchDisplay", "logoDisplay", "zones"}); len(unknown) > 0 {
		return nil, admin.BadRequestf("headerPreferences contains unknown keys")
	}
	if v, ok, terr := obj.OptInt64("version", headerPreferencesVersion, headerPreferencesVersion); terr != nil {
		return nil, terr
	} else if ok {
		patch.version, patch.hasVersion = int(v), true
	}
	if v, ok, terr := obj.OptBool("visible"); terr != nil {
		return nil, terr
	} else if ok {
		patch.visible, patch.hasVisible = v, true
	}
	if v, ok, terr := obj.OptStr("searchDisplay"); terr != nil {
		return nil, terr
	} else if ok {
		if !admin.ContainsString(headerSearchDisplayValues, v) {
			return nil, admin.BadRequestf(`headerPreferences.searchDisplay has an invalid value`)
		}
		patch.searchDisplay, patch.hasSearch = v, true
	}
	if v, ok, terr := obj.OptStr("logoDisplay"); terr != nil {
		return nil, terr
	} else if ok {
		if !admin.ContainsString(headerLogoDisplayValues, v) {
			return nil, admin.BadRequestf(`headerPreferences.logoDisplay has an invalid value`)
		}
		patch.logoDisplay, patch.hasLogo = v, true
	}
	if obj.Has("zones") {
		zonesObj, terr := obj.Object("zones")
		if terr != nil {
			return nil, terr
		}
		if unknown := zonesObj.UnknownKeys([]string{"left", "center", "right"}); len(unknown) > 0 {
			return nil, admin.BadRequestf("headerPreferences.zones contains unknown keys")
		}
		for i, zone := range []string{"left", "center", "right"} {
			if !zonesObj.Has(zone) {
				continue
			}
			raw, _ := zonesObj.Raw(zone)
			items, ok := parseHeaderItemList(raw)
			if !ok {
				return nil, admin.BadRequestf("headerPreferences.zones.%s has an invalid value", zone)
			}
			switch i {
			case 0:
				patch.zones.Left = items
				patch.zonesSet[0] = true
			case 1:
				patch.zones.Center = items
				patch.zonesSet[1] = true
			default:
				patch.zones.Right = items
				patch.zonesSet[2] = true
			}
		}
	}
	return patch, nil
}

// deref returns the pointed string, or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- change* procedures ------------------------------------------------------

// changeHomeBoards is user.changeHomeBoards (user.ts:705-724).
func (s *service) changeHomeBoards(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "userId", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		if v, ok, terr := d.StringPtr("homeBoardId"); terr != nil {
			return terr
		} else if ok {
			patch.homeBoardID = nullablePatch{Set: true, Value: deref(v)}
		}
		if v, ok, terr := d.StringPtr("mobileHomeBoardId"); terr != nil {
			return terr
		} else if ok {
			patch.mobileHomeBoardID = nullablePatch{Set: true, Value: deref(v)}
		}
		return nil
	})
}

// changeDefaultSearchEngine is user.changeDefaultSearchEngine (user.ts:
// 725-749, deprecated alias preserving openSearchInNewTab).
func (s *service) changeDefaultSearchEngine(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "userId", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		if v, ok, terr := d.StringPtr("defaultSearchEngineId"); terr != nil {
			return terr
		} else if ok {
			patch.defaultSearchEngineID = nullablePatch{Set: true, Value: deref(v)}
		}
		if v, ok, terr := d.OptBool("ddgBangsEnabled"); terr != nil {
			return terr
		} else if ok {
			patch.ddgBangs, patch.hasDdgBangs = v, true
		}
		return nil
	})
}

// changeSearchPreferences is user.changeSearchPreferences (user.ts:750-770).
func (s *service) changeSearchPreferences(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "userId", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		if v, ok, terr := d.StringPtr("defaultSearchEngineId"); terr != nil {
			return terr
		} else if ok {
			patch.defaultSearchEngineID = nullablePatch{Set: true, Value: deref(v)}
		}
		if v, ok, terr := d.OptBool("openInNewTab"); terr != nil {
			return terr
		} else if ok {
			patch.openSearchInNewTab, patch.hasOpenSearchInNewTab = v, true
		}
		if v, ok, terr := d.OptBool("ddgBangsEnabled"); terr != nil {
			return terr
		} else if ok {
			patch.ddgBangs, patch.hasDdgBangs = v, true
		}
		return nil
	})
}

// changeColorScheme is user.changeColorScheme (user.ts:771-786): the
// caller only (no userId in the input).
func (s *service) changeColorScheme(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeSelfWithPatch(ctx, input, func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Enum("colorScheme", colorSchemes)
		if terr != nil {
			return terr
		}
		patch.colorScheme, patch.hasColorScheme = v, true
		return nil
	})
}

// changeByteUnitSystem is user.changeByteUnitSystem (user.ts:787-792).
func (s *service) changeByteUnitSystem(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Enum("byteUnitSystem", byteUnitSystems)
		if terr != nil {
			return terr
		}
		patch.byteUnitSystem, patch.hasByteUnitSystem = v, true
		return nil
	})
}

// changeEnableRightClickOnWidgets is user.changeEnableRightClickOnWidgets
// (user.ts:793-809).
func (s *service) changeEnableRightClickOnWidgets(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Bool("enableRightClickOnWidgets")
		if terr != nil {
			return terr
		}
		patch.enableRightClickOnWidgets, patch.hasEnableRightClick = v, true
		return nil
	})
}

// changePingIconsEnabled is user.changePingIconsEnabled (user.ts:810-814).
func (s *service) changePingIconsEnabled(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Bool("pingIconsEnabled")
		if terr != nil {
			return terr
		}
		patch.pingIconsEnabled, patch.hasPingIconsEnabled = v, true
		return nil
	})
}

// changeHeaderPreferences is user.changeHeaderPreferences (user.ts:815-831):
// the mutation schema also accepts legacy v1/v2 objects, migrated on read.
func (s *service) changeHeaderPreferences(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		raw, ok := d.Raw("headerPreferences")
		if !ok {
			return admin.BadRequestf(`expected an object for input key "headerPreferences"`)
		}
		parsed, ok := parseHeaderPreferencesValue(raw)
		if !ok {
			return admin.BadRequestf(`headerPreferences has an invalid value`)
		}
		patch.header = &headerPatch{
			version: parsed.Version, hasVersion: true,
			visible: parsed.Visible, hasVisible: true,
			searchDisplay: parsed.SearchDisplay, hasSearch: true,
			logoDisplay: parsed.LogoDisplay, hasLogo: true,
			zones:    headerZones{Left: parsed.Zones.Left, Center: parsed.Zones.Center, Right: parsed.Zones.Right},
			zonesSet: [3]bool{true, true, true},
		}
		return nil
	})
}

// changeDdgBangs is user.changeDdgBangs (user.ts:832-849, deprecated).
func (s *service) changeDdgBangs(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Bool("ddgBangs")
		if terr != nil {
			return terr
		}
		patch.ddgBangs, patch.hasDdgBangs = v, true
		return nil
	})
}

// changeFirstDayOfWeek is user.changeFirstDayOfWeek (user.ts:850-866).
func (s *service) changeFirstDayOfWeek(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.changeWithPatch(ctx, input, "id", func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error {
		v, terr := d.Int64("firstDayOfWeek", 0, 6)
		if terr != nil {
			return terr
		}
		patch.firstDayOfWeek, patch.hasFirstDayOfWeek = v, true
		return nil
	})
}

// changeWithPatch is the shared shape of the change* procedures with a
// target user: the patch applies to another user only for admins
// (getUserPreferencesAsync guard). The target key differs per procedure
// upstream: byIdSchema carries "id" while the home-board and search
// procedures carry "userId".
func (s *service) changeWithPatch(ctx context.Context, input json.RawMessage, idField string, read func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	patch := prefsPatch{}
	if terr := read(d, &patch); terr != nil {
		return nil, terr
	}
	userID, terr := d.Str(idField)
	if terr != nil {
		return nil, terr
	}
	return s.applyPreferencesPatch(ctx, c, userID, patch)
}

// changeSelfWithPatch targets the caller (changeColorScheme).
func (s *service) changeSelfWithPatch(ctx context.Context, input json.RawMessage, read func(d *admin.InputDecoder, patch *prefsPatch) *trpc.Error) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	patch := prefsPatch{}
	if terr := read(d, &patch); terr != nil {
		return nil, terr
	}
	return s.applyPreferencesPatch(ctx, c, c.Session.UserID, patch)
}
