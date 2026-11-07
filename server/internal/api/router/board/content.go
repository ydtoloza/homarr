package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Board content mutations: board.saveBoard, board.addItem and
// board.updateItemLayout, plus the validation helpers they share
// (board-layout-bounds, widget configuration validation and the Custom
// Widget placement guard of packages/api/src/router/board).

// --- input shapes ------------------------------------------------------------

// sectionInput is one element of boardSaveSchema.sections
// (packages/validation/src/shared.ts:50-102).
type sectionInput struct {
	ID      string
	Kind    string // "empty" | "container" | legacy "dynamic"
	XOffset int64
	YOffset int64
	// Options is the parsed container options (defaults applied); nil for
	// empty sections.
	Options map[string]any
	// Layouts holds the container placements; nil for empty sections.
	Layouts []containerPlacementInput
}

type containerPlacementInput struct {
	LayoutID        string
	ParentSectionID string
	XOffset         int64
	YOffset         int64
	Width           int64
	Height          int64
}

// itemInput is one element of boardSaveSchema.items
// (packages/validation/src/shared.ts:27-48).
type itemInput struct {
	ID              string
	Kind            string
	Options         map[string]any
	Layouts         []itemPlacementInput
	IntegrationIDs  []string
	AdvancedOptions map[string]any
}

type itemPlacementInput struct {
	LayoutID  string
	SectionID string
	XOffset   int64
	YOffset   int64
	Width     int64
	Height    int64
}

// parseSectionInput validates one section against the union schema
// (emptySectionSchema | containerSectionSchema | legacyDynamicSectionSchema).
func parseSectionInput(raw json.RawMessage) (sectionInput, *trpc.Error) {
	dec, terr := newInputDecoder(raw)
	if terr != nil {
		return sectionInput{}, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return sectionInput{}, terr
	}
	kind, terr := dec.str("kind")
	if terr != nil {
		return sectionInput{}, terr
	}
	if kind == "empty" {
		xOffset, terr := dec.int64("xOffset", -1<<31, 1<<31-1)
		if terr != nil {
			return sectionInput{}, terr
		}
		yOffset, terr := dec.int64("yOffset", -1<<31, 1<<31-1)
		if terr != nil {
			return sectionInput{}, terr
		}
		return sectionInput{ID: id, Kind: "empty", XOffset: xOffset, YOffset: yOffset}, nil
	}
	if kind != "container" && kind != "dynamic" {
		return sectionInput{}, badRequestf("invalid section kind")
	}
	options, terr := parseContainerOptionsInput(dec)
	if terr != nil {
		return sectionInput{}, terr
	}
	layoutsRaw, ok := dec.obj["layouts"]
	if !ok {
		return sectionInput{}, badRequestf(`expected layouts on container section`)
	}
	layouts, terr := parseContainerPlacements(layoutsRaw)
	if terr != nil {
		return sectionInput{}, terr
	}
	return sectionInput{ID: id, Kind: "container", Options: options, Layouts: layouts}, nil
}

// parseContainerOptionsInput applies containerSectionOptionsSchema
// (packages/validation/src/shared.ts:67-77): defaults for absent keys, type
// checks for present ones, unknown keys stripped.
func parseContainerOptionsInput(dec *inputDecoder) (map[string]any, *trpc.Error) {
	out := map[string]any{}
	raw, ok := dec.obj["options"]
	if !ok || raw == nil {
		// .default(containerSectionOptionsDefaults)
		for key, def := range containerSectionOptionsDefaults {
			out[key] = def
		}
		return out, nil
	}
	opts, ok := raw.(map[string]any)
	if !ok {
		return nil, badRequestf("section options must be an object")
	}
	for key, def := range containerSectionOptionsDefaults {
		value, present := opts[key]
		if !present || value == nil {
			out[key] = def
			continue
		}
		switch key {
		case "title", "borderColor":
			s, ok := value.(string)
			if !ok || (key == "title" && len(s) > 64) {
				return nil, badRequestf(`invalid container option "%s"`, key)
			}
			out[key] = s
		case "customCssClasses":
			classes, terr := stringList(value, "options.customCssClasses")
			if terr != nil {
				return nil, terr
			}
			out[key] = toAnySlice(classes)
		case "showLabel", "collapsible", "showOpenAll", "scrollable":
			b, ok := value.(bool)
			if !ok {
				return nil, badRequestf(`invalid container option "%s"`, key)
			}
			out[key] = b
		}
	}
	return out, nil
}

// parseContainerPlacements validates the container section layouts.
func parseContainerPlacements(raw any) ([]containerPlacementInput, *trpc.Error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, badRequestf("section layouts must be an array")
	}
	out := make([]containerPlacementInput, 0, len(list))
	for _, element := range list {
		entry, ok := element.(map[string]any)
		if !ok {
			return nil, badRequestf("invalid section layout entry")
		}
		dec := &inputDecoder{obj: entry}
		layoutID, terr := dec.str("layoutId")
		if terr != nil {
			return nil, terr
		}
		parentID, terr := dec.str("parentSectionId")
		if terr != nil {
			return nil, terr
		}
		xOffset, terr := dec.int64("xOffset", 0, 32767)
		if terr != nil {
			return nil, terr
		}
		yOffset, terr := dec.int64("yOffset", 0, 32767)
		if terr != nil {
			return nil, terr
		}
		width, terr := dec.int64("width", 1, 32767)
		if terr != nil {
			return nil, terr
		}
		height, terr := dec.int64("height", 1, 32767)
		if terr != nil {
			return nil, terr
		}
		out = append(out, containerPlacementInput{
			LayoutID: layoutID, ParentSectionID: parentID,
			XOffset: xOffset, YOffset: yOffset, Width: width, Height: height,
		})
	}
	return out, nil
}

