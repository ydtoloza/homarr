package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Responsive grid projection: the port of @homarr/common's grid algorithm
// (packages/common/src/grid-algorithm.ts) plus the layout projection that
// wraps it (getUpdatedBoardLayout and the resize-source bookkeeping of
// packages/api/src/router/board.ts:2603-2864 and
// packages/common/src/layout-resize-source.ts).
//
// The algorithm projects the placements of one layout ("previousWidth"
// columns) onto a layout with "width" columns: widening keeps every offset
// (only widths are clamped to the new column count), narrowing packs the
// items back-to-back in (yOffset, xOffset) order of their original
// placements, recursing into nested container sections. Container sections
// are placed like items, with their projected content height folded into the
// packing pass.

// gridItem mirrors GridAlgorithmItem
// (packages/common/src/grid-algorithm.ts:1-9). Type is "item" or "section";
// for sections SectionID is the parent section (root or container) the
// container sits in.
type gridItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Width     int64  `json:"width"`
	Height    int64  `json:"height"`
	XOffset   int64  `json:"xOffset"`
	YOffset   int64  `json:"yOffset"`
	SectionID string `json:"sectionId"`
}

// gridResult mirrors GridAlgorithmOutput
// (packages/common/src/grid-algorithm.ts:18-21).
type gridResult struct {
	Height int64
	Items  []gridItem
}

// errNoFreeSpot mirrors the "No free spot available" Error thrown by the
// packing pass (grid-algorithm.ts:90). It is unreachable through the
// upstream callers (normalizeItems clamps every width to the column count,
// so x=0 always fits once the scan passes the occupied rows), but it is kept
// for exact parity; callers surface it as INTERNAL_SERVER_ERROR.
var errNoFreeSpot = errors.New("No free spot available")

// generateResponsiveGridFor ports generateResponsiveGridFor
// (packages/common/src/grid-algorithm.ts:23-37): it groups the placements by
// section and projects the subtree rooted at sectionID.
func generateResponsiveGridFor(items []gridItem, width, previousWidth int64, sectionID string) (gridResult, error) {
	itemsBySection := map[string][]gridItem{}
	for _, item := range items {
		itemsBySection[item.SectionID] = append(itemsBySection[item.SectionID], item)
	}
	return generateResponsiveGridForSection(itemsBySection, previousWidth, width, sectionID, map[string]bool{})
}

// normalizedGridItem is a placement with the per-item previousWidth produced
// by normalizeItems (grid-algorithm.ts:99-101): previousWidth keeps the
// original width, Width is clamped to the current column count. The extra
// field never reaches the outputs (upstream spreads it into the result
// items, but every consumer reads only the GridAlgorithmItem fields).
type normalizedGridItem struct {
	gridItem
	previousWidth int64
}

