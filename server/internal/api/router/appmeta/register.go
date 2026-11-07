// Package appmeta is the Go port of the app.detectMetadata procedure of the
// upstream app router (packages/api/src/router/app.ts:227-239) — the
// "detect" button of the app creation modal that fills the name and icon of
// a new app from the website behind its URL.
//
// Upstream the procedure is a MUTATION of the app router (the website url
// travels in the request body and stays out of access logs) gated by
// permissionRequiredProcedure.requiresPermission("app-create"). It composes:
//
//   - fetchWebsiteMetadataAsync (packages/icons/src/favicon-fetcher.ts): the
//     SSRF-guarded fetch of the page (Accept text/html, 5s per hop, 15s
//     detection deadline, 256 KiB head, up to 5 redirects per hop re-checked),
//     the <title> and <link rel=icon> declarations with their relation/size
//     ranking, and the /favicon.ico well known fallback of the origin that
//     answered — ported in favicon.go + guardedfetch.go;
//   - getIconForName (packages/icons/src/auto-icon-searcher.ts): the first
//     icon row whose name contains the detected name (LIKE %name%, no
//     escaping — % and _ act as wildcards there too), db.FindIconByNameLike;
//   - the default icon (app.ts:21): the homarr dashboard icon of jsdelivr.
//
// The name falls back to the hostname of the requested URL and is cut at 64
// (app.ts:236; the upstream slice counts UTF-16 units, the port counts bytes
// — only a multi-byte title cut at the boundary notices). The output schema
// guarantees both members non-null: {name: string, iconUrl: string}
// (app.ts:232).
//
// The procedure lives in its own package because the board package owns the
// rest of the app router (register.go there) and this flow drags the
// guarded-fetch machinery with it; the dotted path is unchanged for the
// frontend. env.NO_EXTERNAL_CONNECTION (upstream test env answering null
// metadata) is not ported, like the other external proxies.
package appmeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// defaultIcon is the defaultIcon constant of app.ts:21.
const defaultIcon = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons@master/svg/homarr.svg"

// Options configures the app metadata router.
type Options struct {
	// HTTPClient overrides the guarded client (tests inject a plain client
	// against an httptest site; the production Register wraps
	// newGuardedClient).
	HTTPClient *http.Client
}

// DefaultOptions is the production Options: the SSRF-guarded client with the
// upstream per-hop timeout.
func DefaultOptions() Options {
	return Options{HTTPClient: newGuardedClient(requestTimeout)}
}

// service carries the dependencies of the procedure.
type service struct {
	db     *db.DB
	client *guardedHTTP
}

// Register registers app.detectMetadata on the shared router. Call once at
// startup alongside board.Register (which owns the other app.* procedures).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	s := &service{db: database, client: &guardedHTTP{client: opts.HTTPClient}}

	// app.detectMetadata (app.ts:227-239): a mutation — wrapped with
	// admin.Writable (the credentialProcedure read-only gate).
	r.Register("app.detectMetadata", admin.Writable(s.detectMetadata))
}

// detectMetadata mirrors the procedure body (app.ts:233-239).
func (s *service) detectMetadata(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(caller, "app-create"); terr != nil {
		return nil, terr
	}

	// iconsDetectFaviconSchema (packages/validation/src/icons.ts:7-12):
	// {href: trimmed URL string matching ^https?://}. The zod .url() is
	// structurally checked here: parseable, http/https, with a host.
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	href, terr := d.Str("href")
	if terr != nil {
		return nil, terr
	}
	href = strings.TrimSpace(href)
	if !strings.HasPrefix(strings.ToLower(href), "http://") && !strings.HasPrefix(strings.ToLower(href), "https://") {
		return nil, admin.BadRequestf("expected an http(s) url for href")
	}
	parsed, perr := url.Parse(href)
	if perr != nil || parsed.Host == "" {
		return nil, admin.BadRequestf("expected a valid url for href")
	}

	metadata := fetchWebsiteMetadata(ctx, s.client, href)

	// name = (metadata.title ?? new URL(input.href).hostname).slice(0, 64)
	// (app.ts:236).
	name := metadata.Title
	if name == "" {
		name = parsed.Hostname()
	}
	if len(name) > 64 {
		name = name[:64]
	}

	// iconUrl = metadata.iconUrl ?? getIconForName(ctx.db, name).sync()?.url
	// ?? defaultIcon (app.ts:238).
	iconURL := metadata.IconURL
	if iconURL == "" {
		if icon, err := s.db.FindIconByNameLike(ctx, name); err == nil {
			iconURL = icon.URL
		}
	}
	if iconURL == "" {
		iconURL = defaultIcon
	}

	// The output schema (app.ts:232) fixes the key order name, iconUrl.
	return detectMetadataOutput{Name: name, IconURL: iconURL}, nil
}

// detectMetadataOutput is the .output(z.object({name, iconUrl})) shape.
type detectMetadataOutput struct {
	Name    string `json:"name"`
	IconURL string `json:"iconUrl"`
}
