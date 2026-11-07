// Package searchengine is the Go port of the searchEngine tRPC router
// (packages/api/src/router/search-engine/search-engine-router.ts): CRUD of
// the search_engine table plus the public catalog/search used by the
// spotlight search bar.
//
// Permission model (upstream middlewares): reads are protectedProcedure
// except getDefaultSearchEngine / catalog / search (publicProcedure, with
// anonymous callers restricted to generic engines), create requires
// search-engine-create, update search-engine-modify-all and delete
// search-engine-full-all.
package searchengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options configures the searchEngine router. The zero value is valid.
type Options struct{}

type service struct {
	db *db.DB
}

// jsonNull is the explicit JS `return null` of getDefaultSearchEngine (a
// void output would be undefined, which the trpc layer renders as {}).
var jsonNull = json.RawMessage("null")

// Register registers the searchEngine router under its upstream paths.
func Register(r *trpc.Router, database *db.DB, _ Options) {
	s := &service{db: database}
	// searchEngine.getPaginated (search-engine-router.ts:17-31).
	r.Register("searchEngine.getPaginated", s.getPaginated)
	// searchEngine.getSelectable (search-engine-router.ts:32-45).
	r.Register("searchEngine.getSelectable", s.getSelectable)
	// searchEngine.byId (search-engine-router.ts:47-72).
	r.Register("searchEngine.byId", s.byId)
	// searchEngine.getDefaultSearchEngine (search-engine-router.ts:73-135).
	r.Register("searchEngine.getDefaultSearchEngine", s.getDefaultSearchEngine)
	// searchEngine.catalog (search-engine-router.ts:136-157).
	r.Register("searchEngine.catalog", s.catalog)
	// searchEngine.search (search-engine-router.ts:158-178).
	r.Register("searchEngine.search", s.search)
	// searchEngine.create (search-engine-router.ts:179-193).
	r.Register("searchEngine.create", s.create)
	// searchEngine.update (search-engine-router.ts:194-220).
	r.Register("searchEngine.update", s.update)
	// searchEngine.delete (search-engine-router.ts:221-232).
	r.Register("searchEngine.delete", s.delete)
}

// --- output shapes -----------------------------------------------------------

// engineShape renders one search_engine row
// (selectSearchEnginesSchema): nulls preserved.
func engineShape(engine db.SearchEngine) map[string]any {
	return map[string]any{
		"id":            engine.ID,
		"iconUrl":       engine.IconURL,
		"name":          engine.Name,
		"short":         engine.Short,
		"description":   nullString(engine.Description),
		"urlTemplate":   nullString(engine.URLTemplate),
		"type":          engine.Type,
		"integrationId": nullString(engine.IntegrationID),
	}
}

// engineFullShape renders a row with the integration relation
// ({kind, url, id}) attached; the key is always present, null when the
// engine is not integration-backed.
func engineFullShape(engine db.SearchEngineFull) map[string]any {
	out := engineShape(engine.SearchEngine)
	if engine.Integration == nil {
		out["integration"] = nil
	} else {
		out["integration"] = map[string]any{
			"id":   engine.Integration.ID,
			"kind": engine.Integration.Kind,
			"url":  engine.Integration.URL,
		}
	}
	return out
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- handlers ----------------------------------------------------------------

// getPaginated is searchEngine.getPaginated: protectedProcedure,
// paginatedSchema ({search?, pageSize ≤ 100 default 10, page default 1}).
func (s *service) getPaginated(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	search, _, terr := d.OptStr("search")
	if terr != nil {
		return nil, terr
	}
	pageSize, _, terr := d.OptInt64("pageSize", 1, 100)
	if terr != nil {
		return nil, terr
	}
	if pageSize == 0 {
		pageSize = 10 // zod default
	}
	page, _, terr := d.OptInt64("page", 1, 1<<62)
	if terr != nil {
		return nil, terr
	}
	if page == 0 {
		page = 1 // zod default
	}

	namePattern := ""
	if search != "" {
		namePattern = "%" + strings.TrimSpace(search) + "%"
	}
	totalCount, err := s.db.CountSearchEngines(ctx, namePattern)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	engines, err := s.db.ListSearchEnginesPaginated(ctx, namePattern, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	items := make([]any, 0, len(engines))
	for _, engine := range engines {
		items = append(items, engineShape(engine))
	}
	return map[string]any{"items": items, "totalCount": totalCount}, nil
}

// getSelectable is searchEngine.getSelectable: protectedProcedure;
// {withIntegrations: boolean} with the whole object defaulting to
// {withIntegrations: true}. Output: [{value, label}] ordered by name.
func (s *service) getSelectable(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	withIntegrations := true // zod .default({withIntegrations: true})
	if len(input) > 0 && string(input) != "null" {
		d, terr := admin.NewInputDecoder(input)
		if terr != nil {
			return nil, terr
		}
		v, ok, terr := d.OptBool("withIntegrations")
		if terr != nil {
			return nil, terr
		}
		if ok {
			withIntegrations = v
		}
	}
	engines, err := s.db.ListSelectableSearchEngines(ctx, !withIntegrations)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(engines))
	for _, engine := range engines {
		out = append(out, map[string]any{"value": engine.ID, "label": engine.Name})
	}
	return out, nil
}

// byId is searchEngine.byId: protectedProcedure; NOT_FOUND
// "Search engine not found" for unknown ids. The type discriminator forces
// the relevant field non-null in the upstream output shape.
func (s *service) byId(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequireSignedIn(c); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	engine, err := s.db.GetSearchEngineByID(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "Search engine not found")
		}
		return nil, admin.InternalError(err)
	}
	return engineShape(engine), nil
}

