// Package info is the Go port of the info tRPC router
// (packages/api/src/router/info.ts): server version reporting and the demo
// mode flags. All three procedures are static — the update-checker router
// (not ported here) is the one that talks to the outside world.
package info

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// defaultVersion is the upstream package.json version this port tracks
// (root package.json:3). The integrator can override it through Options.
const defaultVersion = "2.11.1"

// Options configures the info router. The zero value serves the upstream
// defaults (version 2.11.1, demo mode off).
type Options struct {
	// Version overrides the reported version (info.getInfo).
	Version string
	// DemoMode mirrors env.DEMO_MODE (isDemoMode, packages/api/src/trpc.ts:114).
	DemoMode bool
	// DemoReadOnly mirrors env.DEMO_READ_ONLY; the wire value of
	// info.isDemoReadOnly is DemoMode && DemoReadOnly (trpc.ts:118).
	DemoReadOnly bool
}

// Register registers the info router procedures. The database is unused
// (every procedure is static); the parameter keeps the uniform
// Register(r, database, opts) signature of the router packages.
func Register(r *trpc.Router, _ *db.DB, opts Options) {
	version := opts.Version
	if version == "" {
		version = defaultVersion
	}

	// info.getInfo (info.ts:7-28): protectedProcedure, no message on the
	// UNAUTHORIZED error (tRPC falls back to the code string).
	r.Register("info.getInfo", func(ctx context.Context, _ json.RawMessage) (any, *trpc.Error) {
		if ctxauth.FromContext(ctx) == nil {
			return nil, trpc.NewError(trpc.CodeUnauthorized, "UNAUTHORIZED")
		}
		return map[string]any{"version": version}, nil
	})

	// info.isDemoMode / info.isDemoReadOnly (info.ts:29-30): public.
	r.Register("info.isDemoMode", func(_ context.Context, _ json.RawMessage) (any, *trpc.Error) {
		return opts.DemoMode, nil
	})
	r.Register("info.isDemoReadOnly", func(_ context.Context, _ json.RawMessage) (any, *trpc.Error) {
		return opts.DemoMode && opts.DemoReadOnly, nil
	})
}
