package user

import (
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
)

// Header preferences, ported from packages/validation/src/header-preferences.ts.
// The user.headerPreferences column stores a JSON string; v3 is the current
// shape, v1/v2 are migrated on read (headerPreferencesMutationSchema,
// header-preferences.ts:209-219). Field order in the Go structs matches
// JSON.stringify so the serialized round trip is byte-stable.

// headerPreferencesVersion is headerPreferencesVersion
// (header-preferences.ts:3).
const headerPreferencesVersion = 3

// headerBuiltinItemIds is headerBuiltinItemIds (header-preferences.ts:5-17).
var headerBuiltinItemIds = []string{
	"logo", "search", "home", "boardSwitcher", "assistant", "docker",
	"boardEdit", "boardSettings", "settings", "themeToggle", "user",
}

// requiredHeaderBuiltinItemIds is requiredHeaderBuiltinItemIds
// (header-preferences.ts:19): account access must stay in the header.
var requiredHeaderBuiltinItemIds = []string{"user"}

// headerSearchDisplayValues / headerLogoDisplayValues
// (header-preferences.ts:28-29).
var (
	headerSearchDisplayValues = []string{"input", "icon"}
	headerLogoDisplayValues   = []string{"logo", "logoAndText"}
)

// headerItem is one discriminated-union item: {type:"builtin", id} or
// {type:"board", boardId} (strict objects — unknown keys reject).
type headerItem struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	BoardID string `json:"boardId,omitempty"`
}

type headerZones struct {
	Left   []headerItem `json:"left"`
	Center []headerItem `json:"center"`
	Right  []headerItem `json:"right"`
}

// headerPreferences is headerPreferencesSchema (header-preferences.ts:102-110).
type headerPreferences struct {
	Version       int         `json:"version"`
	Visible       bool        `json:"visible"`
	SearchDisplay string      `json:"searchDisplay"`
	LogoDisplay   string      `json:"logoDisplay"`
	Zones         headerZones `json:"zones"`
}

// defaultHeaderPreferences is defaultHeaderPreferences
// (header-preferences.ts:221-235) — the same object the column default
// carries in the migration.
func defaultHeaderPreferences() headerPreferences {
	return headerPreferences{
		Version:       headerPreferencesVersion,
		Visible:       true,
		SearchDisplay: "input",
		LogoDisplay:   "logoAndText",
		Zones: headerZones{
			Left:   []headerItem{{Type: "builtin", ID: "logo"}},
			Center: []headerItem{{Type: "builtin", ID: "search"}},
			Right: []headerItem{
				{Type: "builtin", ID: "boardEdit"},
				{Type: "builtin", ID: "boardSettings"},
				{Type: "builtin", ID: "user"},
			},
		},
	}
}

// headerItemKey is getHeaderItemKey (header-preferences.ts:64-67).
func headerItemKey(item headerItem) string {
	if item.Type == "board" {
		return "board:" + item.BoardID
	}
	return "builtin:" + item.ID
}

// headerItems is getHeaderItems (header-preferences.ts:69).
func headerItems(zones headerZones) []headerItem {
	return append(append(append([]headerItem{}, zones.Left...), zones.Center...), zones.Right...)
}

// getDefaultHeaderZone is getDefaultHeaderZone (header-preferences.ts:76-81).
func getDefaultHeaderZone(item headerItem) string {
	if item.Type == "board" {
		return "right"
	}
	switch item.ID {
	case "logo":
		return "left"
	case "search":
		return "center"
	default:
		return "right"
	}
}

// includeRequiredHeaderItems is includeRequiredHeaderItems
// (header-preferences.ts:193-207): append the missing required items to the
// right zone.
func includeRequiredHeaderItems(preferences headerPreferences) headerPreferences {
	keys := map[string]bool{}
	for _, item := range headerItems(preferences.Zones) {
		keys[headerItemKey(item)] = true
	}
	for _, required := range requiredHeaderBuiltinItemIds {
		if keys["builtin:"+required] {
			continue
		}
		preferences.Zones.Right = append(preferences.Zones.Right, headerItem{Type: "builtin", ID: required})
	}
	return preferences
}