// getDefaultSearchEngine is searchEngine.getDefaultSearchEngine:
// publicProcedure. Resolution order (search-engine-router.ts:73-135): the
// caller's default, else the server default from the search settings; a
// non-generic server default stays hidden from anonymous callers; a stale
// server default id is cleared on read.
func (s *service) getDefaultSearchEngine(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}

	if c.Session != nil {
		userDefaultID, err := s.db.GetUserDefaultSearchEngineID(ctx, c.Session.UserID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, admin.InternalError(err)
		}
		if userDefaultID != "" {
			engine, err := s.db.GetSearchEngineWithIntegrationByID(ctx, userDefaultID)
			if err != nil {
				return nil, admin.InternalError(err)
			}
			return s.renderOptionalEngine(engine), nil
		}
	}

	searchSettings, terr := s.getServerSettingSearch(ctx)
	if terr != nil {
		return nil, terr
	}
	defaultID, _ := searchSettings["defaultSearchEngineId"].(string)
	if defaultID == "" {
		return jsonNull, nil
	}
	engine, err := s.db.GetSearchEngineWithIntegrationByID(ctx, defaultID)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	if engine != nil {
		if c.Session == nil && engine.Type != "generic" {
			return jsonNull, nil
		}
		return engineFullShape(*engine), nil
	}

	// Remove the stale default from the settings (search-engine-router.ts:
	// 122-133); a failure only logs upstream.
	searchSettings["defaultSearchEngineId"] = nil
	if err := s.db.UpsertServerSetting(ctx, "search", admin.WrapSuperJSON(searchSettings)); err != nil {
		return nil, admin.InternalError(err)
	}
	return jsonNull, nil
}

// getServerSettingSearch reads the "search" settings record (the defaults
// merge of the serverSettings domain, duplicated here to avoid a package
// cycle with serversettings).
func (s *service) getServerSettingSearch(ctx context.Context) (map[string]any, *trpc.Error) {
	setting, err := s.db.GetServerSetting(ctx, "search")
	if errors.Is(err, db.ErrNotFound) {
		return map[string]any{"defaultSearchEngineId": nil}, nil
	}
	if err != nil {
		return nil, admin.InternalError(err)
	}
	raw, terr := admin.UnwrapSuperJSON(json.RawMessage(setting.Value), "server setting search")
	if terr != nil {
		return nil, terr
	}
	var stored map[string]any
	if json.Unmarshal(raw, &stored) != nil || stored == nil {
		return map[string]any{"defaultSearchEngineId": nil}, nil
	}
	if _, ok := stored["defaultSearchEngineId"]; !ok {
		stored["defaultSearchEngineId"] = nil
	}
	return stored, nil
}

// renderOptionalEngine returns nil for a missing engine row (the
// findFirst(... of a dangling user default returns undefined upstream).
func (s *service) renderOptionalEngine(engine *db.SearchEngineFull) any {
	if engine == nil {
		return nil
	}
	return engineFullShape(*engine)
}

// catalog is searchEngine.catalog: publicProcedure; anonymous callers see
// generic engines only; rows carry the integration relation, ordered by
// name.
func (s *service) catalog(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	engines, err := s.db.ListSearchEnginesWithIntegration(ctx, c.Session == nil)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(engines))
	for _, engine := range engines {
		out = append(out, engineFullShape(engine))
	}
	return out, nil
}

// search is searchEngine.search: publicProcedure; LIKE '<prefix>%' on the
// lowercased short column, generic-only for anonymous callers (#4132).
func (s *service) search(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := d.Str("query")
	if terr != nil {
		return nil, terr
	}
	limit, _, terr := d.OptInt64("limit", 1, 100)
	if terr != nil {
		return nil, terr
	}
	if limit == 0 {
		limit = 10 // zod default
	}
	prefix := strings.ToLower(strings.TrimSpace(query))
	engines, err := s.db.SearchSearchEnginesByShortPrefix(ctx, prefix, limit, c.Session == nil)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	out := make([]any, 0, len(engines))
	for _, engine := range engines {
		out = append(out, engineFullShape(engine))
	}
	return out, nil
}

