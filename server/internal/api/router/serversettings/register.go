// Package serversettings is the Go port of the serverSettings tRPC router
// (packages/api/src/router/serverSettings.ts): the instance-wide settings
// stored in the serverSetting table as superjson payload strings.
//
// Reads merge the stored record over the upstream defaults
// (getServerSettingByKeyAsync, packages/db/queries/server-setting.ts:34-51);
// branding additionally runs through parseBrandingSettings
// (packages/server-settings/src/index.ts:104-128), which falls back to the
// full defaults when validation fails. JSON object key order follows Go map
// marshaling (alphabetical); upstream emits insertion order, which no
// consumer depends on.
package serversettings

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// defaultServerSettingsKeys is defaultServerSettingsKeys
// (packages/server-settings/src/index.ts:6-15).
var defaultServerSettingsKeys = []string{
	"analytics", "crawlingAndIndexing", "board", "user",
	"appearance", "branding", "culture", "search",
}

// options carries per-package behavior. The zero value is valid.
type Options struct{}

type service struct {
	db *db.DB
}

// Register registers the serverSettings router under its upstream paths.
func Register(r *trpc.Router, database *db.DB, _ Options) {
	s := &service{db: database}
	// serverSettings.getCulture (serverSettings.ts:83-85): public.
	r.Register("serverSettings.getCulture", s.getCulture)
	// serverSettings.getAll (serverSettings.ts:86-88): admin.
	r.Register("serverSettings.getAll", s.getAll)
	// serverSettings.getBranding (serverSettings.ts:89-96): public.
	r.Register("serverSettings.getBranding", s.getBranding)
	// serverSettings.getBoardSettings (serverSettings.ts:97-119): admin.
	r.Register("serverSettings.getBoardSettings", s.getBoardSettings)
	// serverSettings.updateBoardSettings (serverSettings.ts:120-142): admin.
	r.Register("serverSettings.updateBoardSettings", s.updateBoardSettings)
	// serverSettings.saveSettings (serverSettings.ts:143-183): admin.
	r.Register("serverSettings.saveSettings", s.saveSettings)
}

// getServerSettingByKey is getServerSettingByKeyAsync
// (packages/db/queries/server-setting.ts:34-51): the stored superjson
// record unwrapped and shallow-merged over the defaults; branding goes
// through parseBrandingSettings instead.
func (s *service) getServerSettingByKey(ctx context.Context, key string) (map[string]any, *trpc.Error) {
	setting, err := s.db.GetServerSetting(ctx, key)
	if errors.Is(err, db.ErrNotFound) {
		return defaultsFor(key), nil
	}
	if err != nil {
		return nil, admin.InternalError(err)
	}
	raw, terr := admin.UnwrapSuperJSON(json.RawMessage(setting.Value), "server setting "+key)
	if terr != nil {
		return nil, terr
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		// A malformed stored value behaves like "no stored keys" — the
		// spread of a non-object would throw upstream; the defaults keep
		// the procedure usable.
		return defaultsFor(key), nil
	}
	if key == "branding" {
		return parseBrandingSettings(stored), nil
	}
	merged := defaultsFor(key)
	for k, v := range stored {
		merged[k] = v
	}
	return merged, nil
}

// defaultsFor returns a fresh copy of defaultServerSettings[key]
// (packages/server-settings/src/index.ts:130-161).
func defaultsFor(key string) map[string]any {
	var value any
	switch key {
	case "analytics":
		value = map[string]any{
			"enableGeneral":            true,
			"instanceId":               nil,
			"lastSuccessfulSnapshotAt": nil,
		}
	case "crawlingAndIndexing":
		value = map[string]any{
			"noIndex":              true,
			"noFollow":             true,
			"noTranslate":          true,
			"noSiteLinksSearchBox": false,
		}
	case "board":
		value = map[string]any{
			"homeBoardId":           nil,
			"mobileHomeBoardId":     nil,
			"enableStatusByDefault": true,
			"forceDisableStatus":    false,
		}
	case "user":
		value = map[string]any{"enableGravatar": true}
	case "appearance":
		value = map[string]any{"defaultColorScheme": "auto"}
	case "branding":
		value = map[string]any{
			"appName":                  "Homarr",
			"greeting":                 "",
			"logoImageUrl":             nil,
			"faviconImageUrl":          nil,
			"primaryColor":             "#fa5252",
			"secondaryColor":           "#fd7e14",
			"lockPrimaryColor":         false,
			"signInBackgroundImageUrl": nil,
			"signInBackgroundOverlay":  0.55,
			"showSignInMarquee":        true,
			"authBranding": map[string]any{
				"showAppName":  true,
				"showLogo":     true,
				"showGreeting": true,
			},
			"defaultRadius": "md",
			"customCss":     "",
		}
	case "culture":
		value = map[string]any{"defaultLocale": "en"}
	case "search":
		value = map[string]any{"defaultSearchEngineId": nil}
	default:
		value = map[string]any{}
	}
	// Fresh copy: callers merge into the returned map.
	raw, _ := json.Marshal(value)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// getCulture is serverSettings.getCulture: publicProcedure.
func (s *service) getCulture(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	return s.getServerSettingByKey(ctx, "culture")
}

// getAll is serverSettings.getAll: admin; every settings key merged.
func (s *service) getAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	out := map[string]any{}
	for _, key := range defaultServerSettingsKeys {
		value, terr := s.getServerSettingByKey(ctx, key)
		if terr != nil {
			return nil, terr
		}
		out[key] = value
	}
	return out, nil
}