// generateResponsiveGridForSection ports generateResponsiveGridForSection
// (grid-algorithm.ts:39-97).
func generateResponsiveGridForSection(itemsBySection map[string][]gridItem, previousWidth, width int64, sectionID string, ancestorSectionIDs map[string]bool) (gridResult, error) {
	// A corrupted recursive section graph stops projecting
	// (grid-algorithm.ts:50).
	if ancestorSectionIDs[sectionID] {
		return gridResult{}, nil
	}
	nextAncestors := make(map[string]bool, len(ancestorSectionIDs)+1)
	for id := range ancestorSectionIDs {
		nextAncestors[id] = true
	}
	nextAncestors[sectionID] = true

	itemsOfCurrentSection := append([]gridItem(nil), itemsBySection[sectionID]...)
	sortItemsForGrid(itemsOfCurrentSection)
	normalizedItems := make([]normalizedGridItem, len(itemsOfCurrentSection))
	for i, item := range itemsOfCurrentSection {
		normalizedItems[i] = normalizedGridItem{gridItem: item, previousWidth: item.Width}
		normalizedItems[i].Width = min(item.Width, width)
	}
	if len(itemsOfCurrentSection) == 0 {
		return gridResult{}, nil
	}

	// Nested container sections are always projected first, even when this
	// section itself is not reflowed (grid-algorithm.ts:61-74).
	newItems := make([]gridItem, 0, len(normalizedItems))
	dynamicSectionHeight := map[string]int64{}
	for _, dyn := range normalizedItems {
		if dyn.Type != "section" {
			continue
		}
		result, err := generateResponsiveGridForSection(itemsBySection, dyn.previousWidth, dyn.Width, dyn.ID, nextAncestors)
		if err != nil {
			return gridResult{}, err
		}
		newItems = append(newItems, result.Items...)
		dynamicSectionHeight[dyn.ID] = result.Height
	}

	// Widening (or an unchanged width) keeps every offset; only the widths
	// were clamped above. The section height covers the declared placements
	// (grid-algorithm.ts:76-81).
	if width >= previousWidth {
		var height int64
		for _, item := range itemsOfCurrentSection {
			height = max(height, item.YOffset+item.Height)
		}
		for _, item := range normalizedItems {
			newItems = append(newItems, item.gridItem)
		}
		return gridResult{Height: height, Items: newItems}, nil
	}

	// Narrowing packs the items back-to-back in scan order, folding the
	// projected heights of nested containers into the occupancy
	// (grid-algorithm.ts:83-96).
	occupied := make([][]bool, 0, len(normalizedItems))
	for _, item := range normalizedItems {
		height := item.Height
		if item.Type == "section" {
			// Math.max(dynamicSectionHeightMap.get(item.id) ?? 1, item.height)
			// — the ?? 1 branch is unreachable (every section of the current
			// section was recursed above), preserved for parity.
			projected := int64(1)
			if v, ok := dynamicSectionHeight[item.ID]; ok {
				projected = v
			}
			height = max(projected, item.Height)
		}
		position, ok := nextFreeGridSpot(occupied, item.Width, height, width)
		if !ok {
			return gridResult{}, errNoFreeSpot
		}
		addGridItemToOccupied(&occupied, item.Width, height, position[0], position[1], width)
		placed := item.gridItem
		placed.Height = height
		placed.XOffset = position[0]
		placed.YOffset = position[1]
		newItems = append(newItems, placed)
	}
	return gridResult{Height: int64(len(occupied)), Items: newItems}, nil
}

// sortItemsForGrid reproduces the toSorted comparator of
// grid-algorithm.ts:53-55: by yOffset, ties by xOffset (stable).
func sortItemsForGrid(items []gridItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].YOffset != items[j].YOffset {
			return items[i].YOffset < items[j].YOffset
		}
		return items[i].XOffset < items[j].XOffset
	})
}

// nextFreeGridSpot ports nextFreeSpot (grid-algorithm.ts:118-127): the first
// scan position (y up to 99999, x up to columnCount-1) where the block fits.
func nextFreeGridSpot(occupied [][]bool, width, height, columnCount int64) ([2]int64, bool) {
	for offsetY := int64(0); offsetY < 99999; offsetY++ {
		for offsetX := int64(0); offsetX < columnCount; offsetX++ {
			if offsetX+width <= columnCount && isGridSpotFree(occupied, width, height, offsetX, offsetY) {
				return [2]int64{offsetX, offsetY}, true
			}
		}
	}
	return [2]int64{}, false
}

// isGridSpotFree ports isFree (grid-algorithm.ts:129-138): rows beyond the
// occupied grid are free.
func isGridSpotFree(occupied [][]bool, width, height, x, y int64) bool {
	for yOffset := int64(0); yOffset < height; yOffset++ {
		row := y + yOffset
		if row >= int64(len(occupied)) {
			return true
		}
		for xOffset := int64(0); xOffset < width; xOffset++ {
			if occupied[row][x+xOffset] {
				return false
			}
		}
	}
	return true
}

// addGridItemToOccupied ports addItemToOccupied (grid-algorithm.ts:102-116):
// marks the block cells, extending the grid with empty rows of columnCount
// columns as needed.
func addGridItemToOccupied(occupied *[][]bool, width, height, x, y, columnCount int64) {
	for yOffset := int64(0); yOffset < height; yOffset++ {
		row := y + yOffset
		for int64(len(*occupied)) <= row {
			*occupied = append(*occupied, make([]bool, columnCount))
		}
		for xOffset := int64(0); xOffset < width; xOffset++ {
			(*occupied)[row][x+xOffset] = true
		}
	}
}

// --- layout projection (getUpdatedBoardLayout) --------------------------------