// parseHeaderPreferences is parseHeaderPreferences
// (header-preferences.ts:239-248): any parse/validation failure falls back
// to the defaults.
func parseHeaderPreferences(value string) headerPreferences {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) != nil {
		return defaultHeaderPreferences()
	}
	if parsed, ok := parseHeaderPreferencesValue(decoded); ok {
		return parsed
	}
	return defaultHeaderPreferences()
}

// parseHeaderPreferencesValue is headerPreferencesMutationSchema
// (header-preferences.ts:209-219): the discriminated union on version with
// the v1/v2 migrations.
func parseHeaderPreferencesValue(decoded any) (headerPreferences, bool) {
	obj, ok := decoded.(map[string]any)
	if !ok {
		return headerPreferences{}, false
	}
	version, ok := obj["version"].(float64)
	if !ok {
		return headerPreferences{}, false
	}
	switch int(version) {
	case 1:
		return migrateLegacyV1(obj)
	case 2:
		return migrateLegacyV2(obj)
	case 3:
		return parseV3(obj)
	default:
		return headerPreferences{}, false
	}
}

// parseV3 is headerPreferencesSchema (strict object + zones refinements).
func parseV3(obj map[string]any) (headerPreferences, bool) {
	visible, ok := obj["visible"].(bool)
	if !ok {
		return headerPreferences{}, false
	}
	searchDisplay, ok := obj["searchDisplay"].(string)
	if !ok || !admin.ContainsString(headerSearchDisplayValues, searchDisplay) {
		return headerPreferences{}, false
	}
	// logoDisplay carries a zod default (logoAndText) when absent.
	logoDisplay := "logoAndText"
	if raw, present := obj["logoDisplay"]; present && raw != nil {
		s, isString := raw.(string)
		if !isString || !admin.ContainsString(headerLogoDisplayValues, s) {
			return headerPreferences{}, false
		}
		logoDisplay = s
	}
	zonesObj, ok := obj["zones"].(map[string]any)
	if !ok {
		return headerPreferences{}, false
	}
	preferences := headerPreferences{
		Version:       headerPreferencesVersion,
		Visible:       visible,
		SearchDisplay: searchDisplay,
		LogoDisplay:   logoDisplay,
	}
	var itemsOK bool
	preferences.Zones.Left, itemsOK = parseHeaderItemList(zonesObj["left"])
	if !itemsOK {
		return headerPreferences{}, false
	}
	preferences.Zones.Center, itemsOK = parseHeaderItemList(zonesObj["center"])
	if !itemsOK {
		return headerPreferences{}, false
	}
	preferences.Zones.Right, itemsOK = parseHeaderItemList(zonesObj["right"])
	if !itemsOK {
		return headerPreferences{}, false
	}
	// Extra top-level keys reject (strict object).
	allowed := map[string]bool{"version": true, "visible": true, "searchDisplay": true,
		"logoDisplay": true, "zones": true}
	for key := range obj {
		if !allowed[key] {
			return headerPreferences{}, false
		}
	}
	// Extra zone keys reject (strict object).
	for key := range zonesObj {
		switch key {
		case "left", "center", "right":
		default:
			return headerPreferences{}, false
		}
	}
	if !validateHeaderZones(preferences.Zones) {
		return headerPreferences{}, false
	}
	return includeRequiredHeaderItems(preferences), true
}

// parseHeaderItemList is headerItemListSchema (max 100 strict items).
func parseHeaderItemList(raw any) ([]headerItem, bool) {
	list, isList := raw.([]any)
	if !isList {
		// Absent zones are allowed by the patch schema; the zod schema
		// itself requires all three keys, which the callers enforce.
		return nil, false
	}
	if len(list) > 100 {
		return nil, false
	}
	items := make([]headerItem, 0, len(list))
	for _, entry := range list {
		obj, isObj := entry.(map[string]any)
		if !isObj {
			return nil, false
		}
		itemType, _ := obj["type"].(string)
		switch itemType {
		case "builtin":
			id, _ := obj["id"].(string)
			if !admin.ContainsString(headerBuiltinItemIds, id) {
				return nil, false
			}
			if len(obj) != 2 {
				return nil, false
			}
			items = append(items, headerItem{Type: "builtin", ID: id})
		case "board":
			boardID, _ := obj["boardId"].(string)
			if len(boardID) < 1 || len(boardID) > 255 || len(obj) != 2 {
				return nil, false
			}
			items = append(items, headerItem{Type: "board", BoardID: boardID})
		default:
			return nil, false
		}
	}
	return items, true
}

