// Package bangs is the Go port of the bangs tRPC router
// (packages/api/src/router/bangs/bangs-router.ts).
//
// Upstream has a single procedure, bangs.search: it proxies the DuckDuckGo
// bang list (https://duckduckgo.com/bang.js, ~13k entries) through
// @homarr/request-handler/duckduckgo-bangs, which fetches the list over the
// network, normalizes, sorts and caches it. It is NOT an in-memory/static
// dataset upstream, so the procedure is deferred until the external HTTP
// layer of the port exists (M3/M4): Register intentionally registers
// nothing, exactly like the deferred procedures of the board router
// (BOARDS.md §6) — callers get the standard NOT_FOUND envelope for
// "bangs.search" until then.
package bangs

import (
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// Options is present for the uniform Register(r, database, opts) signature;
// it has no fields yet.
type Options struct{}

// Register registers no procedures for now — see the package comment. When
// the DuckDuckGo request handler is ported, "bangs.search" (publicProcedure,
// {query, limit 1-50 default 20}) registers here.
func Register(_ *trpc.Router, _ *db.DB, _ Options) {}