// layoutGeometryValue mirrors BoardLayoutGeometry (board.ts:2746-2751) and
// the geometrySchema of layout-resize-source.ts:5-9. The layout id travels
// with the runtime geometry but is not part of the serialized schema keys
// (zod strips it); omitempty reproduces both shapes on the wire.
type layoutGeometryValue struct {
	LayoutID               string `json:"layoutId,omitempty"`
	ColumnCount            int64  `json:"columnCount"`
	LeftGutterColumnCount  int64  `json:"leftGutterColumnCount"`
	RightGutterColumnCount int64  `json:"rightGutterColumnCount"`
}

// layoutGeometryValueOf converts a stored layout row.
func layoutGeometryValueOf(l db.Layout) layoutGeometryValue {
	return layoutGeometryValue{
		LayoutID:               l.ID,
		ColumnCount:            l.ColumnCount,
		LeftGutterColumnCount:  l.LeftGutterColumnCount,
		RightGutterColumnCount: l.RightGutterColumnCount,
	}
}

// laneGeometry adapts the geometry for getBoardLaneColumnCount: the runtime
// geometries carry no role, so gutters always apply (the TS objects have no
// role property either — undefined !== "mobile").
func laneGeometry(g layoutGeometryValue) layoutGeometry {
	return layoutGeometry{
		columnCount:            g.ColumnCount,
		leftGutterColumnCount:  g.LeftGutterColumnCount,
		rightGutterColumnCount: g.RightGutterColumnCount,
	}
}

// layoutResizeSource mirrors LayoutResizeSource
// (packages/common/src/layout-resize-source.ts:27-30): the pre-resize
// arrangement kept so a follow-up resize can rewind to it.
type layoutResizeSource struct {
	Geometry layoutGeometryValue
	Elements []gridItem
}

// geometryKey mirrors geometryKey
// (layout-resize-source.ts:32-33): JSON of [columnCount, left, right].
func geometryKey(g layoutGeometryValue) string {
	data, _ := json.Marshal([]int64{g.ColumnCount, g.LeftGutterColumnCount, g.RightGutterColumnCount})
	return string(data)
}

// elementsKey mirrors elementsKey (layout-resize-source.ts:34-47): JSON of
// the [id, type, sectionId, xOffset, yOffset, width, height] tuples sorted by
// id (upstream uses localeCompare; ids are ASCII so a byte comparison
// produces the same order for every id the database can contain).
func elementsKey(elements []gridItem) string {
	rows := make([][]any, 0, len(elements))
	for _, element := range elements {
		rows = append(rows, []any{
			element.ID, element.Type, element.SectionID,
			element.XOffset, element.YOffset, element.Width, element.Height,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i][0].(string) < rows[j][0].(string)
	})
	data, _ := json.Marshal(rows)
	return string(data)
}

// validResizeGeometry mirrors geometrySchema (z.number().int().positive() /
// nonnegative) for the parsed resize source.
func validResizeGeometry(g layoutGeometryValue) bool {
	return g.ColumnCount >= 1 && g.LeftGutterColumnCount >= 0 && g.RightGutterColumnCount >= 0
}

// validResizeElements mirrors elementSchema (layout-resize-source.ts:10-18).
func validResizeElements(elements []gridItem) bool {
	for _, element := range elements {
		if element.Type != "item" && element.Type != "section" {
			return false
		}
		if element.XOffset < 0 || element.YOffset < 0 || element.Width < 1 || element.Height < 1 {
			return false
		}
	}
	return true
}

// getLayoutResizeSource ports getLayoutResizeSource
// (layout-resize-source.ts:50-70): the stored pre-resize arrangement is kept
// only while the projected placements remain untouched; malformed or
// obsolete history falls back to the current geometry and elements.
func getLayoutResizeSource(geometry layoutGeometryValue, elements []gridItem, serializedSource string) layoutResizeSource {
	if serializedSource != "" {
		var parsed struct {
			Geometry          layoutGeometryValue `json:"geometry"`
			Elements          []gridItem          `json:"elements"`
			ProjectedGeometry layoutGeometryValue `json:"projectedGeometry"`
			ProjectedElements []gridItem          `json:"projectedElements"`
		}
		if err := json.Unmarshal([]byte(serializedSource), &parsed); err == nil &&
			validResizeGeometry(parsed.Geometry) && validResizeGeometry(parsed.ProjectedGeometry) &&
			validResizeElements(parsed.Elements) && validResizeElements(parsed.ProjectedElements) &&
			geometryKey(parsed.ProjectedGeometry) == geometryKey(geometry) &&
			elementsKey(parsed.ProjectedElements) == elementsKey(elements) {
			return layoutResizeSource{Geometry: parsed.Geometry, Elements: parsed.Elements}
		}
	}
	return layoutResizeSource{Geometry: geometry, Elements: elements}
}

