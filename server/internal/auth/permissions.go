package auth

import "slices"

// Go port of the global group-permission model in
// packages/definitions/src/permissions.ts: the raw permissions stored in the
// groupPermission table expand into inherited child permissions before they
// reach the client (getCurrentUserPermissionsAsync in
// packages/auth/callbacks.ts:10-21 calls getPermissionsWithChildren).

// permissionAdmin is the key that marks administrator access. Upstream checks
// session.user.permissions.includes("admin") (packages/api/src/trpc.ts:149).
const permissionAdmin = "admin"

// permissionParents mirrors groupPermissionParents
// (packages/definitions/src/permissions.ts:53-66): every listed permission
// implies its children.
var permissionParents = map[string][]string{
	"board-modify-all":         {"board-view-all"},
	"board-full-all":           {"board-modify-all", "board-create"},
	"app-modify-all":           {"app-create"},
	"app-full-all":             {"app-modify-all", "app-use-all"},
	"integration-interact-all": {"integration-use-all"},
	"integration-full-all":     {"integration-interact-all", "integration-create"},
	"search-engine-modify-all": {"search-engine-create"},
	"search-engine-full-all":   {"search-engine-modify-all"},
	"media-full-all":           {"media-upload", "media-view-all"},
	permissionAdmin: {
		"board-full-all",
		"app-full-all",
		"integration-full-all",
		"search-engine-full-all",
		"media-full-all",
		"other-view-logs",
	},
}

// expandPermissions ports getPermissionsWithChildren
// (packages/definitions/src/permissions.ts:97-101): each permission is
// resolved together with every permission it implies. The input order is not
// significant; the result is deduplicated and sorted for stable output.
func expandPermissions(permissions []string) []string {
	set := make(map[string]struct{}, len(permissions))
	var add func(keys []string)
	add = func(keys []string) {
		for _, key := range keys {
			if _, ok := set[key]; ok {
				continue
			}
			set[key] = struct{}{}
			if children, ok := permissionParents[key]; ok {
				add(children)
			}
		}
	}
	add(permissions)

	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