// validateHeaderZones is headerZonesSchema's refinements
// (header-preferences.ts:85-100): at least one item, unique keys, account
// access present.
func validateHeaderZones(zones headerZones) bool {
	items := headerItems(zones)
	if len(items) == 0 {
		return false
	}
	seen := map[string]bool{}
	hasUser := false
	for _, item := range items {
		key := headerItemKey(item)
		if seen[key] {
			return false
		}
		seen[key] = true
		if item.Type == "builtin" && item.ID == "user" {
			hasUser = true
		}
	}
	return hasUser
}

// migrateLegacyV1 is migrateLegacyHeaderPreferencesV1
// (header-preferences.ts:179-188).
func migrateLegacyV1(obj map[string]any) (headerPreferences, bool) {
	visible, ok := obj["visible"].(bool)
	if !ok {
		return headerPreferences{}, false
	}
	legacyIDs, ok := legacyItemList(obj["items"], 1, len(headerBuiltinItemIds))
	if !ok {
		return headerPreferences{}, false
	}
	zones := headerZones{Left: []headerItem{}, Center: []headerItem{}, Right: []headerItem{}}
	for _, id := range legacyIDs {
		item := headerItem{Type: "builtin", ID: id}
		switch getDefaultHeaderZone(item) {
		case "left":
			zones.Left = append(zones.Left, item)
		case "center":
			zones.Center = append(zones.Center, item)
		default:
			zones.Right = append(zones.Right, item)
		}
	}
	return migrateLegacyItems(visible, zones), true
}

// migrateLegacyV2 is migrateLegacyHeaderPreferencesV2
// (header-preferences.ts:190-191).
func migrateLegacyV2(obj map[string]any) (headerPreferences, bool) {
	visible, ok := obj["visible"].(bool)
	if !ok {
		return headerPreferences{}, false
	}
	zonesObj, ok := obj["zones"].(map[string]any)
	if !ok {
		return headerPreferences{}, false
	}
	zones := headerZones{}
	var all []string
	for _, zone := range []string{"left", "center", "right"} {
		ids, ok := legacyItemList(zonesObj[zone], 0, len(headerBuiltinItemIds))
		if !ok {
			return headerPreferences{}, false
		}
		for _, id := range ids {
			all = append(all, id)
			item := headerItem{Type: "builtin", ID: id}
			switch zone {
			case "left":
				zones.Left = append(zones.Left, item)
			case "center":
				zones.Center = append(zones.Center, item)
			default:
				zones.Right = append(zones.Right, item)
			}
		}
	}
	// The v2 zones refine (header-preferences.ts:157-160): non-empty,
	// unique, includes account access.
	if len(all) == 0 || !hasUniqueKeysWithUser(all) {
		return headerPreferences{}, false
	}
	return migrateLegacyItems(visible, zones), true
}

// legacyItemList is the legacy z.enum(legacyHeaderItemIds) list with its
// min/max bounds.
func legacyItemList(raw any, minLen, maxLen int) ([]string, bool) {
	list, ok := raw.([]any)
	if !ok || len(list) < minLen || len(list) > maxLen {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		id, isString := entry.(string)
		if !isString || !admin.ContainsString(headerBuiltinItemIds, id) {
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}

// hasUniqueKeysWithUser is the legacy uniqueness refine.
func hasUniqueKeysWithAll(ids []string) bool {
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func hasUniqueKeysWithUser(ids []string) bool {
	if !hasUniqueKeysWithAll(ids) {
		return false
	}
	return admin.ContainsString(ids, "user")
}

// migrateLegacyItems is migrateLegacyItems (header-preferences.ts:164-177).
func migrateLegacyItems(visible bool, zones headerZones) headerPreferences {
	return includeRequiredHeaderItems(headerPreferences{
		Version:       headerPreferencesVersion,
		Visible:       visible,
		SearchDisplay: "input",
		LogoDisplay:   "logoAndText",
		Zones:         zones,
	})
}

// serializeHeaderPreferences is serializeHeaderPreferences
// (header-preferences.ts:250-251): JSON.stringify of the validated object.
func serializeHeaderPreferences(preferences headerPreferences) string {
	data, err := json.Marshal(includeRequiredHeaderItems(preferences))
	if err != nil {
		// headerPreferences only carries JSON-native fields.
		return ""
	}
	return string(data)
}
