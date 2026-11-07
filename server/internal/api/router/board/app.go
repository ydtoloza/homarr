package board

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// appRouter port (packages/api/src/router/app.ts). app.detectMetadata is
// deferred: it fetches the submitted website over the network and searches
// the icon database (fetchWebsiteMetadataAsync / getIconForName).

// appOutput is selectAppSchema (packages/db/validationSchemas.ts:7): the
// plain app row with nulls preserved.
type appOutput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	IconURL     string  `json:"iconUrl"`
	HREF        *string `json:"href"`
	PingURL     *string `json:"pingUrl"`
}

func appOutputOf(a db.App) appOutput {
	return appOutput{
		ID:          a.ID,
		Name:        a.Name,
		Description: nullableStr(a.Description),
		IconURL:     a.IconURL,
		HREF:        nullableStr(a.HREF),
		PingURL:     nullableStr(a.PingURL),
	}
}

// appList is an app array that serializes as [] instead of null.
func appList(apps []db.App) []appOutput {
	out := make([]appOutput, 0, len(apps))
	for _, a := range apps {
		out = append(out, appOutputOf(a))
	}
	return out
}

// appGetPaginated is app.getPaginated (app.ts:24-59).
func (s *service) appGetPaginated(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-modify-all"); terr != nil {
		return nil, terr
	}
	// paginatedSchema (packages/validation/src/common.ts): search optional,
	// pageSize default 10 (max 100), page default 1.
	search := ""
	pageSize, page := int64(10), int64(1)
	if len(input) > 0 && string(input) != "null" {
		dec, terr := newInputDecoder(input)
		if terr != nil {
			return nil, terr
		}
		if value, ok, terr := dec.optStr("search"); terr != nil {
			return nil, terr
		} else if ok {
			search = value
		}
		if dec.has("pageSize") {
			pageSize, terr = dec.int64("pageSize", 1, 100)
			if terr != nil {
				return nil, terr
			}
		}
		if dec.has("page") {
			page, terr = dec.int64("page", 1, 1<<31-1)
			if terr != nil {
				return nil, terr
			}
		}
	}
	nameLike := ""
	if strings.TrimSpace(search) != "" {
		nameLike = "%" + strings.TrimSpace(search) + "%"
	}
	totalCount, err := s.db.CountApps(ctx, nameLike)
	if err != nil {
		return nil, internalError(err)
	}
	apps, err := s.db.ListAppsPaginated(ctx, nameLike, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, internalError(err)
	}
	return map[string]any{
		"items":      appList(apps),
		"totalCount": int64(totalCount),
	}, nil
}

// appAll is app.all (app.ts:60-79).
func (s *service) appAll(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-modify-all"); terr != nil {
		return nil, terr
	}
	apps, err := s.db.ListAllApps(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	return appList(apps), nil
}

// appSearch is app.search (app.ts:80-119): app-modify-all OR
// board-modify-all, otherwise FORBIDDEN "Permission denied".
func (s *service) appSearch(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	if !c.hasPermission("app-modify-all") && !c.hasPermission("board-modify-all") {
		return nil, trpc.NewError(trpc.CodeForbidden, "Permission denied")
	}
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := dec.str("query")
	if terr != nil {
		return nil, terr
	}
	limit := int64(10)
	if dec.has("limit") {
		limit, terr = dec.int64("limit", 1, 100)
		if terr != nil {
			return nil, terr
		}
	}
	apps, err := s.db.SearchApps(ctx, query, limit)
	if err != nil {
		return nil, internalError(err)
	}
	return appList(apps), nil
}