// getBranding is serverSettings.getBranding: publicProcedure; the branding
// settings through parseBrandingSettings (defaults on any inconsistency).
func (s *service) getBranding(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	return s.getServerSettingByKey(ctx, "branding")
}

// getBoardSettings is serverSettings.getBoardSettings: admin; the board
// defaults ({homeBoardId, mobileHomeBoardId, enableStatusByDefault,
// forceDisableStatus}).
func (s *service) getBoardSettings(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	return s.getServerSettingByKey(ctx, "board")
}

// updateBoardSettings is serverSettings.updateBoardSettings: admin; the
// partial input merges over the current settings, the full record is
// validated and every home board id must reference a public board
// (saveBoardSettingsAsync, serverSettings.ts:42-80). Returns the resulting
// settings.
func (s *service) updateBoardSettings(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	patch, terr := validateBoardSettingsPatch(d)
	if terr != nil {
		return nil, terr
	}
	return s.saveBoardSettings(ctx, patch)
}

// saveBoardSettingsAsync port (serverSettings.ts:42-80): patch merges over
// the current board settings, the merged record is validated, home board
// ids must reference public boards, then the row is upserted.
func (s *service) saveBoardSettings(ctx context.Context, patch map[string]any) (map[string]any, *trpc.Error) {
	current, terr := s.getServerSettingByKey(ctx, "board")
	if terr != nil {
		return nil, terr
	}
	next := map[string]any{}
	for k, v := range current {
		next[k] = v
	}
	for k, v := range patch {
		next[k] = v
	}
	if terr := validateBoardSettingsFull(next); terr != nil {
		return nil, terr
	}

	// The home boards must be public boards
	// (serverSettings.ts:54-69): the upstream query lists the public
	// boards among the ids, so a missing board is simply not public.
	var invalid []string
	for _, key := range []string{"homeBoardId", "mobileHomeBoardId"} {
		if id, ok := next[key].(string); ok && id != "" {
			public, err := s.db.BoardIsPublic(ctx, id)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return nil, admin.InternalError(err)
			}
			if !public {
				invalid = append(invalid, id)
			}
		}
	}
	if len(invalid) > 0 {
		message := "Board settings home board IDs must reference public boards: "
		for i, id := range invalid {
			if i > 0 {
				message += ", "
			}
			message += id
		}
		return nil, trpc.NewError(trpc.CodeBadRequest, message)
	}

	if err := s.db.UpsertServerSetting(ctx, "board", admin.WrapSuperJSON(next)); err != nil {
		return nil, admin.InternalError(err)
	}
	return next, nil
}

// validateBoardSettingsPatch is boardServerSettingsUpdateSchema
// (boardServerSettingsSchema.partial()): every supplied key must have the
// right type; unknown keys are dropped like zod non-strict objects do. The
// home board keys are z.string().nullable(): JSON null stays null (it clears
// the default upstream).
func validateBoardSettingsPatch(d *admin.InputDecoder) (map[string]any, *trpc.Error) {
	patch := map[string]any{}
	if raw, ok := d.Raw("homeBoardId"); ok {
		switch v := raw.(type) {
		case nil:
			patch["homeBoardId"] = nil
		case string:
			patch["homeBoardId"] = v
		default:
			return nil, admin.BadRequestf("board settings homeBoardId must be a string or null")
		}
	}
	if raw, ok := d.Raw("mobileHomeBoardId"); ok {
		switch v := raw.(type) {
		case nil:
			patch["mobileHomeBoardId"] = nil
		case string:
			patch["mobileHomeBoardId"] = v
		default:
			return nil, admin.BadRequestf("board settings mobileHomeBoardId must be a string or null")
		}
	}
	if v, ok, terr := d.OptBool("enableStatusByDefault"); terr != nil {
		return nil, terr
	} else if ok {
		patch["enableStatusByDefault"] = v
	}
	if v, ok, terr := d.OptBool("forceDisableStatus"); terr != nil {
		return nil, terr
	} else if ok {
		patch["forceDisableStatus"] = v
	}
	return patch, nil
}