// serializeLayoutResizeSource ports serializeLayoutResizeSource
// (layout-resize-source.ts:72-79): an unchanged geometry needs no history;
// otherwise the source arrangement and the projection are stored for the
// next resize. "" represents null.
func serializeLayoutResizeSource(source layoutResizeSource, projectedGeometry layoutGeometryValue, projectedElements []gridItem) string {
	if geometryKey(source.Geometry) == geometryKey(projectedGeometry) {
		return ""
	}
	if source.Elements == nil {
		source.Elements = []gridItem{}
	}
	if projectedElements == nil {
		projectedElements = []gridItem{}
	}
	data, err := json.Marshal(struct {
		Geometry          layoutGeometryValue `json:"geometry"`
		Elements          []gridItem          `json:"elements"`
		ProjectedGeometry layoutGeometryValue `json:"projectedGeometry"`
		ProjectedElements []gridItem          `json:"projectedElements"`
	}{source.Geometry, source.Elements, projectedGeometry, projectedElements})
	if err != nil {
		// All values are JSON-native; marshalling cannot fail.
		return ""
	}
	return string(data)
}

// projectionPlacement is the subset of an item_layout / section_layout row
// the projection consumes.
type projectionPlacement struct {
	LayoutID        string
	SectionID       string // item placements
	ParentSectionID string // container placements; empty = NULL
	Width           int64
	Height          int64
	XOffset         int64
	YOffset         int64
}

// projectionItem mirrors the items of BoardForLayoutProjection
// (board.ts:2576-2586) — the parsed items upstream feeds the projection.
type projectionItem struct {
	ID      string
	Layouts []projectionPlacement
}

// projectionSection mirrors the sections of BoardForLayoutProjection
// (board.ts:2587-2601). Only empty sections carry offsets (their union
// schema requires them); containers place themselves through Layouts.
type projectionSection struct {
	ID      string
	Kind    string
	XOffset int64
	YOffset int64
	Layouts []projectionPlacement
}

// boardForLayoutProjection mirrors BoardForLayoutProjection
// (board.ts:2573-2601): the board subset consumed by getUpdatedBoardLayout.
type boardForLayoutProjection struct {
	ID       string
	Layouts  []db.Layout
	Items    []projectionItem
	Sections []projectionSection
}

// newBoardForLayoutProjection assembles the projection input from the raw
// rows, mirroring the raw relational query of board.duplicateBoard
// (board.ts:756-773): section kinds stay as stored (the legacy "dynamic"
// kind is only transformed by parseSection, which the parsed paths run
// before assembling this input) and every item passes through. Empty
// sections without offsets behave like offsets 0 — the nullish handling of
// getRootSectionLane and the (yOffset ?? 0) sort of board.ts:2617-2619.
func newBoardForLayoutProjection(boardID string, layouts []db.Layout, sections []db.Section, sectionLayouts []db.SectionLayout, items []db.Item, itemLayouts []db.ItemLayout) *boardForLayoutProjection {
	board := &boardForLayoutProjection{ID: boardID, Layouts: layouts}

	board.Sections = make([]projectionSection, 0, len(sections))
	sectionIndexByID := map[string]int{}
	for _, sec := range sections {
		entry := projectionSection{ID: sec.ID, Kind: sec.Kind}
		if sec.Kind == "empty" {
			if sec.XOffset.Valid {
				entry.XOffset = sec.XOffset.Int64
			}
			if sec.YOffset.Valid {
				entry.YOffset = sec.YOffset.Int64
			}
		}
		board.Sections = append(board.Sections, entry)
		sectionIndexByID[sec.ID] = len(board.Sections) - 1
	}
	// The slice is pre-sized and only indexed (never re-pointed) so the
	// placement rows cannot land on a stale slice copy.
	for _, sl := range sectionLayouts {
		index, ok := sectionIndexByID[sl.SectionID]
		if !ok {
			continue
		}
		board.Sections[index].Layouts = append(board.Sections[index].Layouts, projectionPlacement{
			LayoutID: sl.LayoutID, ParentSectionID: sl.ParentSectionID,
			Width: sl.Width, Height: sl.Height, XOffset: sl.XOffset, YOffset: sl.YOffset,
		})
	}

	for _, item := range items {
		entry := projectionItem{ID: item.ID}
		for _, l := range itemLayouts {
			if l.ItemID != item.ID {
				continue
			}
			entry.Layouts = append(entry.Layouts, projectionPlacement{
				LayoutID: l.LayoutID, SectionID: l.SectionID,
				Width: l.Width, Height: l.Height, XOffset: l.XOffset, YOffset: l.YOffset,
			})
		}
		board.Items = append(board.Items, entry)
	}
	return board
}

