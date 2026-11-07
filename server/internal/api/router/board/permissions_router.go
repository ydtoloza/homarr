package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Board permission management procedures
// (board.getBoardPermissions / saveUserBoardPermissions /
// saveGroupBoardPermissions, packages/api/src/router/board.ts:2123-2274).

// groupPermissionOutput is one inherited permission row
// ({permission, group: {id, name}}).
type groupPermissionOutput struct {
	Permission string        `json:"permission"`
	Group      groupIDOutput `json:"group"`
}

type groupIDOutput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// userPermissionOutput is one direct user permission row
// ({user: {id, name, image, email}, permission}).
type userPermissionOutput struct {
	User       userColumnsOutput `json:"user"`
	Permission string            `json:"permission"`
}

type userColumnsOutput struct {
	ID    string  `json:"id"`
	Name  *string `json:"name"`
	Image *string `json:"image"`
	Email *string `json:"email"`
}

// boardGetBoardPermissions is board.getBoardPermissions (board.ts:2123-2194).
func (s *service) boardGetBoardPermissions(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
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

	// Inherited group permissions: every group permission row whose key is
	// board-view-all, board-modify-all, board-full-all or their parent
	// board-create (getPermissionsWithParents of the three board keys,
	// board.ts:2126-2142).
	parents := expandPermissions([]string{"board-view-all", "board-modify-all", "board-full-all"})
	inheritedKeys := make([]string, 0, len(parents))
	for key := range parents {
		inheritedKeys = append(inheritedKeys, key)
	}
	sort.Strings(inheritedKeys)

	groups, err := s.listAllGroups(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	groupPerms, err := s.db.ListGroupPermissionsForGroups(ctx, groupIDs(groups))
	if err != nil {
		return nil, internalError(err)
	}
	groupNameByID := map[string]string{}
	for _, g := range groups {
		groupNameByID[g.ID] = g.Name
	}
	inherited := []groupPermissionOutput{}
	for _, row := range groupPerms {
		if !containsString(inheritedKeys, row.Permission) {
			continue
		}
		inherited = append(inherited, groupPermissionOutput{
			Permission: row.Permission,
			Group:      groupIDOutput{ID: row.GroupID, Name: groupNameByID[row.GroupID]},
		})
	}
	sort.SliceStable(inherited, func(i, j int) bool {
		return inherited[i].Group.Name < inherited[j].Group.Name
	})

	// Direct user permissions (board.ts:2144-2156).
	userRows, err := s.db.ListBoardUserPermissionsForBoardWithUser(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	users := []userPermissionOutput{}
	for _, row := range userRows {
		users = append(users, userPermissionOutput{
			User: userColumnsOutput{
				ID:    row.UserID,
				Name:  nullStrOr(row.UserName),
				Image: nullStrOr(row.UserImage),
				Email: nullStrOr(row.UserEmail),
			},
			Permission: row.Permission,
		})
	}
	sort.SliceStable(users, func(i, j int) bool {
		a, b := "", ""
		if users[i].User.Name != nil {
			a = *users[i].User.Name
		}
		if users[j].User.Name != nil {
			b = *users[j].User.Name
		}
		return a < b
	})

	// Direct group permissions (board.ts:2158-2168).
	groupRows, err := s.db.ListBoardGroupPermissionsForBoardWithGroup(ctx, id)
	if err != nil {
		return nil, internalError(err)
	}
	boardGroups := []groupPermissionOutput{}
	for _, row := range groupRows {
		boardGroups = append(boardGroups, groupPermissionOutput{
			Permission: row.Permission,
			Group:      groupIDOutput{ID: row.GroupID, Name: row.GroupName},
		})
	}
	sort.SliceStable(boardGroups, func(i, j int) bool {
		return boardGroups[i].Group.Name < boardGroups[j].Group.Name
	})

	return map[string]any{
		"inherited": inherited,
		"users":     users,
		"groups":    boardGroups,
	}, nil
}

func nullStrOr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// boardSaveUserBoardPermissions is board.saveUserBoardPermissions
// (board.ts:2195-2233): replaces the direct user permissions of a board.
func (s *service) boardSaveUserBoardPermissions(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	entityID, permissions, terr := s.parseSavePermissionsInput(input)
	if terr != nil {
		return nil, terr
	}
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, entityID, c, permFull); terr != nil {
		return nil, terr
	}
	rows := make([]db.BoardUserPermission, 0, len(permissions))
	for _, p := range permissions {
		rows = append(rows, db.BoardUserPermission{BoardID: entityID, UserID: p.principalID, Permission: p.permission})
	}
	if err := s.db.ReplaceBoardUserPermissions(ctx, entityID, rows); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// boardSaveGroupBoardPermissions is board.saveGroupBoardPermissions
// (board.ts:2234-2274): replaces the group permissions of a board.
func (s *service) boardSaveGroupBoardPermissions(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	entityID, permissions, terr := s.parseSavePermissionsInput(input)
	if terr != nil {
		return nil, terr
	}
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	if terr := s.requireBoardAccess(ctx, entityID, c, permFull); terr != nil {
		return nil, terr
	}
	rows := make([]db.BoardGroupPermission, 0, len(permissions))
	for _, p := range permissions {
		rows = append(rows, db.BoardGroupPermission{BoardID: entityID, GroupID: p.principalID, Permission: p.permission})
	}
	if err := s.db.ReplaceBoardGroupPermissions(ctx, entityID, rows); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}

// principalPermission is one entry of boardSavePermissionsSchema
// (packages/validation/src/permissions.ts:3-15).
type principalPermission struct {
	principalID string
	permission  string
}

// parseSavePermissionsInput validates {entityId, permissions: [{principalId,
// permission}]} with the board permission enum.
func (s *service) parseSavePermissionsInput(input json.RawMessage) (string, []principalPermission, *trpc.Error) {
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return "", nil, terr
	}
	entityID, terr := dec.str("entityId")
	if terr != nil {
		return "", nil, terr
	}
	permissionsRaw, ok := dec.obj["permissions"]
	if !ok || permissionsRaw == nil {
		return "", nil, badRequestf(`expected permissions array`)
	}
	list, ok := permissionsRaw.([]any)
	if !ok {
		return "", nil, badRequestf("permissions must be an array")
	}
	permissions := make([]principalPermission, 0, len(list))
	for _, element := range list {
		entry, ok := element.(map[string]any)
		if !ok {
			return "", nil, badRequestf("invalid permission entry")
		}
		entryDec := &inputDecoder{obj: entry}
		principalID, terr := entryDec.str("principalId")
		if terr != nil {
			return "", nil, terr
		}
		permission, terr := entryDec.enum("permission", boardPermissionValues)
		if terr != nil {
			return "", nil, terr
		}
		permissions = append(permissions, principalPermission{principalID: principalID, permission: permission})
	}
	return entityID, permissions, nil
}

// --- minimal group reads ------------------------------------------------------

// groupMinimal is the {id, name} pair of a group.
type groupMinimal struct {
	ID   string
	Name string
}

// ListAllGroupsMinimal returns every group (the getBoardPermissions
// inherited-permission query iterates all groups' permissions).
func (s *service) listAllGroups(ctx context.Context) ([]groupMinimal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM "group"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []groupMinimal{}
	for rows.Next() {
		var g groupMinimal
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func groupIDs(groups []groupMinimal) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.ID
	}
	return out
}