// validateBoardSettingsFull is boardServerSettingsSchema
// (serverSettings.ts:24-29): the merged record must carry the four keys
// with the right types.
func validateBoardSettingsFull(settings map[string]any) *trpc.Error {
	if _, ok := settings["homeBoardId"].(string); !ok && settings["homeBoardId"] != nil {
		return admin.BadRequestf("board settings homeBoardId must be a string or null")
	}
	if _, ok := settings["mobileHomeBoardId"].(string); !ok && settings["mobileHomeBoardId"] != nil {
		return admin.BadRequestf("board settings mobileHomeBoardId must be a string or null")
	}
	if _, ok := settings["enableStatusByDefault"].(bool); !ok {
		return admin.BadRequestf("board settings enableStatusByDefault must be a boolean")
	}
	if _, ok := settings["forceDisableStatus"].(bool); !ok {
		return admin.BadRequestf("board settings forceDisableStatus must be a boolean")
	}
	return nil
}

// saveSettings is serverSettings.saveSettings: admin; dispatches on the
// settingsKey (serverSettings.ts:143-183).
func (s *service) saveSettings(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "admin"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	settingsKey, terr := d.Enum("settingsKey", defaultServerSettingsKeys)
	if terr != nil {
		return nil, terr
	}
	valueDecoder, terr := d.Object("value")
	if terr != nil {
		return nil, terr
	}

	switch settingsKey {
	case "board":
		patch, terr := validateBoardSettingsPatch(valueDecoder)
		if terr != nil {
			return nil, terr
		}
		if _, terr := s.saveBoardSettings(ctx, patch); terr != nil {
			return nil, terr
		}
		return nil, nil
	case "branding":
		if terr := s.saveBrandingSettings(ctx, valueDecoder); terr != nil {
			return nil, terr
		}
		return nil, nil
	case "analytics":
		// analyticsServerSettingsUpdateSchema is strict: unknown keys
		// reject (serverSettings.ts:32).
		if unknown := valueDecoder.UnknownKeys([]string{"enableGeneral"}); len(unknown) > 0 {
			return nil, admin.BadRequestf("analytics settings accept only the enableGeneral key")
		}
		current, terr := s.getServerSettingByKey(ctx, "analytics")
		if terr != nil {
			return nil, terr
		}
		// The update callback always runs (serverSettings.ts:175):
		// {...current, ...parsedInput}.
		if valueDecoder.Has("enableGeneral") {
			enableGeneral, _, terr := valueDecoder.OptBool("enableGeneral")
			if terr != nil {
				return nil, terr
			}
			current["enableGeneral"] = enableGeneral
		}
		if err := s.db.UpsertServerSetting(ctx, "analytics", admin.WrapSuperJSON(current)); err != nil {
			return nil, admin.InternalError(err)
		}
		return nil, nil
	default:
		// Generic merge: {...current, ...value} with no content
		// validation (serverSettings.ts:178-182).
		current, terr := s.getServerSettingByKey(ctx, settingsKey)
		if terr != nil {
			return nil, terr
		}
		for k, v := range valueDecoder.All() {
			current[k] = v
		}
		if err := s.db.UpsertServerSetting(ctx, settingsKey, admin.WrapSuperJSON(current)); err != nil {
			return nil, admin.InternalError(err)
		}
		return nil, nil
	}
}

// saveBrandingSettings is the branding branch of saveSettings
// (serverSettings.ts:156-171): the supplied keys (including the three
// legacy showCustom*OnLogin flags) merge over parseBrandingSettings of the
// current record, the full branding schema validates the result and the row
// is rewritten.
func (s *service) saveBrandingSettings(ctx context.Context, value *admin.InputDecoder) *trpc.Error {
	current, terr := s.getServerSettingByKey(ctx, "branding")
	if terr != nil {
		return terr
	}
	patch, terr := validateBrandingUpdate(value)
	if terr != nil {
		return terr
	}
	// authBranding starts from the current values and is layered: first the
	// legacy keys, then the structured authBranding patch
	// (serverSettings.ts:160-164).
	authBranding, _ := current["authBranding"].(map[string]any)
	if authBranding == nil {
		authBranding = defaultsFor("branding")["authBranding"].(map[string]any)
	}
	if v, ok := patchLegacy(value, "showCustomAppNameOnLogin"); ok {
		authBranding["showAppName"] = v
	}
	if v, ok := patchLegacy(value, "showCustomLogoOnLogin"); ok {
		authBranding["showLogo"] = v
	}
	if v, ok := patchLegacy(value, "showCustomGreetingOnLogin"); ok {
		authBranding["showGreeting"] = v
	}
	if nested, ok := patch["authBranding"].(map[string]any); ok {
		for k, v := range nested {
			authBranding[k] = v
		}
	}
	delete(patch, "authBranding")

	merged := parseBrandingSettings(current)
	for k, v := range patch {
		merged[k] = v
	}
	merged["authBranding"] = authBranding
	if terr := validateBrandingFull(merged); terr != nil {
		return terr
	}
	if err := s.db.UpsertServerSetting(ctx, "branding", admin.WrapSuperJSON(merged)); err != nil {
		return admin.InternalError(err)
	}
	return nil
}

// patchLegacy reads one optional boolean legacy key.
func patchLegacy(d *admin.InputDecoder, key string) (bool, bool) {
	v, ok := d.Raw(key)
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	return b, isBool
}

// deref returns the pointed string, or "" for nil (a null homeBoardId
// reference writes NULL).
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