// appSelectable is app.selectable (app.ts:120-157): signed-in callers get
// the selection columns of every app.
func (s *service) appSelectable(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requireSignedIn(c); terr != nil {
		return nil, terr
	}
	apps, err := s.db.ListSelectableApps(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	return appList(apps), nil
}

// appByID is app.byId (app.ts:158-185) with the AppAccessControl rules
// (app/app-access-control.ts): signed-in callers see every app, anonymous
// callers only apps placed on public boards.
func (s *service) appByID(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	// byIdSchema: {id}.
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := dec.str("id")
	if terr != nil {
		return nil, terr
	}
	apps, terr := s.visibleApps(ctx, c, []string{id})
	if terr != nil {
		return nil, terr
	}
	if len(apps) == 0 {
		return nil, trpc.NewError(trpc.CodeNotFound, "App not found")
	}
	return appOutputOf(apps[0]), nil
}

// appByIDs is app.byIds (app.ts:186-189): input is a plain string array.
func (s *service) appByIDs(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	raw, terr := unwrapSuperJSON(input, "input")
	if terr != nil {
		return nil, terr
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, badRequestf("expected an array of app ids")
	}
	apps, terr := s.visibleApps(ctx, c, ids)
	if terr != nil {
		return nil, terr
	}
	return appList(apps), nil
}

// visibleApps is AppRepository.getByIdsAsync (app.ts:349-356).
func (s *service) visibleApps(ctx context.Context, c caller, ids []string) ([]db.App, *trpc.Error) {
	canSee, terr := s.canUserSeeApps(ctx, c, ids)
	if terr != nil {
		return nil, terr
	}
	if !canSee {
		return nil, nil
	}
	apps, err := s.db.ListAppsByIDs(ctx, ids)
	if err != nil {
		return nil, internalError(err)
	}
	return apps, nil
}

// canUserSeeApps is AppAccessControl.canUserSeeAppsAsync (app-access-control.ts:20-46):
// any signed-in user sees all apps; anonymous callers only see apps on
// public boards.
func (s *service) canUserSeeApps(ctx context.Context, c caller, appIDs []string) (bool, *trpc.Error) {
	if c.session != nil {
		return true, nil
	}
	publicAppIDs, terr := s.appIDsOnPublicBoards(ctx)
	if terr != nil {
		return false, terr
	}
	for _, id := range appIDs {
		if !containsString(publicAppIDs, id) {
			return false, nil
		}
	}
	return true, nil
}

// appIDsOnPublicBoards is getAllAppIdsOnPublicBoardsAsync: "app" items
// contribute their options.appId, "bookmarks" items their legacy options
// .items entries.
func (s *service) appIDsOnPublicBoards(ctx context.Context) ([]string, *trpc.Error) {
	items, err := s.db.ListAppOptionsOnPublicBoards(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	ids := []string{}
	for _, item := range items {
		raw, terr := unwrapSuperJSON(json.RawMessage(item.Options), "item options")
		if terr != nil {
			continue
		}
		var options map[string]any
		if json.Unmarshal(raw, &options) != nil || options == nil {
			continue
		}
		if item.Kind == "app" {
			if appID, ok := options["appId"].(string); ok {
				ids = append(ids, appID)
			}
			continue
		}
		// bookmarks: parsedOptions.items (legacy string list).
		if entries, ok := options["items"].([]any); ok {
			for _, entry := range entries {
				if appID, ok := entry.(string); ok {
					ids = append(ids, appID)
				}
			}
		}
	}
	return ids, nil
}

// appCreate is app.create (app.ts:190-225).
func (s *service) appCreate(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-create"); terr != nil {
		return nil, terr
	}
	values, terr := parseAppManageInput(input, false)
	if terr != nil {
		return nil, terr
	}
	id := createID()
	app := db.App{ID: id, Name: values.name, Description: values.description, IconURL: values.iconURL,
		HREF: values.href, PingURL: values.pingURL}
	if err := s.db.InsertApp(ctx, app); err != nil {
		return nil, internalError(err)
	}
	// The legacy appId member is still returned upstream (app.ts:224).
	return map[string]any{
		"appId":       id,
		"id":          id,
		"name":        app.Name,
		"description": nullableStr(app.Description),
		"iconUrl":     app.IconURL,
		"href":        nullableStr(app.HREF),
		"pingUrl":     nullableStr(app.PingURL),
	}, nil
}

// parsedAppInput is the validated appManageSchema payload.
type parsedAppInput struct {
	name        string
	description string // empty = NULL
	iconURL     string
	href        string // empty = NULL
	pingURL     string // empty = NULL
}

// parseAppManageInput validates appManageSchema
// (packages/validation/src/app.ts:7-35): trimmed strings, "" transformed to
// null, pingUrl restricted to http(s) URLs.
func parseAppManageInput(input json.RawMessage, iconURLOptional bool) (parsedAppInput, *trpc.Error) {
	dec, terr := newInputDecoder(input)
	if terr != nil {
		return parsedAppInput{}, terr
	}
	name, terr := dec.str("name")
	if terr != nil {
		return parsedAppInput{}, terr
	}
	name = trimSpace(name)
	if len(name) < 1 || len(name) > 64 {
		return parsedAppInput{}, badRequestf("name must be 1-64 characters")
	}
	out := parsedAppInput{name: name}

	if dec.has("description") {
		value, ok, terr := dec.optStr("description")
		if terr != nil {
			return parsedAppInput{}, terr
		}
		if ok {
			trimmed := trimSpace(value)
			if len(trimmed) > 512 {
				return parsedAppInput{}, badRequestf("description must be at most 512 characters")
			}
			out.description = trimmed
		}
	}
	if iconURLOptional {
		// appCreateManySchema swaps the managed iconUrl for
		// z.string().min(1).nullable(): no trimming, null falls back to the
		// icon search (app.ts:251).
		value, ok, terr := dec.optStr("iconUrl")
		if terr != nil {
			return parsedAppInput{}, terr
		}
		if ok {
			if len(value) < 1 {
				return parsedAppInput{}, badRequestf("iconUrl must not be empty")
			}
			out.iconURL = value
		}
	} else {
		value, terr := dec.str("iconUrl")
		if terr != nil {
			return parsedAppInput{}, terr
		}
		out.iconURL = trimSpace(value)
		if len(out.iconURL) < 1 {
			return parsedAppInput{}, badRequestf("iconUrl must not be empty")
		}
	}

	if dec.has("href") {
		value, ok, terr := dec.optStr("href")
		if terr != nil {
			return parsedAppInput{}, terr
		}
		if ok {
			out.href = value
		}
	}
	if dec.has("pingUrl") {
		value, ok, terr := dec.optStr("pingUrl")
		if terr != nil {
			return parsedAppInput{}, terr
		}
		if ok {
			trimmed := trimSpace(value)
			if trimmed != "" && !isHTTPOrHTTPSURL(trimmed) {
				return parsedAppInput{}, badRequestf("pingUrl must be an http(s) URL")
			}
			out.pingURL = trimmed
		}
	}
	return out, nil
}

// isHTTPOrHTTPSURL checks the pingUrl constraint: a parseable URL with an
// http/https scheme and a host (appManageSchema:
// .url().regex(/^https?:\/\//)).
func isHTTPOrHTTPSURL(v string) bool {
	parsed, err := url.Parse(v)
	if err != nil || parsed.Host == "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

// appCreateMany is app.createMany (app.ts:241-255): the icon falls back to
// the icon database and then to the Homarr dashboard icon.
func (s *service) appCreateMany(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-create"); terr != nil {
		return nil, terr
	}
	raw, terr := unwrapSuperJSON(input, "input")
	if terr != nil {
		return nil, terr
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, badRequestf("expected an array of apps")
	}
	if len(entries) < 1 {
		return nil, badRequestf("at least one app is required")
	}
	const defaultIcon = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons@master/svg/homarr.svg"
	for _, entry := range entries {
		values, terr := parseAppManageInput(entry, true)
		if terr != nil {
			return nil, terr
		}
		if values.iconURL == "" {
			if iconURL, err := s.db.FindIconURLByName(ctx, values.name); err == nil {
				values.iconURL = iconURL
			}
			if values.iconURL == "" {
				values.iconURL = defaultIcon
			}
		}
		if err := s.db.InsertApp(ctx, db.App{
			ID: createID(), Name: values.name, Description: values.description,
			IconURL: values.iconURL, HREF: values.href,
		}); err != nil {
			return nil, internalError(err)
		}
	}
	return nil, nil
}

// appUpdate is app.update (app.ts:256-306).
func (s *service) appUpdate(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-modify-all"); terr != nil {
		return nil, terr
	}
	values, terr := parseAppManageInput(input, false)
	if terr != nil {
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
	app, err := s.db.GetAppByID(ctx, id)
	if err != nil {
		return nil, trpc.NewError(trpc.CodeNotFound, "App not found")
	}
	if err := s.db.UpdateApp(ctx, db.App{
		ID: id, Name: values.name, Description: values.description,
		IconURL: values.iconURL, HREF: values.href, PingURL: values.pingURL,
	}); err != nil {
		return nil, internalError(err)
	}
	// Upstream invalidates the response cache of integrations linked to the
	// app when href changes (app.ts:299-305); integration response caching is
	// not part of this milestone, so there is nothing to invalidate.
	_ = app
	return nil, nil
}

// appDelete is app.delete (app.ts:307-329): no NOT_FOUND check upstream.
func (s *service) appDelete(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := s.resolveCaller(ctx)
	if terr != nil {
		return nil, terr
	}
	if terr := requirePermission(c, "app-full-all"); terr != nil {
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
	if err := s.db.DeleteApp(ctx, id); err != nil {
		return nil, internalError(err)
	}
	return nil, nil
}