// applyParsedBoardRows mirrors the parseSection/parseItem preprocessing of
// getFullBoardWithWhereAsync (board.ts:3024-3058) for the procedures that
// load the board through it (board.saveLayouts / board.resetLayout): the
// legacy "dynamic" kind becomes "container", empty sections must carry both
// offsets (a parseSection Error → INTERNAL_SERVER_ERROR) and items that
// fail the output union are skipped (board.ts:3086-3094). The section and
// item slices are modified in place.
func applyParsedBoardRows(sections []db.Section, items []db.Item) *trpc.Error {
	for i := range sections {
		switch sections[i].Kind {
		case "empty":
			if !sections[i].XOffset.Valid || !sections[i].YOffset.Valid {
				return trpc.NewError(trpc.CodeInternalServerError, "Section "+sections[i].ID+" is missing its canvas offsets")
			}
		case "container", "dynamic":
			// The legacy "dynamic" kind is transformed by
			// legacyDynamicSectionSchema (shared.ts:98-100).
			sections[i].Kind = "container"
		default:
			// sectionSchema is a closed union: any other kind fails
			// parseSection.
			return trpc.NewError(trpc.CodeInternalServerError, `Section "`+sections[i].ID+`" has an invalid kind`)
		}
	}
	kept := items[:0]
	for _, item := range items {
		// parseItem skips items that fail the output union
		// (board.ts:3086-3094): unknown kinds and non-record options or
		// advanced options blobs.
		if widgetKindSet[item.Kind] && isStoredRecord(item.Options) && isStoredRecord(item.AdvancedOptions) {
			kept = append(kept, item)
		}
	}
	return nil
}

// isStoredRecord reports whether the superjson blob unwraps to a JSON object
// (the z.record check of sharedItemSchema / itemAdvancedOptionsSchema).
func isStoredRecord(raw string) bool {
	unwrapped, terr := unwrapSuperJSON(json.RawMessage(raw), "stored value")
	if terr != nil {
		return false
	}
	var record map[string]any
	if err := json.Unmarshal(unwrapped, &record); err != nil {
		return false
	}
	return record != nil
}

// getElementsForLayout ports getElementsForLayout (board.ts:2826-2864): the
// grid items of one layout — items first, then containers with a parent
// (first placement per element wins, like Array.prototype.find).
func getElementsForLayout(board *boardForLayoutProjection, layoutID string) []gridItem {
	elements := []gridItem{}
	for _, item := range board.Items {
		for _, l := range item.Layouts {
			if l.LayoutID != layoutID {
				continue
			}
			elements = append(elements, gridItem{
				ID: item.ID, Type: "item",
				Height: l.Height, Width: l.Width,
				XOffset: l.XOffset, YOffset: l.YOffset,
				SectionID: l.SectionID,
			})
			break
		}
	}
	for _, sec := range board.Sections {
		if sec.Kind != "container" {
			continue
		}
		for _, l := range sec.Layouts {
			if l.LayoutID != layoutID || l.ParentSectionID == "" {
				continue
			}
			elements = append(elements, gridItem{
				ID: sec.ID, Type: "section",
				Height: l.Height, Width: l.Width,
				XOffset: l.XOffset, YOffset: l.YOffset,
				SectionID: l.ParentSectionID,
			})
			break
		}
	}
	return elements
}

// projectedItemLayout is one projected item_layout row
// (board.ts:2675-2690).
type projectedItemLayout struct {
	ItemID    string
	LayoutID  string
	SectionID string
	XOffset   int64
	YOffset   int64
	Width     int64
	Height    int64
}