// parseItemInput validates one item against commonItemSchema
// (packages/validation/src/board.ts:141-145 + shared.ts).
func parseItemInput(raw json.RawMessage) (itemInput, *trpc.Error) {
	dec, terr := newInputDecoder(raw)
	if terr != nil {
		return itemInput{}, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return itemInput{}, terr
	}
	kind, terr := dec.str("kind")
	if terr != nil {
		return itemInput{}, terr
	}
	if !widgetKindSet[kind] {
		return itemInput{}, badRequestf("invalid item kind")
	}
	options, terr := decodeRecord(dec, "options")
	if terr != nil {
		return itemInput{}, terr
	}
	integrationIDs, terr := dec.strListDefault("integrationIds", []string{})
	if terr != nil {
		return itemInput{}, terr
	}
	layoutsRaw, ok := dec.obj["layouts"]
	if !ok {
		return itemInput{}, badRequestf(`expected layouts on item`)
	}
	layoutsList, ok := layoutsRaw.([]any)
	if !ok {
		return itemInput{}, badRequestf("item layouts must be an array")
	}
	layouts := make([]itemPlacementInput, 0, len(layoutsList))
	for _, element := range layoutsList {
		entry, ok := element.(map[string]any)
		if !ok {
			return itemInput{}, badRequestf("invalid item layout entry")
		}
		layDec := &inputDecoder{obj: entry}
		layoutID, terr := layDec.str("layoutId")
		if terr != nil {
			return itemInput{}, terr
		}
		sectionID, terr := layDec.str("sectionId")
		if terr != nil {
			return itemInput{}, terr
		}
		xOffset, terr := layDec.int64("xOffset", 0, 32767)
		if terr != nil {
			return itemInput{}, terr
		}
		yOffset, terr := layDec.int64("yOffset", 0, 32767)
		if terr != nil {
			return itemInput{}, terr
		}
		width, terr := layDec.int64("width", 1, 32767)
		if terr != nil {
			return itemInput{}, terr
		}
		height, terr := layDec.int64("height", 1, 32767)
		if terr != nil {
			return itemInput{}, terr
		}
		layouts = append(layouts, itemPlacementInput{
			LayoutID: layoutID, SectionID: sectionID,
			XOffset: xOffset, YOffset: yOffset, Width: width, Height: height,
		})
	}

	// itemAdvancedOptionsSchema with defaults (title null, classes empty,
	// borderColor "").
	advanced := map[string]any{"title": nil, "customCssClasses": []any{}, "borderColor": ""}
	if advRaw, ok := dec.obj["advancedOptions"]; ok && advRaw != nil {
		adv, ok := advRaw.(map[string]any)
		if !ok {
			return itemInput{}, badRequestf("item advancedOptions must be an object")
		}
		if title, present := adv["title"]; present {
			if title == nil {
				advanced["title"] = nil
			} else if s, ok := title.(string); ok && len(s) <= 64 {
				advanced["title"] = s
			} else {
				return itemInput{}, badRequestf("invalid item advancedOptions.title")
			}
		}
		if classes, present := adv["customCssClasses"]; present {
			list, terr := stringList(classes, "advancedOptions.customCssClasses")
			if terr != nil {
				return itemInput{}, terr
			}
			advanced["customCssClasses"] = toAnySlice(list)
		}
		if border, present := adv["borderColor"]; present {
			s, ok := border.(string)
			if !ok {
				return itemInput{}, badRequestf("invalid item advancedOptions.borderColor")
			}
			advanced["borderColor"] = s
		}
	}

	return itemInput{
		ID: id, Kind: kind, Options: options, Layouts: layouts,
		IntegrationIDs: integrationIDs, AdvancedOptions: advanced,
	}, nil
}

// decodeRecord reads a required record<string, unknown> field.
func decodeRecord(dec *inputDecoder, key string) (map[string]any, *trpc.Error) {
	raw, ok := dec.obj[key]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, badRequestf(`expected an object for input key "%s"`, key)
	}
	return obj, nil
}

