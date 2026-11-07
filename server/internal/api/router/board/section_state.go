package board

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// sectionRouter and userItemStateRouter ports.

// sectionChangeCollapsed is section.changeCollapsed
// (packages/api/src/router/section/section-router.ts:13-71). Only the
// protectedProcedure gate applies: any signed-in user may collapse its own
// view of a collapsible container, mirroring upstream (no board permission
// check).
func (s *service) sectionChangeCollapsed(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	sectionID, terr := dec.str("sectionId")
	if terr != nil {
		return nil, terr
	}
	collapsed, terr := dec.bool("collapsed")
	if terr != nil {
		return nil, terr
	}

	section, err := s.db.GetSectionByID(ctx, sectionID)
	if err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "Section not found id="+sectionID)
	}
	if section.Kind != "container" {
		return nil, badRequestf("Section cannot be collapsed id=%s", sectionID)
	}

	// containerSectionOptionsSchema.collapsible
	// (section-router.ts:44-52): parse failures and missing collapsible both
	// reject.
	options, terr := parseContainerSectionOptions(section.Options)
	if terr != nil {
		return nil, terr
	}
	collapsible, ok := options["collapsible"].(bool)
	if !ok || !collapsible {
		return nil, badRequestf("Section cannot be collapsed id=%s", sectionID)
	}

	hasState, err := s.db.HasSectionCollapseState(ctx, section.ID, c.session.UserID)
	if err != nil {
		return nil, internalError(err)
	}
	if !hasState {
		if err := s.db.InsertSectionCollapseState(ctx, db.SectionCollapseState{
			SectionID: section.ID, UserID: c.session.UserID, Collapsed: collapsed,
		}); err != nil {
			return nil, internalError(err)
		}
		return nil, nil
	}
	if err := s.db.UpdateSectionCollapseState(ctx, db.SectionCollapseState{
		SectionID: section.ID, UserID: c.session.UserID, Collapsed: collapsed,
	}); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// --- userItemState ------------------------------------------------------------

// userItemStateGet is userItemState.get (packages/api/src/router/
// user-item-state.ts:23-30): the item's state keys as an object.
func (s *service) userItemStateGet(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	itemID, terr := s.requireItemStateAccess(ctx, c, input)
	if terr != nil {
		return nil, terr
	}
	states, err := s.db.ListUserItemStates(ctx, c.session.UserID, itemID)
	if err != nil {
		return nil, internalError(err)
	}
	// Object.fromEntries: every key maps to its raw JSON value string.
	out := map[string]json.RawMessage{}
	for _, state := range states {
		out[state.Key] = json.RawMessage(state.Value)
	}
	return out, nil
}

// userItemStateSet is userItemState.set (packages/api/src/router/
// user-item-state.ts:31-75).
func (s *service) userItemStateSet(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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
	itemID, terr := s.requireItemStateAccess(ctx, c, input)
	if terr != nil {
		return nil, terr
	}
	key, terr := dec.str("key")
	if terr != nil {
		return nil, terr
	}
	if len(key) < 1 || len(key) > 255 {
		return nil, badRequestf("key must be 1-255 characters")
	}
	value, terr := dec.str("value")
	if terr != nil {
		return nil, terr
	}
	if len(value) > 4096 {
		return nil, badRequestf("value must be at most 4096 characters")
	}
	if !json.Valid([]byte(value)) {
		return nil, badRequestf("Value must be JSON")
	}

	// The 500-key cap only applies to NEW keys
	// (user-item-state.ts:53-66).
	exists, err := s.db.HasUserItemState(ctx, c.session.UserID, itemID, key)
	if err != nil {
		return nil, internalError(err)
	}
	if !exists {
		count, err := s.db.CountUserItemStates(ctx, c.session.UserID, itemID)
		if err != nil {
			return nil, internalError(err)
		}
		if count >= 500 {
			return nil, badRequestf("Each user can save at most 500 state keys per item. Existing keys can still be updated.")
		}
	}

	if err := s.db.UpsertUserItemState(ctx, db.UserItemState{
		UserID: c.session.UserID, ItemID: itemID, Key: key, Value: value,
	}); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// requireItemStateAccess is the itemStateProcedure middleware
// (user-item-state.ts:12-20): the item must exist and the caller needs view
// access to its board; both failures are NOT_FOUND.
func (s *service) requireItemStateAccess(ctx context.Context, c caller, input json.RawMessage) (string, *trpc.Error) {
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return "", terr
	}
	itemID, terr := dec.str("itemId")
	if terr != nil {
		return "", terr
	}
	if len(itemID) < 1 || len(itemID) > 64 {
		return "", badRequestf("itemId must be 1-64 characters")
	}
	item, err := s.db.GetItemByID(ctx, itemID)
	if err != nil {
		return "", trpc.NewError(trpc.CodeNotFound, "Item not found")
	}
	if terr := s.requireBoardAccess(ctx, item.BoardID, c, permView); terr != nil {
		return "", terr
	}
	return itemID, nil
}