// projectedSectionLayout is one projected section_layout row
// (board.ts:2692-2708). ParentSectionID is empty for NULL.
type projectedSectionLayout struct {
	SectionID       string
	LayoutID        string
	ParentSectionID string
	XOffset         int64
	YOffset         int64
	Width           int64
	Height          int64
}

// updatedBoardLayout is the getUpdatedBoardLayout return value
// (board.ts:2710-2714). ResizeSource is "" for null.
type updatedBoardLayout struct {
	ItemSectionLayouts []projectedItemLayout
	SectionLayouts     []projectedSectionLayout
	ResizeSource       string
}

// updatedLayoutOptions is the options parameter of getUpdatedBoardLayout
// (board.ts:2603-2609). PreviousElements carries pre-computed source
// elements (the chained resize of board.saveLayouts); nil derives them from
// the board's placements in the previous layout.
type updatedLayoutOptions struct {
	PreviousGeometry layoutGeometryValue
	PreviousElements []gridItem
	CurrentGeometry  layoutGeometryValue
}

// getUpdatedBoardLayout ports getUpdatedBoardLayout (board.ts:2603-2715):
// projects the board's placements onto one target layout and produces the
// item_layout / section_layout rows plus the resize-source history. The
// source arrangement is rewound through the stored resize source when the
// current placements are still the untouched projection of the last resize.
func getUpdatedBoardLayout(board *boardForLayoutProjection, opts updatedLayoutOptions) (updatedBoardLayout, error) {
	currentElements := opts.PreviousElements
	if currentElements == nil {
		currentElements = getElementsForLayout(board, opts.PreviousGeometry.LayoutID)
	}
	var serializedSource string
	if opts.PreviousGeometry.LayoutID == opts.CurrentGeometry.LayoutID {
		for _, layout := range board.Layouts {
			if layout.ID == opts.PreviousGeometry.LayoutID {
				serializedSource = layout.ResizeSource
				break
			}
		}
	}
	source := getLayoutResizeSource(opts.PreviousGeometry, currentElements, serializedSource)
	elements := source.Elements
	previousGeometry := source.Geometry
	previousGeometry.LayoutID = opts.PreviousGeometry.LayoutID

	// The canvas roots per lane (board.ts:2617-2626): empty sections sorted
	// by (yOffset ?? 0, id), first root per lane wins.
	var emptyRoots []projectionSection
	for _, sec := range board.Sections {
		if sec.Kind == "empty" {
			emptyRoots = append(emptyRoots, sec)
		}
	}
	// (first.yOffset ?? 0) - (second.yOffset ?? 0) || first.id.localeCompare(second.id)
	// — empty sections always carry both offsets here (validated while
	// assembling the projection input), so the nullish default never applies.
	sort.SliceStable(emptyRoots, func(i, j int) bool {
		if emptyRoots[i].YOffset != emptyRoots[j].YOffset {
			return emptyRoots[i].YOffset < emptyRoots[j].YOffset
		}
		return emptyRoots[i].ID < emptyRoots[j].ID
	})
	rootByLane := map[string]string{}
	for _, root := range emptyRoots {
		lane := getRootSectionLane(root.XOffset)
		if _, ok := rootByLane[lane]; !ok {
			rootByLane[lane] = root.ID
		}
	}
	mainRoot := rootByLane[laneMain]
	if mainRoot == "" {
		return updatedBoardLayout{}, fmt.Errorf(`Board "%s" has no main canvas root`, board.ID)
	}
	sourceRootByID := map[string]string{}
	for _, root := range emptyRoots {
		sourceRootByID[root.ID] = getRootSectionLane(root.XOffset)
	}
	// Lanes without columns in the target layout fold into the main canvas
	// (board.ts:2631-2636).
	targetLaneBySourceLane := map[string]string{}
	for _, lane := range boardLanes {
		if lane != laneMain && getBoardLaneColumnCount(laneGeometry(opts.CurrentGeometry), lane) == 0 {
			targetLaneBySourceLane[lane] = laneMain
		} else {
			targetLaneBySourceLane[lane] = lane
		}
	}
	// Only direct children of roots move between lanes (board.ts:2637-2646):
	// nested container children keep their sectionId.
	remappedElements := make([]gridItem, len(elements))
	for i, element := range elements {
		remappedElements[i] = element
		sourceLane, ok := sourceRootByID[element.SectionID]
		if !ok {
			continue
		}
		targetLane := targetLaneBySourceLane[sourceLane]
		if targetLane == "" {
			targetLane = laneMain
		}
		targetRoot := rootByLane[targetLane]
		if targetRoot == "" {
			targetRoot = mainRoot
		}
		remappedElements[i].SectionID = targetRoot
	}

	// One projection per target lane with a root and non-zero width
	// (board.ts:2648-2672). When several source lanes merge into the target
	// lane the placements can overlap, so the width ceiling forces the
	// packing pass (Number.MAX_SAFE_INTEGER upstream).
	var updatedElements []gridItem
	for _, lane := range boardLanes {
		root, ok := rootByLane[lane]
		if !ok {
			continue
		}
		width := getBoardLaneColumnCount(laneGeometry(opts.CurrentGeometry), lane)
		if width == 0 {
			continue
		}
		var sourceLanes []string
		for _, sourceLane := range boardLanes {
			if _, hasRoot := rootByLane[sourceLane]; !hasRoot {
				continue
			}
			if getBoardLaneColumnCount(laneGeometry(previousGeometry), sourceLane) > 0 &&
				targetLaneBySourceLane[sourceLane] == lane {
				sourceLanes = append(sourceLanes, sourceLane)
			}
		}
		previousWidth := int64(1)<<53 - 1
		if len(sourceLanes) == 1 {
			previousWidth = getBoardLaneColumnCount(laneGeometry(previousGeometry), sourceLanes[0])
		}
		result, err := generateResponsiveGridFor(remappedElements, width, previousWidth, root)
		if err != nil {
			return updatedBoardLayout{}, err
		}
		updatedElements = append(updatedElements, result.Items...)
	}
	if updatedElements == nil {
		updatedElements = []gridItem{}
	}
	updatedElementByID := map[string]gridItem{}
	for _, element := range updatedElements {
		updatedElementByID[element.ID] = element
	}

	out := updatedBoardLayout{ResizeSource: serializeLayoutResizeSource(source, opts.CurrentGeometry, updatedElements)}
	for _, item := range board.Items {
		currentElement, ok := updatedElementByID[item.ID]
		if !ok || currentElement.Type != "item" {
			continue
		}
		out.ItemSectionLayouts = append(out.ItemSectionLayouts, projectedItemLayout{
			ItemID: item.ID, LayoutID: opts.CurrentGeometry.LayoutID,
			SectionID: currentElement.SectionID,
			XOffset:   currentElement.XOffset, YOffset: currentElement.YOffset,
			Width: currentElement.Width, Height: currentElement.Height,
		})
	}
	for _, sec := range board.Sections {
		if sec.Kind != "container" {
			continue
		}
		currentElement, ok := updatedElementByID[sec.ID]
		if !ok || currentElement.Type != "section" {
			continue
		}
		out.SectionLayouts = append(out.SectionLayouts, projectedSectionLayout{
			SectionID: sec.ID, LayoutID: opts.CurrentGeometry.LayoutID,
			ParentSectionID: currentElement.SectionID,
			XOffset:         currentElement.XOffset, YOffset: currentElement.YOffset,
			Width: currentElement.Width, Height: currentElement.Height,
		})
	}
	return out, nil
}

// getElementsForProjectedLayout ports getElementsForProjectedLayout
// (board.ts:2717-2744): the projected rows as grid items, so follow-up
// projections chain from the resized arrangement.
func getElementsForProjectedLayout(projected updatedBoardLayout) []gridItem {
	elements := []gridItem{}
	for _, layout := range projected.ItemSectionLayouts {
		elements = append(elements, gridItem{
			ID: layout.ItemID, Type: "item",
			Height: layout.Height, Width: layout.Width,
			XOffset: layout.XOffset, YOffset: layout.YOffset,
			SectionID: layout.SectionID,
		})
	}
	for _, layout := range projected.SectionLayouts {
		if layout.ParentSectionID == "" {
			continue
		}
		elements = append(elements, gridItem{
			ID: layout.SectionID, Type: "section",
			Height: layout.Height, Width: layout.Width,
			XOffset: layout.XOffset, YOffset: layout.YOffset,
			SectionID: layout.ParentSectionID,
		})
	}
	return elements
}