func toAnySlice(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

// --- saveBoard ---------------------------------------------------------------

// boardSaveBoard is board.saveBoard (board.ts:1688-2122): replaces the
// board's sections and items with the submitted arrays, diffing against the
// stored state.
func (s *service) boardSaveBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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

	sectionsRaw, ok := dec.obj["sections"]
	if !ok || sectionsRaw == nil {
		return nil, badRequestf(`expected sections array`)
	}
	sectionsList, ok := sectionsRaw.([]any)
	if !ok {
		return nil, badRequestf("sections must be an array")
	}
	sections := make([]sectionInput, 0, len(sectionsList))
	for _, element := range sectionsList {
		data, err := json.Marshal(element)
		if err != nil {
			return nil, badRequestf("invalid section entry")
		}
		sec, terr := parseSectionInput(data)
		if terr != nil {
			return nil, terr
		}
		sections = append(sections, sec)
	}

	itemsRaw, ok := dec.obj["items"]
	if !ok || itemsRaw == nil {
		return nil, badRequestf(`expected items array`)
	}
	itemsList, ok := itemsRaw.([]any)
	if !ok {
		return nil, badRequestf("items must be an array")
	}
	items := make([]itemInput, 0, len(itemsList))
	for _, element := range itemsList {
		data, err := json.Marshal(element)
		if err != nil {
			return nil, badRequestf("invalid item entry")
		}
		item, terr := parseItemInput(data)
		if terr != nil {
			return nil, terr
		}
		items = append(items, item)
	}

	// Stored state (the getFullBoardWithWhereAsync call of upstream).
	layoutRows, err := s.db.ListLayoutsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	storedSections, err := s.db.ListSectionsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	storedItems, err := s.db.ListItemsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	storedRelations, err := s.db.ListIntegrationItemsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}

	// Placement bounds (board.ts:1711): submitted sections/items against the
	// stored layouts.
	if issue := getBoardLayoutBoundsIssue(layoutRows, sections, items); issue != "" {
		return nil, boundsError(issue)
	}

	// Widget configuration validation (board.ts:1712).
	if terr := s.validateWidgetConfigurations(ctx, c, widgetConfigsOf(items), widgetConfigsOfStored(storedItems, storedRelations)); terr != nil {
		return nil, terr
	}

	// Custom Widget placement guard (board.ts:1713-1717).
	storedItemPlacements := make([]itemPlacement, len(storedItems))
	for i, item := range storedItems {
		storedItemPlacements[i] = itemPlacement{ID: item.ID, Kind: item.Kind, Options: storedItemOptions(item)}
	}
	submittedPlacements := make([]itemPlacement, len(items))
	for i, item := range items {
		submittedPlacements[i] = itemPlacement{ID: item.ID, Kind: item.Kind, Options: item.Options}
	}
	if err := throwIfCustomWidgetPlacementChangeForbidden(c.isAdmin(), submittedPlacements, storedItemPlacements); err != nil {
		return nil, err
	}

	// App option normalization (board.ts:1719-1720).
	for i := range items {
		if items[i].Kind == "app" {
			items[i].Options = normalizeAppWidgetOptions(items[i].Options)
		}
		// Timetable option changes are validated against the timetable
		// request handler upstream (SSRF checks with DNS resolution); that
		// external validation is deferred, see BOARDS.md.
	}

	storedSectionIDs := map[string]bool{}
	for _, sec := range storedSections {
		storedSectionIDs[sec.ID] = true
	}
	storedItemIDs := map[string]bool{}
	for _, item := range storedItems {
		storedItemIDs[item.ID] = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalError(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Added sections (board.ts:1742-1774).
	for _, sec := range sections {
		if storedSectionIDs[sec.ID] {
			continue
		}
		var (
			xOffset, yOffset any
			options          string
		)
		if sec.Kind == "empty" {
			xOffset, yOffset = sec.XOffset, sec.YOffset
			options = `{"json": {}}`
		} else {
			options = wrapSuperJSON(sec.Options)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section" (id, board_id, kind, x_offset, y_offset, name, options)
			VALUES (?, ?, ?, ?, ?, NULL, ?)`,
			sec.ID, id, sec.Kind, xOffset, yOffset, options); err != nil {
			return nil, internalError(err)
		}
		for _, layout := range sec.Layouts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
				x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				sec.ID, layout.LayoutID, nullableStr(layout.ParentSectionID), layout.XOffset, layout.YOffset,
				layout.Width, layout.Height); err != nil {
				return nil, internalError(err)
			}
		}
	}

	// Added items with their placements (board.ts:1776-1801).
	for _, item := range items {
		if storedItemIDs[item.ID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "item" (id, board_id, kind, options, advanced_options)
			VALUES (?, ?, ?, ?, ?)`,
			item.ID, id, item.Kind, wrapSuperJSON(item.Options), wrapSuperJSON(item.AdvancedOptions)); err != nil {
			return nil, internalError(err)
		}
		for _, layout := range item.Layouts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
				x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				item.ID, layout.SectionID, layout.LayoutID, layout.XOffset, layout.YOffset,
				layout.Width, layout.Height); err != nil {
				return nil, internalError(err)
			}
		}
	}

	// Integration relation diff (board.ts:1803-1831, 1894-1912).
	inputRelations := map[string]bool{}
	for _, item := range items {
		for _, integrationID := range item.IntegrationIDs {
			inputRelations[item.ID+"\x00"+integrationID] = true
		}
	}
	storedRelationSet := map[string]bool{}
	for _, rel := range storedRelations {
		storedRelationSet[rel.ItemID+"\x00"+rel.IntegrationID] = true
	}
	for key := range inputRelations {
		if storedRelationSet[key] {
			continue
		}
		var itemID, integrationID string
		_, _ = fmtSscan(key, &itemID, &integrationID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO "integration_item" (item_id, integration_id) VALUES (?, ?)`,
			itemID, integrationID); err != nil {
			return nil, internalError(err)
		}
	}
	for key := range storedRelationSet {
		if inputRelations[key] {
			continue
		}
		var itemID, integrationID string
		_, _ = fmtSscan(key, &itemID, &integrationID)
		if _, err := tx.ExecContext(ctx, `DELETE FROM "integration_item" WHERE item_id = ? AND integration_id = ?`,
			itemID, integrationID); err != nil {
			return nil, internalError(err)
		}
	}

	// Updated items (board.ts:1833-1859).
	for _, item := range items {
		if !storedItemIDs[item.ID] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "item" SET kind = ?, options = ?, advanced_options = ? WHERE id = ?`,
			item.Kind, wrapSuperJSON(item.Options), wrapSuperJSON(item.AdvancedOptions), item.ID); err != nil {
			return nil, internalError(err)
		}
		for _, layout := range item.Layouts {
			if _, err := tx.ExecContext(ctx, `UPDATE "item_layout" SET x_offset = ?, y_offset = ?, width = ?,
				height = ?, section_id = ? WHERE item_id = ? AND layout_id = ?`,
				layout.XOffset, layout.YOffset, layout.Width, layout.Height, layout.SectionID,
				item.ID, layout.LayoutID); err != nil {
				return nil, internalError(err)
			}
		}
	}

	// Updated sections (board.ts:1862-1891).
	for _, sec := range sections {
		if !storedSectionIDs[sec.ID] {
			continue
		}
		var (
			xOffset, yOffset any
			options          string
		)
		if sec.Kind == "empty" {
			xOffset, yOffset = sec.XOffset, sec.YOffset
			options = `{"json": {}}`
		} else {
			options = wrapSuperJSON(sec.Options)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "section" SET x_offset = ?, y_offset = ?, options = ?, name = NULL
			WHERE id = ?`, xOffset, yOffset, options, sec.ID); err != nil {
			return nil, internalError(err)
		}
		if sec.Kind != "container" {
			continue
		}
		for _, layout := range sec.Layouts {
			if _, err := tx.ExecContext(ctx, `UPDATE "section_layout" SET x_offset = ?, y_offset = ?, width = ?,
				height = ?, parent_section_id = ? WHERE section_id = ? AND layout_id = ?`,
				layout.XOffset, layout.YOffset, layout.Width, layout.Height,
				nullableStr(layout.ParentSectionID), sec.ID, layout.LayoutID); err != nil {
				return nil, internalError(err)
			}
		}
	}

	// Removed items and sections (board.ts:1914-1923). Like upstream, child
	// rows are not cascaded (foreign keys off, see the db package doc).
	removedItems := make([]string, 0)
	for _, item := range storedItems {
		if !submittedItemIDSet(items)[item.ID] {
			removedItems = append(removedItems, item.ID)
		}
	}
	for _, itemID := range removedItems {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "item" WHERE id = ?`, itemID); err != nil {
			return nil, internalError(err)
		}
	}
	removedSections := make([]string, 0)
	for _, sec := range storedSections {
		if !submittedSectionIDSet(sections)[sec.ID] {
			removedSections = append(removedSections, sec.ID)
		}
	}
	for _, sectionID := range removedSections {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "section" WHERE id = ?`, sectionID); err != nil {
			return nil, internalError(err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

func submittedItemIDSet(items []itemInput) map[string]bool {
	set := map[string]bool{}
	for _, item := range items {
		set[item.ID] = true
	}
	return set
}

func submittedSectionIDSet(sections []sectionInput) map[string]bool {
	set := map[string]bool{}
	for _, sec := range sections {
		set[sec.ID] = true
	}
	return set
}

// fmtSscan splits the "itemID\x00integrationID" relation key.
func fmtSscan(key string, itemID, integrationID *string) (int, error) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			*itemID = key[:i]
			*integrationID = key[i+1:]
			return 2, nil
		}
	}
	*itemID = key
	return 1, nil
}

// --- widget configuration validation ------------------------------------------

// widgetConfig is one submitted/stored widget configuration
// (packages/api/src/router/board.ts:152-156).
type widgetConfig struct {
	ID             string
	Kind           string
	IntegrationIDs []string
}

func widgetConfigsOf(items []itemInput) []widgetConfig {
	out := make([]widgetConfig, len(items))
	for i, item := range items {
		out[i] = widgetConfig{ID: item.ID, Kind: item.Kind, IntegrationIDs: item.IntegrationIDs}
	}
	return out
}

func widgetConfigsOfStored(items []db.Item, relations []db.IntegrationItem) []widgetConfig {
	out := make([]widgetConfig, 0, len(items))
	for _, item := range items {
		ids := []string{}
		for _, rel := range relations {
			if rel.ItemID == item.ID {
				ids = append(ids, rel.IntegrationID)
			}
		}
		out = append(out, widgetConfig{ID: item.ID, Kind: item.Kind, IntegrationIDs: ids})
	}
	return out
}

// haveSameIntegrationIds mirrors haveSameIntegrationIds (order-insensitive,
// board.ts:158-162).
func haveSameIntegrationIds(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	sortedLeft := sortedCopy(left)
	sortedRight := sortedCopy(right)
	for i := range sortedLeft {
		if sortedLeft[i] != sortedRight[i] {
			return false
		}
	}
	return true
}

func sortedCopy(list []string) []string {
	out := append([]string(nil), list...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// validateWidgetConfigurations is validateWidgetConfigurationsAsync
// (board.ts:164-219): unknown integrations and missing "use" access are
// NOT_FOUND. The widget-kind ↔ integration-kind compatibility map
// (getWidgetIntegrationIssue) is deferred, see BOARDS.md.
func (s *service) validateWidgetConfigurations(ctx context.Context, c caller, submitted, stored []widgetConfig) *trpc.Error {
	storedByID := map[string]widgetConfig{}
	for _, item := range stored {
		storedByID[item.ID] = item
	}
	changed := make([]widgetConfig, 0)
	for _, item := range submitted {
		storedItem, ok := storedByID[item.ID]
		if !ok || storedItem.Kind != item.Kind || !haveSameIntegrationIds(item.IntegrationIDs, storedItem.IntegrationIDs) {
			changed = append(changed, item)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	selected := map[string]bool{}
	var selectedIDs []string
	for _, item := range changed {
		for _, integrationID := range item.IntegrationIDs {
			if !selected[integrationID] {
				selected[integrationID] = true
				selectedIDs = append(selectedIDs, integrationID)
			}
		}
	}
	if len(selectedIDs) > 0 {
		kinds, err := s.db.IntegrationKinds(ctx, selectedIDs)
		if err != nil {
			return internalError(err)
		}
		for _, integrationID := range selectedIDs {
			if _, ok := kinds[integrationID]; !ok {
				return trpc.NewError(trpc.CodeNotFound, "Integration not found")
			}
		}
	}

	addedOrReconfigured := map[string]bool{}
	var addedIDs []string
	for _, item := range changed {
		storedItem, ok := storedByID[item.ID]
		if !ok || storedItem.Kind != item.Kind {
			for _, integrationID := range item.IntegrationIDs {
				if !addedOrReconfigured[integrationID] {
					addedOrReconfigured[integrationID] = true
					addedIDs = append(addedIDs, integrationID)
				}
			}
			continue
		}
		for _, integrationID := range item.IntegrationIDs {
			if containsString(storedItem.IntegrationIDs, integrationID) {
				continue
			}
			if !addedOrReconfigured[integrationID] {
				addedOrReconfigured[integrationID] = true
				addedIDs = append(addedIDs, integrationID)
			}
		}
	}
	return s.throwIfIntegrationActionsForbidden(ctx, c, addedIDs, "use")
}

// throwIfIntegrationActionsForbidden is throwIfIntegrationActionsForbiddenAsync
// (packages/api/src/router/integration/integration-access.ts:59-75): every
// integration must exist and grant the caller the permission; otherwise
// NOT_FOUND "Integration not found".
func (s *service) throwIfIntegrationActionsForbidden(ctx context.Context, c caller, integrationIDs []string, permission string) *trpc.Error {
	unique := map[string]bool{}
	var ids []string
	for _, id := range integrationIDs {
		if !unique[id] {
			unique[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	kinds, err := s.db.IntegrationKinds(ctx, ids)
	if err != nil {
		return internalError(err)
	}
	var userPerms, groupPerms map[string][]string
	if c.session != nil {
		if userPerms, err = s.db.IntegrationUserPermissionsForUser(ctx, c.session.UserID); err != nil {
			return internalError(err)
		}
		if groupPerms, err = s.db.IntegrationGroupPermissionsForUser(ctx, c.session.UserID); err != nil {
			return internalError(err)
		}
	}
	for _, id := range ids {
		if _, ok := kinds[id]; !ok {
			return trpc.NewError(trpc.CodeNotFound, "Integration not found")
		}
		if !hasIntegrationPermission(userPerms[id], groupPerms[id], c, permission) {
			return trpc.NewError(trpc.CodeNotFound, "Integration not found")
		}
	}
	return nil
}

// hasIntegrationPermission mirrors hasIntegrationPermission +
// constructIntegrationPermissions (packages/auth/permissions/
// integration-permissions.ts:14-34, integration-access.ts:12-22).
func hasIntegrationPermission(userPerms, groupPerms []string, c caller, permission string) bool {
	all := append(append([]string{}, userPerms...), groupPerms...)
	hasGlobalFull := c.hasPermission("integration-full-all")
	hasGlobalInteract := hasGlobalFull || c.hasPermission("integration-interact-all")
	hasGlobalUse := hasGlobalInteract || c.hasPermission("integration-use-all")

	hasFull := hasGlobalFull || containsString(all, "full")
	if hasFull {
		return true
	}
	hasInteract := hasGlobalInteract || containsString(all, "full") || containsString(all, "interact")
	hasUse := hasGlobalUse || len(all) >= 1
	if permission == "interact" {
		return hasInteract
	}
	if permission == "use" {
		return hasUse || hasInteract
	}
	return false
}

// --- Custom Widget placement guard -------------------------------------------

// itemPlacement is the guard's view of one item
// (custom-widget-placement-access.ts).
type itemPlacement struct {
	ID      string
	Kind    string
	Options map[string]any
}

// storedItemOptions decodes the stored options of an item for the Custom
// Widget guard; parse failures render the empty record (options only matter
// for Custom Widgets and the guard normalizes missing keys).
func storedItemOptions(item db.Item) map[string]any {
	options := map[string]any{}
	raw, terr := unwrapSuperJSON(json.RawMessage(item.Options), "item options")
	if terr == nil {
		_ = json.Unmarshal(raw, &options)
		if options == nil {
			options = map[string]any{}
		}
	}
	return options
}

// throwIfCustomWidgetPlacementChangeForbidden ports
// throwIfCustomWidgetPlacementChangeForbidden
// (packages/api/src/router/board/custom-widget-placement-access.ts:49-79).
func throwIfCustomWidgetPlacementChangeForbidden(isAdmin bool, submitted, stored []itemPlacement) *trpc.Error {
	storedByID := map[string]itemPlacement{}
	for _, item := range stored {
		storedByID[item.ID] = item
	}
	for _, submittedItem := range submitted {
		storedItem, ok := storedByID[submittedItem.ID]
		if !ok {
			if submittedItem.Kind == "customApi" && !isAdmin {
				return customWidgetForbidden()
			}
			continue
		}
		wasCustom := storedItem.Kind == "customApi"
		isCustom := submittedItem.Kind == "customApi"
		if wasCustom != isCustom {
			if isCustom || !isAdmin {
				return customWidgetForbidden()
			}
			continue
		}
		if wasCustom && !reflect.DeepEqual(
			normalizeCustomWidgetOptions(submittedItem.Options),
			normalizeCustomWidgetOptions(storedItem.Options),
		) && !isAdmin {
			return customWidgetForbidden()
		}
	}
	return nil
}

func customWidgetForbidden() *trpc.Error {
	return trpc.NewError(trpc.CodeForbidden, "Only administrators can add or configure Custom Widgets")
}

// normalizeCustomWidgetOptions ports normalizeCustomWidgetOptions
// (custom-widget-placement-access.ts:82-91).
func normalizeCustomWidgetOptions(options map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range options {
		out[key] = value
	}
	if configuration, ok := out["configuration"].(map[string]any); ok {
		out["configuration"] = configuration
	} else {
		out["configuration"] = map[string]any{}
	}
	version, ok := out["configurationVersion"].(float64)
	if ok && version == float64(int64(version)) {
		out["configurationVersion"] = int64(version)
	} else {
		out["configurationVersion"] = int64(1)
	}
	if interval, ok := out["refreshInterval"].(float64); ok {
		out["refreshInterval"] = interval
	} else {
		out["refreshInterval"] = float64(30)
	}
	return out
}

// normalizeAppWidgetOptions ports normalizeAppWidgetOptions
// (packages/validation/src/app.ts:67-79): migrates the legacy
// openInNewTab/openInNewWindow flags to the openIn option.
func normalizeAppWidgetOptions(options map[string]any) map[string]any {
	if _, ok := options["openIn"]; ok {
		return options
	}
	newTab, hasNewTab := options["openInNewTab"]
	newWindow, hasNewWindow := options["openInNewWindow"]
	if !hasNewTab && !hasNewWindow {
		return options
	}
	out := map[string]any{}
	for key, value := range options {
		if key == "openInNewTab" || key == "openInNewWindow" {
			continue
		}
		out[key] = value
	}
	openIn := "newTab"
	if b, ok := newWindow.(bool); ok && b {
		openIn = "newWindow"
	} else if b, ok := newTab.(bool); ok && !b {
		openIn = "currentTab"
	}
	out["openIn"] = openIn
	return out
}

// --- placement bounds --------------------------------------------------------

// boundsError builds the BAD_REQUEST of throwIfBoardLayoutBoundsInvalid
// (board.ts:147-150): the message plus the isBoardLayoutBoundsError marker
// the upstream errorFormatter adds to data.
func boundsError(issue string) *trpc.Error {
	return &trpc.Error{
		Code:    trpc.CodeBadRequest,
		Message: issue,
		Data:    map[string]any{"isBoardLayoutBoundsError": true},
	}
}

// boundsContext is the geometry subset used by the bounds helpers
// (BoardLayoutBoundsContext, packages/validation/src/board-layout-bounds.ts).
type boundsContext struct {
	layouts  []db.Layout
	sections []boundsSection
}

type boundsSection struct {
	id       string
	kind     string
	xOffset  int64
	xValid   bool
	layouts  []db.SectionLayout // stored container placements
	inputs   []containerPlacementInput
	hasStore bool
}

// getBoardSectionColumnCount is getBoardSectionColumnCount
// (board-layout-bounds.ts:24-34): "" (null) when the layout or section is
// unknown.
func getBoardSectionColumnCount(ctx boundsContext, layoutID, sectionID string, sectionWidth int64, sectionWidthSet bool) (int64, bool) {
	var layout *db.Layout
	for i := range ctx.layouts {
		if ctx.layouts[i].ID == layoutID {
			layout = &ctx.layouts[i]
			break
		}
	}
	section := findBoundsSection(ctx, sectionID)
	if layout == nil || section == nil {
		return 0, false
	}
	if section.kind == "empty" {
		offset := int64(0)
		if section.xValid {
			offset = section.xOffset
		}
		return getBoardLaneColumnCount(layoutGeometryOf(*layout), getRootSectionLane(offset)), true
	}
	if sectionWidthSet {
		return sectionWidth, true
	}
	return 0, false
}

func findBoundsSection(ctx boundsContext, id string) *boundsSection {
	for i := range ctx.sections {
		if ctx.sections[i].id == id {
			return &ctx.sections[i]
		}
	}
	return nil
}

// getBoardPlacementBoundsIssue is getBoardPlacementBoundsIssue
// (board-layout-bounds.ts:36-51): "" means valid.
func getBoardPlacementBoundsIssue(ctx boundsContext, layoutID string, xOffset, width int64, sectionID, name string, sectionWidth int64, sectionWidthSet bool) string {
	columnCount, ok := getBoardSectionColumnCount(ctx, layoutID, sectionID, sectionWidth, sectionWidthSet)
	layoutName := layoutID
	for _, l := range ctx.layouts {
		if l.ID == layoutID {
			layoutName = l.Name
			break
		}
	}
	if !ok {
		return name + ` references an unknown layout or section in layout "` + layoutName + `"`
	}
	if xOffset < 0 || width < 1 || xOffset+width > columnCount {
		return name + ` exceeds section "` + sectionID + `" bounds in layout "` + layoutName + `": xOffset ` +
			intToString(xOffset) + ` + width ` + intToString(width) + ` must fit within ` + intToString(columnCount) + ` columns`
	}
	return ""
}

// getBoardLayoutBoundsIssue is getBoardLayoutBoundsIssue
// (board-layout-bounds.ts:53-82): the first issue found across container
// placements and item placements.
func getBoardLayoutBoundsIssue(layouts []db.Layout, sections []sectionInput, items []itemInput) string {
	ctx := boundsContext{layouts: layouts}
	for _, sec := range sections {
		ctx.sections = append(ctx.sections, boundsSection{
			id: sec.ID, kind: sec.Kind, xOffset: sec.XOffset, xValid: true, inputs: sec.Layouts,
		})
	}

	for _, sec := range sections {
		if sec.Kind != "container" {
			continue
		}
		for _, placement := range sec.Layouts {
			// The parent section must resolve to the submitted section itself.
			issue := getBoardPlacementBoundsIssueWithInput(ctx, placement.LayoutID, placement.XOffset, placement.Width,
				placement.ParentSectionID, `Section "`+sec.ID+`"`, sections)
			if issue != "" {
				return issue
			}
		}
	}
	for _, item := range items {
		for _, placement := range item.Layouts {
			issue := getBoardPlacementBoundsIssueWithInput(ctx, placement.LayoutID, placement.XOffset, placement.Width,
				placement.SectionID, `Item "`+item.ID+`"`, sections)
			if issue != "" {
				return issue
			}
		}
	}
	return ""
}

// getBoardPlacementBoundsIssueWithInput resolves the section width from the
// submitted container placements.
func getBoardPlacementBoundsIssueWithInput(ctx boundsContext, layoutID string, xOffset, width int64, sectionID, name string, sections []sectionInput) string {
	sectionWidth := int64(0)
	widthSet := false
	for _, sec := range sections {
		if sec.ID != sectionID || sec.Kind == "empty" {
			continue
		}
		for _, placement := range sec.Layouts {
			if placement.LayoutID == layoutID {
				sectionWidth = placement.Width
				widthSet = true
				break
			}
		}
		if widthSet {
			break
		}
	}
	return getBoardPlacementBoundsIssue(ctx, layoutID, xOffset, width, sectionID, name, sectionWidth, widthSet)
}

// intToString renders int64 values for the bounds messages.
func intToString(v int64) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// --- addItem ------------------------------------------------------------------

// boardAddItem is board.addItem (board.ts:2275-2398): places a new item at
// the first free grid position of the main canvas in every layout.
func (s *service) boardAddItem(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	kind, terr := dec.enum("kind", widgetKinds)
	if terr != nil {
		return nil, terr
	}
	options, terr := decodeRecord(dec, "options")
	if terr != nil {
		return nil, terr
	}
	integrationIDs, terr := dec.strListDefault("integrationIds", []string{})
	if terr != nil {
		return nil, terr
	}
	if len(integrationIDs) > 32 {
		return nil, badRequestf("integrationIds accepts at most 32 entries")
	}
	if len(integrationIDs) != len(uniqueStrings(integrationIDs)) {
		return nil, badRequestf("integrationIds must be unique")
	}
	var sizeW, sizeH int64 = -1, -1
	if dec.has("size") {
		sizeRaw, ok := dec.obj["size"].(map[string]any)
		if !ok {
			return nil, badRequestf("size must be an object")
		}
		sizeDec := &inputDecoder{obj: sizeRaw}
		sizeW, terr = sizeDec.int64("width", 1, 24)
		if terr != nil {
			return nil, terr
		}
		sizeH, terr = sizeDec.int64("height", 1, 24)
		if terr != nil {
			return nil, terr
		}
	}

	if terr := s.requireBoardAccess(ctx, boardID, c, permModify); terr != nil {
		return nil, terr
	}

	if kind == "app" {
		options = normalizeAppWidgetOptions(options)
	}
	if terr := s.validateWidgetConfigurations(ctx, c, []widgetConfig{{ID: "", Kind: kind, IntegrationIDs: integrationIDs}}, nil); terr != nil {
		return nil, terr
	}
	if err := throwIfCustomWidgetPlacementChangeForbidden(c.isAdmin(),
		[]itemPlacement{{ID: "", Kind: kind, Options: options}}, nil); err != nil {
		return nil, err
	}
	// Timetable SSRF validation deferred, see BOARDS.md.

	// serializeBoardItemPlacementAsync (board.ts:95-119): per-board
	// serialization of the placement algorithm.
	unlock := s.placementLocks.lock(boardID)
	defer unlock()

	sections, err := s.db.ListSectionsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	layouts, err := s.db.ListLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	itemLayouts, err := s.db.ListItemLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	sectionLayouts, err := s.db.ListSectionLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}

	// The first main-canvas empty section by (yOffset, id) (board.ts:2327-2332).
	var emptySection *db.Section
	{
		candidates := []db.Section{}
		for i := range sections {
			sec := sections[i]
			if sec.Kind != "empty" {
				continue
			}
			offset := int64(0)
			if sec.XOffset.Valid {
				offset = sec.XOffset.Int64
			}
			if getRootSectionLane(offset) != laneMain {
				continue
			}
			candidates = append(candidates, sec)
		}
		db.SortEmptySectionsByOffset(candidates)
		if len(candidates) > 0 {
			emptySection = &candidates[0]
		}
	}
	if emptySection == nil {
		return nil, badRequestf("Board has no main section to place items in")
	}

	itemID := createID()
	defaultW, defaultH := int64(1), int64(1)
	if sizeW >= 0 {
		defaultW, defaultH = sizeW, sizeH
	} else if def, ok := widgetDefaultSizes[kind]; ok {
		defaultW, defaultH = def[0], def[1]
	}

	lane := getRootSectionLane(sectionOffset(emptySection))
	type placementRow struct {
		sectionID string
		layoutID  string
		xOffset   int64
		yOffset   int64
		width     int64
		height    int64
	}
	var layoutRows []placementRow
	for _, layout := range layouts {
		columnCount := getBoardLaneColumnCount(layoutGeometryOf(layout), lane)
		width := defaultW
		if columnCount < width {
			width = columnCount
		}
		var rectangles []placementRect
		for _, l := range itemLayouts {
			if l.SectionID == emptySection.ID && l.LayoutID == layout.ID {
				rectangles = append(rectangles, placementRect{xOffset: l.XOffset, yOffset: l.YOffset, width: l.Width, height: l.Height})
			}
		}
		for _, sl := range sectionLayouts {
			if sl.ParentSectionID == emptySection.ID && sl.LayoutID == layout.ID {
				rectangles = append(rectangles, placementRect{xOffset: sl.XOffset, yOffset: sl.YOffset, width: sl.Width, height: sl.Height})
			}
		}
		position, found := findFirstAvailableBoardItemPosition(rectangles, columnCount, width, defaultH)
		if !found {
			return nil, badRequestf("Board section is full, no free grid position available")
		}
		layoutRows = append(layoutRows, placementRow{
			sectionID: emptySection.ID, layoutID: layout.ID,
			xOffset: position[0], yOffset: position[1], width: width, height: defaultH,
		})
	}

	// Placement bounds (board.ts:2376-2380).
	boundsCtx := boundsContext{layouts: layouts}
	for _, sec := range sections {
		boundsCtx.sections = append(boundsCtx.sections, boundsSection{
			id: sec.ID, kind: sec.Kind, xOffset: sectionOffset(&sec), xValid: sec.XOffset.Valid,
			layouts: sectionLayouts, hasStore: true,
		})
	}
	for _, row := range layoutRows {
		issue := getBoardPlacementBoundsIssue(boundsCtx, row.layoutID, row.xOffset, row.width, row.sectionID,
			`Item "`+itemID+`"`, 0, false)
		if issue != "" {
			return nil, boundsError(issue)
		}
	}

	err = s.db.InsertItem(ctx, db.Item{
		ID: itemID, BoardID: boardID, Kind: kind,
		Options:         wrapSuperJSON(options),
		AdvancedOptions: `{"json": {}}`,
	})
	if err != nil {
		return nil, internalError(err)
	}
	for _, row := range layoutRows {
		if err := s.db.InsertItemLayout(ctx, db.ItemLayout{
			ItemID: itemID, SectionID: row.sectionID, LayoutID: row.layoutID,
			XOffset: row.xOffset, YOffset: row.yOffset, Width: row.width, Height: row.height,
		}); err != nil {
			return nil, internalError(err)
		}
	}
	for _, integrationID := range integrationIDs {
		if err := s.db.InsertIntegrationItem(ctx, db.IntegrationItem{ItemID: itemID, IntegrationID: integrationID}); err != nil {
			return nil, internalError(err)
		}
	}

	return map[string]any{"itemId": itemID}, nil
}

func sectionOffset(sec *db.Section) int64 {
	if sec.XOffset.Valid {
		return sec.XOffset.Int64
	}
	return 0
}

func uniqueStrings(list []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range list {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// placementRect is a BoardItemPlacementRectangle (board.ts:88-93).
type placementRect struct {
	xOffset int64
	yOffset int64
	width   int64
	height  int64
}

// findFirstAvailableBoardItemPosition ports findFirstAvailableBoardItemPosition
// (board.ts:121-139): the first free top-left grid slot for a size block.
func findFirstAvailableBoardItemPosition(placements []placementRect, columnCount, width, height int64) ([2]int64, bool) {
	var none [2]int64
	for yOffset := int64(0); yOffset < 9999; yOffset++ {
		for xOffset := int64(0); xOffset+width <= columnCount; xOffset++ {
			overlaps := false
			for _, p := range placements {
				if p.yOffset < yOffset+height &&
					p.yOffset+p.height > yOffset &&
					p.xOffset < xOffset+width &&
					p.xOffset+p.width > xOffset {
					overlaps = true
					break
				}
			}
			if !overlaps {
				return [2]int64{xOffset, yOffset}, true
			}
		}
	}
	return none, false
}

// --- updateItemLayout ----------------------------------------------------------

// boardUpdateItemLayout is board.updateItemLayout (board.ts:2399-2484).
func (s *service) boardUpdateItemLayout(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	itemID, terr := dec.str("itemId")
	if terr != nil {
		return nil, terr
	}
	layoutID, terr := dec.str("layoutId")
	if terr != nil {
		return nil, terr
	}
	xOffset, terr := dec.int64("xOffset", 0, 32767)
	if terr != nil {
		return nil, terr
	}
	yOffset, terr := dec.int64("yOffset", 0, 32767)
	if terr != nil {
		return nil, terr
	}
	width, terr := dec.int64("width", 1, 24)
	if terr != nil {
		return nil, terr
	}
	height, terr := dec.int64("height", 1, 24)
	if terr != nil {
		return nil, terr
	}

	if terr := s.requireBoardAccess(ctx, boardID, c, permModify); terr != nil {
		return nil, terr
	}

	unlock := s.placementLocks.lock(boardID)
	defer unlock()

	layouts, err := s.db.ListLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	sections, err := s.db.ListSectionsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	itemLayouts, err := s.db.ListItemLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	sectionLayouts, err := s.db.ListSectionLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}

	var targetLayout *db.Layout
	for i := range layouts {
		if layouts[i].ID == layoutID {
			targetLayout = &layouts[i]
			break
		}
	}
	var targetItemLayout *db.ItemLayout
	for i := range itemLayouts {
		if itemLayouts[i].ItemID == itemID && itemLayouts[i].LayoutID == layoutID {
			targetItemLayout = &itemLayouts[i]
			break
		}
	}
	if targetLayout == nil || targetItemLayout == nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "Item layout not found on this board")
	}
	var targetSection *db.Section
	for i := range sections {
		if sections[i].ID == targetItemLayout.SectionID {
			targetSection = &sections[i]
			break
		}
	}
	if targetSection == nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "Item section not found on this board")
	}

	// Bounds (board.ts:2446).
	boundsCtx := boundsContext{layouts: layouts}
	for i := range sections {
		boundsCtx.sections = append(boundsCtx.sections, boundsSection{
			id: sections[i].ID, kind: sections[i].Kind,
			xOffset: sectionOffset(&sections[i]), xValid: sections[i].XOffset.Valid,
			layouts: sectionLayouts, hasStore: true,
		})
	}
	sectionWidth := int64(0)
	widthSet := false
	if targetSection.Kind != "empty" {
		for _, sl := range sectionLayouts {
			if sl.SectionID == targetSection.ID && sl.LayoutID == layoutID {
				sectionWidth = sl.Width
				widthSet = true
				break
			}
		}
	}
	if issue := getBoardPlacementBoundsIssue(boundsCtx, layoutID, xOffset, width, targetSection.ID,
		`Item "`+itemID+`"`, sectionWidth, widthSet); issue != "" {
		return nil, boundsError(issue)
	}
	if yOffset+height > 32767 {
		return nil, badRequestf("Item layout exceeds its section bounds")
	}

	// Overlap check (board.ts:2451-2464): other items in the same section and
	// layout, plus child container placements.
	var others []placementRect
	for _, l := range itemLayouts {
		if l.ItemID == itemID || l.LayoutID != layoutID || l.SectionID != targetSection.ID {
			continue
		}
		others = append(others, placementRect{xOffset: l.XOffset, yOffset: l.YOffset, width: l.Width, height: l.Height})
	}
	for _, sl := range sectionLayouts {
		if sl.LayoutID != layoutID || sl.ParentSectionID != targetSection.ID {
			continue
		}
		others = append(others, placementRect{xOffset: sl.XOffset, yOffset: sl.YOffset, width: sl.Width, height: sl.Height})
	}
	submittedRect := placementRect{xOffset: xOffset, yOffset: yOffset, width: width, height: height}
	for _, other := range others {
		if doBoardItemPlacementsOverlap(submittedRect, other) {
			return nil, trpc.NewError(trpc.CodeConflict, "Item layout overlaps another item or section")
		}
	}

	if err := s.db.UpdateItemLayoutPosition(ctx, itemID, layoutID, targetSection.ID, xOffset, yOffset, width, height); err != nil {
		return nil, internalError(err)
	}

	// The input echoes back (output updateBoardItemLayoutSchema).
	return map[string]any{
		"boardId":  boardID,
		"itemId":   itemID,
		"layoutId": layoutID,
		"xOffset":  xOffset,
		"yOffset":  yOffset,
		"width":    width,
		"height":   height,
	}, nil
}

// doBoardItemPlacementsOverlap ports doBoardItemPlacementsOverlap
// (board.ts:141-145).
func doBoardItemPlacementsOverlap(left, right placementRect) bool {
	return left.yOffset < right.yOffset+right.height &&
		left.yOffset+left.height > right.yOffset &&
		left.xOffset < right.xOffset+right.width &&
		left.xOffset+left.width > right.xOffset
}

// --- duplicateBoard ------------------------------------------------------------

// boolToInt renders a boolean as the SQLite 0/1 integer column value.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// boardDuplicateBoard is board.duplicateBoard (board.ts:731-1038): clones the
// board with fresh ids, normalizing its layouts and projecting the source
// placements onto a generated Mobile layout when the source has only one.
func (s *service) boardDuplicateBoard(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	// permissionRequiredProcedure.requiresPermission("board-create")
	// (board.ts:731-732): signed in, then FORBIDDEN "Permission denied".
	if terr := requirePermission(c, "board-create"); terr != nil {
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
	// throwIfActionForbiddenAsync(..., "view") before anything else
	// (board.ts:753).
	if terr := s.requireBoardAccess(ctx, id, c, permView); terr != nil {
		return nil, terr
	}
	// noBoardWithSimilarNameAsync (board.ts:754).
	taken, err := s.db.BoardNameTaken(ctx, name, nil)
	if err != nil {
		return nil, internalError(err)
	}
	if taken {
		return nil, trpc.NewError(trpc.CodeConflict, "Board with similar name already exists")
	}

	// The relational query of board.ts:756-773: layouts, sections (with
	// collapse states and layouts) and items (with layouts and integrations).
	// Unlike the full-board reads this path does not repair protected layouts.
	board, err := s.db.GetBoardFullByID(ctx, id)
	if err != nil {
		return nil, notFoundBoard()
	}
	layoutRows, err := s.db.ListLayoutsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	sectionRows, err := s.db.ListSectionsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	sectionLayoutRows, err := s.db.ListSectionLayoutsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	itemRows, err := s.db.ListItemsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	itemLayoutRows, err := s.db.ListItemLayoutsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	integrationRows, err := s.db.ListIntegrationItemsByBoard(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}

	// throwIfCustomWidgetBoardDuplicationForbidden (board.ts:783): any
	// Custom Widget requires the admin permission.
	for _, item := range itemRows {
		if item.Kind == "customApi" && !c.isAdmin() {
			return nil, customWidgetForbidden()
		}
	}
	// validateWidgetConfigurationsAsync (board.ts:784-791) without stored
	// items: every integration of every item must exist and grant "use".
	widgets := make([]widgetConfig, 0, len(itemRows))
	for _, item := range itemRows {
		integrationIDs := make([]string, 0)
		for _, rel := range integrationRows {
			if rel.ItemID == item.ID {
				integrationIDs = append(integrationIDs, rel.IntegrationID)
			}
		}
		widgets = append(widgets, widgetConfig{ID: item.ID, Kind: item.Kind, IntegrationIDs: integrationIDs})
	}
	if terr := s.validateWidgetConfigurations(ctx, c, widgets, nil); terr != nil {
		return nil, terr
	}

	newBoardID := createID()
	generatedMobileLayoutID := createID()

	// Layout normalization and the projection onto the generated Mobile
	// layout (board.ts:795-851).
	var generatedPositions *updatedBoardLayout
	var normalizedLayouts []db.Layout
	switch {
	case len(layoutRows) == 0:
		normalizedLayouts = []db.Layout{
			{ID: generatedMobileLayoutID, Name: "Mobile", BoardID: id, ColumnCount: 3,
				Breakpoint: 0, Role: "mobile"},
			{ID: createID(), Name: "Base", BoardID: id, ColumnCount: 10,
				Breakpoint: 768, Role: "base"},
		}
	case len(layoutRows) == 1:
		projectionBoard := newBoardForLayoutProjection(id, layoutRows, sectionRows, sectionLayoutRows, itemRows, itemLayoutRows)
		positions, err := getUpdatedBoardLayout(projectionBoard, updatedLayoutOptions{
			PreviousGeometry: layoutGeometryValueOf(layoutRows[0]),
			CurrentGeometry: layoutGeometryValue{
				LayoutID: generatedMobileLayoutID, ColumnCount: 3,
			},
		})
		if err != nil {
			return nil, internalError(err)
		}
		generatedPositions = &positions
		base := layoutRows[0]
		base.Breakpoint, base.Role = 768, "base"
		normalizedLayouts = []db.Layout{
			{ID: generatedMobileLayoutID, Name: "Mobile", BoardID: id, ColumnCount: 3,
				Breakpoint: 0, Role: "mobile"},
			base,
		}
	default:
		normalizedLayouts = nil
		for _, layout := range normalizeBoardLayoutRoles(layoutRows) {
			normalizedLayouts = append(normalizedLayouts, layout.Layout)
		}
	}

	// The new ids (board.ts:852-911).
	layoutIDs := map[string]string{}
	for _, layout := range normalizedLayouts {
		layoutIDs[layout.ID] = createID()
	}
	sectionIDs := map[string]string{}
	for _, sec := range sectionRows {
		sectionIDs[sec.ID] = createID()
	}
	itemIDs := map[string]string{}
	for _, item := range itemRows {
		itemIDs[item.ID] = createID()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalError(err)
	}
	defer func() { _ = tx.Rollback() }()

	// The board row keeps every column of the source except name (the input)
	// and creatorId (the caller), board.ts:958-963/998-1003.
	if _, err := tx.ExecContext(ctx, `INSERT INTO "board" (id, name, is_public, creator_id, page_title,
		meta_title, logo_image_url, favicon_image_url, background_image_url, background_image_attachment,
		background_image_repeat, background_image_size, primary_color, secondary_color, opacity,
		custom_css, icon_color, item_radius, disable_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newBoardID, name, boolToInt(board.IsPublic), c.session.UserID,
		nullableStr(board.PageTitle), nullableStr(board.MetaTitle),
		nullableStr(board.LogoImageUrl), nullableStr(board.FaviconImageUrl),
		nullableStr(board.BackgroundImageUrl),
		board.BackgroundImageAttachment, board.BackgroundImageRepeat, board.BackgroundImageSize,
		board.PrimaryColor, board.SecondaryColor, board.Opacity,
		nullableStr(board.CustomCss), nullableStr(board.IconColor), board.ItemRadius,
		boolToInt(board.DisableStatus)); err != nil {
		return nil, internalError(err)
	}

	for _, layout := range normalizedLayouts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "layout" (id, name, board_id, column_count,
			left_gutter_column_count, right_gutter_column_count, breakpoint, role, resize_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			layoutIDs[layout.ID], layout.Name, newBoardID, layout.ColumnCount,
			layout.LeftGutterColumnCount, layout.RightGutterColumnCount,
			layout.Breakpoint, layout.Role); err != nil {
			return nil, internalError(err)
		}
	}
	for _, sec := range sectionRows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section" (id, board_id, kind, x_offset, y_offset, name, options)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sectionIDs[sec.ID], newBoardID, sec.Kind, sec.XOffset, sec.YOffset, sec.Name, sec.Options); err != nil {
			return nil, internalError(err)
		}
	}
	// Container placements of the stored layouts plus the projected
	// placements of the generated Mobile layout (board.ts:871-899). Parents
	// are remapped through the section map; an unmapped (orphaned) parent
	// becomes NULL like the undefined of sectionMap.get(...).
	for _, sl := range sectionLayoutRows {
		if _, ok := sectionIDs[sl.SectionID]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sectionIDs[sl.SectionID], layoutIDs[sl.LayoutID], nullableStr(sectionIDs[sl.ParentSectionID]),
			sl.XOffset, sl.YOffset, sl.Width, sl.Height); err != nil {
			return nil, internalError(err)
		}
	}
	if generatedPositions != nil {
		for _, row := range generatedPositions.SectionLayouts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO "section_layout" (section_id, layout_id, parent_section_id,
				x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				sectionIDs[row.SectionID], layoutIDs[row.LayoutID], nullableStr(sectionIDs[row.ParentSectionID]),
				row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
				return nil, internalError(err)
			}
		}
	}
	// Per-user collapse states keep their user and follow the new section id
	// (board.ts:900-909).
	if err := s.insertDuplicatedCollapseStates(ctx, tx, id, sectionIDs); err != nil {
		return nil, err
	}
	for _, item := range itemRows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "item" (id, board_id, kind, options, advanced_options)
			VALUES (?, ?, ?, ?, ?)`,
			itemIDs[item.ID], newBoardID, item.Kind, item.Options, item.AdvancedOptions); err != nil {
			return nil, internalError(err)
		}
	}
	for _, il := range itemLayoutRows {
		if _, ok := itemIDs[il.ItemID]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
			x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			itemIDs[il.ItemID], sectionIDs[il.SectionID], layoutIDs[il.LayoutID],
			il.XOffset, il.YOffset, il.Width, il.Height); err != nil {
			return nil, internalError(err)
		}
	}
	if generatedPositions != nil {
		for _, row := range generatedPositions.ItemSectionLayouts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO "item_layout" (item_id, section_id, layout_id,
				x_offset, y_offset, width, height) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				itemIDs[row.ItemID], sectionIDs[row.SectionID], layoutIDs[row.LayoutID],
				row.XOffset, row.YOffset, row.Width, row.Height); err != nil {
				return nil, internalError(err)
			}
		}
	}
	for _, rel := range integrationRows {
		if _, ok := itemIDs[rel.ItemID]; !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "integration_item" (item_id, integration_id) VALUES (?, ?)`,
			itemIDs[rel.ItemID], rel.IntegrationID); err != nil {
			return nil, internalError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, internalError(err)
	}
	return map[string]any{"boardId": newBoardID}, nil
}