// create is searchEngine.create: permission "search-engine-create".
func (s *service) create(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "search-engine-create"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	engine, terr := parseManageSchema(d, false)
	if terr != nil {
		return nil, terr
	}
	engine.ID = admin.CreateID()
	engine.Short = strings.ToLower(engine.Short)
	if err := s.db.CreateSearchEngine(ctx, engine); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// update is searchEngine.update: permission "search-engine-modify-all";
// NOT_FOUND "Search engine not found" for unknown ids; the short column is
// never updated.
func (s *service) update(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "search-engine-modify-all"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	engine, terr := parseManageSchema(d, true)
	if terr != nil {
		return nil, terr
	}
	if _, err := s.db.GetSearchEngineByID(ctx, engine.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, trpc.NewError(trpc.CodeNotFound, "Search engine not found")
		}
		return nil, admin.InternalError(err)
	}
	if err := s.db.UpdateSearchEngine(ctx, engine); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// delete is searchEngine.delete: permission "search-engine-full-all";
// detaches the engine from every user default before deleting.
func (s *service) delete(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	c, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(c, "search-engine-full-all"); terr != nil {
		return nil, terr
	}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	id, terr := d.Str("id")
	if terr != nil {
		return nil, terr
	}
	if err := s.db.ClearUserDefaultSearchEngine(ctx, id); err != nil {
		return nil, admin.InternalError(err)
	}
	if err := s.db.DeleteSearchEngine(ctx, id); err != nil {
		return nil, admin.InternalError(err)
	}
	return nil, nil
}

// parseManageSchema is searchEngineManageSchema / searchEngineEditSchema
// (packages/validation/src/search-engine.ts): a discriminated union on
// "type" plus the base fields {name 1-64, short 1-8 (create only),
// iconUrl min 1, description max 512 nullable}.
//
//	generic: urlTemplate min 1, startsWith("http"), includes("%s")
//	fromIntegration: integrationId optional
func parseManageSchema(d *admin.InputDecoder, withID bool) (db.SearchEngine, *trpc.Error) {
	engineType, terr := d.Enum("type", []string{"generic", "fromIntegration"})
	if terr != nil {
		return db.SearchEngine{}, terr
	}
	engine := db.SearchEngine{Type: engineType}
	if withID {
		id, terr := d.Str("id")
		if terr != nil {
			return db.SearchEngine{}, terr
		}
		engine.ID = id
	}
	name, terr := d.Str("name")
	if terr != nil {
		return db.SearchEngine{}, terr
	}
	if len(name) < 1 || len(name) > 64 {
		return db.SearchEngine{}, admin.BadRequestf(`input key "name" must be between 1 and 64 characters`)
	}
	engine.Name = name
	iconURL, terr := d.Str("iconUrl")
	if terr != nil {
		return db.SearchEngine{}, terr
	}
	if iconURL == "" {
		return db.SearchEngine{}, admin.BadRequestf(`input key "iconUrl" must not be empty`)
	}
	engine.IconURL = iconURL
	description, ok, terr := d.StringPtr("description")
	if terr != nil {
		return db.SearchEngine{}, terr
	}
	if ok && description != nil && len(*description) > 512 {
		return db.SearchEngine{}, admin.BadRequestf(`input key "description" must be at most 512 characters`)
	}
	if description != nil {
		engine.Description = *description
	}
	if engineType == "generic" {
		urlTemplate, terr := d.Str("urlTemplate")
		if terr != nil {
			return db.SearchEngine{}, terr
		}
		if urlTemplate == "" {
			return db.SearchEngine{}, admin.BadRequestf(`input key "urlTemplate" must not be empty`)
		}
		if !strings.HasPrefix(urlTemplate, "http") {
			return db.SearchEngine{}, admin.BadRequestf(`input key "urlTemplate" must start with http`)
		}
		if !strings.Contains(urlTemplate, "%s") {
			return db.SearchEngine{}, admin.BadRequestf(`input key "urlTemplate" must contain %%s`)
		}
		engine.URLTemplate = urlTemplate
	} else if integrationID, _, terr := d.StringPtr("integrationId"); terr != nil {
		return db.SearchEngine{}, terr
	} else if integrationID != nil {
		engine.IntegrationID = *integrationID
	}
	if !withID {
		short, terr := d.Str("short")
		if terr != nil {
			return db.SearchEngine{}, terr
		}
		if len(short) < 1 || len(short) > 8 {
			return db.SearchEngine{}, admin.BadRequestf(`input key "short" must be between 1 and 8 characters`)
		}
		engine.Short = short
	}
	return engine, nil
}
