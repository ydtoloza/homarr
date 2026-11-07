package board

import (
	"context"
	"sort"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Protected layout repair: boards created before the layouts migration can
// lack the Mobile/Base pair or carry duplicate breakpoints. Upstream seeds
// them lazily on every full-board read
// (seedProtectedBoardLayoutsAsync, packages/db/migrations/seed.ts:98-190,
// guarded by boardLayoutsNeedRepair, packages/api/src/router/board.ts:3062-3074).

// boardLayoutsNeedRepair is boardLayoutsNeedRepair (board.ts:3062-3074):
// exactly one mobile layout at breakpoint 0, exactly one base layout, and
// unique breakpoints.
func boardLayoutsNeedRepair(layouts []db.Layout) bool {
	mobileCount, baseCount := 0, 0
	var mobileBreakpoint int64 = -1
	seenBreakpoints := map[int64]bool{}
	for _, l := range layouts {
		if l.Role == "mobile" {
			mobileCount++
			mobileBreakpoint = l.Breakpoint
		}
		if l.Role == "base" {
			baseCount++
		}
		seenBreakpoints[l.Breakpoint] = true
	}
	return mobileCount != 1 || baseCount != 1 || mobileBreakpoint != 0 || len(seenBreakpoints) != len(layouts)
}

// repairProtectedLayouts seeds the missing protected layouts and projects the
// item/section positions of the Base layout onto the (new) Mobile layout
// with insertMissingProjectedPositionsAsync
// (packages/db/migrations/seed.ts:98-190).
func (s *service) repairProtectedLayouts(ctx context.Context, boardID string, layouts []db.Layout) *trpc.Error {
	if len(layouts) == 0 {
		// seed.ts:109-129: insert the Mobile + Base pair; the fresh pair does
		// not project positions (continue in seed.ts:128).
		if err := s.db.InsertLayout(ctx, db.Layout{
			ID: createID(), Name: "Mobile", BoardID: boardID, ColumnCount: 3, Breakpoint: 0, Role: "mobile",
		}); err != nil {
			return internalError(err)
		}
		if err := s.db.InsertLayout(ctx, db.Layout{
			ID: createID(), Name: "Base", BoardID: boardID, ColumnCount: 10, Breakpoint: 768, Role: "base",
		}); err != nil {
			return internalError(err)
		}
		return nil
	}
	if len(layouts) == 1 {
		// seed.ts:131-153: the single layout becomes Base (breakpoint 768), a
		// fresh Mobile layout is added and receives the projected positions.
		if err := s.db.UpdateLayoutRoleAndBreakpoint(ctx, layouts[0].ID, "base", 768); err != nil {
			return internalError(err)
		}
		mobile := db.Layout{
			ID: createID(), Name: "Mobile", BoardID: boardID, ColumnCount: 3, Breakpoint: 0, Role: "mobile",
		}
		if err := s.db.InsertLayout(ctx, mobile); err != nil {
			return internalError(err)
		}
		base := layouts[0]
		base.Role, base.Breakpoint = "base", 768
		return s.insertMissingProjectedPositions(ctx, boardID, base, mobile)
	}

	// More than two layouts: normalize roles and breakpoints
	// (seed.ts:155-172), persist the rows whose values changed and project
	// Base onto Mobile (seed.ts:173-179).
	normalized := normalizeBoardLayoutRoles(layouts)
	for _, layout := range normalized {
		if layout.Breakpoint == layout.storedBreakpoint && layout.Role == layout.storedRole {
			continue
		}
		if err := s.db.UpdateLayoutRoleAndBreakpoint(ctx, layout.ID, layout.Role, layout.Breakpoint); err != nil {
			return internalError(err)
		}
	}
	var mobile, base db.Layout
	for _, layout := range normalized {
		if layout.Role == "mobile" {
			mobile = layout.Layout
		}
		if layout.Role == "base" {
			base = layout.Layout
		}
	}
	return s.insertMissingProjectedPositions(ctx, boardID, base, mobile)
}

// insertMissingProjectedPositions ports insertMissingProjectedPositionsAsync
// (packages/db/migrations/seed.ts:209-303): projects every canvas root of the
// source layout onto the target layout and inserts the placements that do not
// exist there yet. Unlike getUpdatedBoardLayout this runs on the raw rows (no
// lane remapping, no parseItem filtering) and leaves resize sources alone.
func (s *service) insertMissingProjectedPositions(ctx context.Context, boardID string, sourceLayout, targetLayout db.Layout) *trpc.Error {
	itemRows, err := s.db.ListItemsByBoard(ctx, boardID)
	if err != nil {
		return internalError(err)
	}
	itemLayoutRows, err := s.db.ListItemLayoutsByBoard(ctx, boardID)
	if err != nil {
		return internalError(err)
	}
	sectionRows, err := s.db.ListSectionsByBoard(ctx, boardID)
	if err != nil {
		return internalError(err)
	}
	sectionLayoutRows, err := s.db.ListSectionLayoutsByBoard(ctx, boardID)
	if err != nil {
		return internalError(err)
	}

	// The elements of the source layout (seed.ts:215-249): the first
	// placement per item, then containers with a parent.
	elements := []gridItem{}
	for _, item := range itemRows {
		for _, l := range itemLayoutRows {
			if l.ItemID == item.ID && l.LayoutID == sourceLayout.ID {
				elements = append(elements, gridItem{
					ID: item.ID, Type: "item",
					Width: l.Width, Height: l.Height,
					XOffset: l.XOffset, YOffset: l.YOffset,
					SectionID: l.SectionID,
				})
				break
			}
		}
	}
	for _, sec := range sectionRows {
		if sec.Kind != "container" {
			continue
		}
		for _, sl := range sectionLayoutRows {
			if sl.SectionID == sec.ID && sl.LayoutID == sourceLayout.ID && sl.ParentSectionID != "" {
				elements = append(elements, gridItem{
					ID: sec.ID, Type: "section",
					Width: sl.Width, Height: sl.Height,
					XOffset: sl.XOffset, YOffset: sl.YOffset,
					SectionID: sl.ParentSectionID,
				})
				break
			}
		}
	}

	// One projection per canvas root, using the raw column counts
	// (seed.ts:251-261).
	projected := []gridItem{}
	for _, sec := range sectionRows {
		if sec.Kind != "empty" {
			continue
		}
		result, err := generateResponsiveGridFor(elements, targetLayout.ColumnCount, sourceLayout.ColumnCount, sec.ID)
		if err != nil {
			return internalError(err)
		}
		projected = append(projected, result.Items...)
	}

	// Only missing placements are inserted (seed.ts:263-302).
	itemHasTargetPlacement := map[string]bool{}
	for _, l := range itemLayoutRows {
		if l.LayoutID == targetLayout.ID {
			itemHasTargetPlacement[l.ItemID] = true
		}
	}
	sectionHasTargetPlacement := map[string]bool{}
	for _, sl := range sectionLayoutRows {
		if sl.LayoutID == targetLayout.ID {
			sectionHasTargetPlacement[sl.SectionID] = true
		}
	}
	for _, element := range projected {
		switch element.Type {
		case "item":
			if itemHasTargetPlacement[element.ID] {
				continue
			}
			if err := s.db.InsertItemLayout(ctx, db.ItemLayout{
				ItemID: element.ID, LayoutID: targetLayout.ID, SectionID: element.SectionID,
				XOffset: element.XOffset, YOffset: element.YOffset,
				Width: element.Width, Height: element.Height,
			}); err != nil {
				return internalError(err)
			}
		case "section":
			if sectionHasTargetPlacement[element.ID] {
				continue
			}
			if err := s.db.InsertSectionLayout(ctx, db.SectionLayout{
				SectionID: element.ID, LayoutID: targetLayout.ID, ParentSectionID: element.SectionID,
				XOffset: element.XOffset, YOffset: element.YOffset,
				Width: element.Width, Height: element.Height,
			}); err != nil {
				return internalError(err)
			}
		}
	}
	return nil
}

// loadBoardForProjection assembles the boardForLayoutProjection input of
// getFullBoardWithWhereAsync (board.ts:2941-3060), including the lazy repair
// of protected layouts every full-board read performs
// (board.ts:3001-3011) and the parseSection/parseItem preprocessing the full
// board applies to the rows.
func (s *service) loadBoardForProjection(ctx context.Context, boardID string) (*boardForLayoutProjection, *trpc.Error) {
	layoutRows, err := s.db.ListLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	if boardLayoutsNeedRepair(layoutRows) {
		if terr := s.repairProtectedLayouts(ctx, boardID, layoutRows); terr != nil {
			return nil, terr
		}
		if layoutRows, err = s.db.ListLayoutsByBoard(ctx, boardID); err != nil {
			return nil, internalError(err)
		}
	}
	db.SortLayoutsByBreakpoint(layoutRows)

	sectionRows, err := s.db.ListSectionsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	itemRows, err := s.db.ListItemsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	if terr := applyParsedBoardRows(sectionRows, itemRows); terr != nil {
		return nil, terr
	}
	sectionLayoutRows, err := s.db.ListSectionLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	itemLayoutRows, err := s.db.ListItemLayoutsByBoard(ctx, boardID)
	if err != nil {
		return nil, internalError(err)
	}
	return newBoardForLayoutProjection(boardID, layoutRows, sectionRows, sectionLayoutRows, itemRows, itemLayoutRows), nil
}

// normalizedLayout is a db.Layout plus the values persisted before
// normalization (for the change detection of seed.ts:156-159).
type normalizedLayout struct {
	db.Layout
	storedBreakpoint int64
	storedRole       string
}

// normalizeBoardLayoutRoles ports normalizeBoardLayoutRoles
// (packages/definitions/src/board.ts:38-66): sorts by breakpoint and, when
// the mobile/base pair is not uniquely present, assigns the first layout the
// mobile role and the last one the base role, recomputing breakpoints so
// they stay strictly increasing and unique.
func normalizeBoardLayoutRoles(layouts []db.Layout) []normalizedLayout {
	out := make([]normalizedLayout, len(layouts))
	for i, l := range layouts {
		out[i] = normalizedLayout{Layout: l, storedBreakpoint: l.Breakpoint, storedRole: l.Role}
	}

	var mobiles, bases []int
	for i, l := range layouts {
		switch l.Role {
		case "mobile":
			mobiles = append(mobiles, i)
		case "base":
			bases = append(bases, i)
		}
	}
	hasProtectedLayouts := len(mobiles) == 1 && len(bases) == 1

	less := func(a, b normalizedLayout) bool {
		if a.Breakpoint != b.Breakpoint {
			return a.Breakpoint < b.Breakpoint
		}
		if a.ColumnCount != b.ColumnCount {
			return a.ColumnCount < b.ColumnCount
		}
		return a.ID < b.ID
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })

	if hasProtectedLayouts {
		mobile := out[mobiles[0]]
		reordered := []normalizedLayout{mobile}
		for _, l := range out {
			if l.ID != mobile.ID {
				reordered = append(reordered, l)
			}
		}
		out = reordered
	}

	const maxBreakpoint = 32767
	previousBreakpoint := int64(-1)
	for index := range out {
		remaining := int64(len(out) - index - 1)
		breakpoint := out[index].Breakpoint
		if index == 0 {
			breakpoint = 0
		} else {
			candidate := previousBreakpoint + 1
			if breakpoint < candidate {
				breakpoint = candidate
			}
			if breakpoint > maxBreakpoint-remaining {
				breakpoint = maxBreakpoint - remaining
			}
		}
		previousBreakpoint = breakpoint
		out[index].Breakpoint = breakpoint
		if !hasProtectedLayouts {
			switch index {
			case 0:
				out[index].Role = "mobile"
			case len(out) - 1:
				out[index].Role = "base"
			default:
				out[index].Role = "custom"
			}
		}
	}
	return out
}