// insertDuplicatedCollapseStates copies the section_collapse_state rows of
// the source board onto the duplicated sections (the collapseStates relation
// of board.ts:761-763, inserted at board.ts:900-909/1018-1020).
func (s *service) insertDuplicatedCollapseStates(ctx context.Context, tx *sql.Tx, boardID string, sectionIDs map[string]string) *trpc.Error {
	rows, err := tx.QueryContext(ctx, `SELECT sc.section_id, sc.user_id, sc.collapsed
		FROM "section_collapse_state" sc
		JOIN "section" s ON s.id = sc.section_id
		WHERE s.board_id = ?`, boardID)
	if err != nil {
		return internalError(err)
	}
	type collapseRow struct {
		sectionID string
		userID    string
		collapsed int
	}
	var states []collapseRow
	for rows.Next() {
		var row collapseRow
		if err := rows.Scan(&row.sectionID, &row.userID, &row.collapsed); err != nil {
			_ = rows.Close()
			return internalError(err)
		}
		states = append(states, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return internalError(err)
	}
	rows.Close()
	for _, state := range states {
		newSectionID, ok := sectionIDs[state.sectionID]
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO "section_collapse_state" (section_id, user_id, collapsed)
			VALUES (?, ?, ?)`, newSectionID, state.userID, state.collapsed); err != nil {
			return internalError(err)
		}
	}
	return nil
}
