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

// repairProtectedLayouts seeds the missing protected layouts. The upstream
// projections of item/section positions onto the new mobile layout
// (insertMissingProjectedPositionsAsync, seed.ts:181+) rely on the
// responsive grid algorithm of @homarr/common and are deferred; newly seeded
// layouts start empty, like a fresh board.
func (s *service) repairProtectedLayouts(ctx context.Context, boardID string, layouts []db.Layout) *trpc.Error {
	if len(layouts) == 0 {
		// seed.ts:109-129: insert the Mobile + Base pair.
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
		// seed.ts:131-153: the single layout becomes Base (breakpoint 768) and
		// a fresh Mobile layout is added.
		if err := s.db.UpdateLayoutRoleAndBreakpoint(ctx, layouts[0].ID, "base", 768); err != nil {
			return internalError(err)
		}
		if err := s.db.InsertLayout(ctx, db.Layout{
			ID: createID(), Name: "Mobile", BoardID: boardID, ColumnCount: 3, Breakpoint: 0, Role: "mobile",
		}); err != nil {
			return internalError(err)
		}
		return nil
	}

	// More than two layouts: normalize roles and breakpoints
	// (seed.ts:155-172) and persist the rows whose values changed.
	normalized := normalizeBoardLayoutRoles(layouts)
	for _, layout := range normalized {
		if layout.Breakpoint == layout.storedBreakpoint && layout.Role == layout.storedRole {
			continue
		}
		if err := s.db.UpdateLayoutRoleAndBreakpoint(ctx, layout.ID, layout.Role, layout.Breakpoint); err != nil {
			return internalError(err)
		}
	}
	return nil
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
