package admin

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// Read-only API key enforcement, ported from credentialProcedure
// (packages/api/src/trpc.ts:130-135):
//
//	const credentialProcedure = t.procedure.use(({ ctx, type, meta, next }) => {
//	  if (ctx.session?.apiKey?.readOnly && !isReadOnlyProcedure(type, meta)) {
//	    throw new TRPCError({ code: "FORBIDDEN", message: "This credential only permits read-only operations" });
//	  }
//	  return next();
//	});
//
// with isReadOnlyProcedure (packages/api/src/credential-access.ts:5-6):
//
//	export const isReadOnlyProcedure = (type: string, meta: unknown) =>
//	  type === "query" && !(... meta.readOnly === false);
//
// i.e. every MUTATION (and every query that declares meta.readOnly === false)
// is rejected for callers authenticated with a read-only API key, and the
// rejection runs as the BASE middleware — before the procedure's own
// permission gates and input parsing. Upstream applies credentialProcedure to
// every procedure of the API (it is the base of publicProcedure,
// protectedProcedure, permissionRequiredProcedure, onboardingClaimedProcedure
// and internalProcedure); session-cookie callers never carry apiKey, so only
// API-key sessions can be rejected.
//
// homarr-go has no procedure types on the wire (internal/api/trpc/trpc.go),
// so the "type === mutation" half of the upstream condition is resolved at
// REGISTRATION time: the upstream port classifies each procedure (a
// procedure that mutates state upstream is a .mutation(...)) and wraps those
// with Writable. None of the routers ported so far declare
// meta.readOnly === false (those live in icons/custom-widget/integration/
// widgets upstream), so mutations alone decide.

// ReadOnlyForbiddenMessage is the exact upstream TRPCError message
// (packages/api/src/trpc.ts:132).
const ReadOnlyForbiddenMessage = "This credential only permits read-only operations"

// RequireWritable reports the credentialProcedure rejection for a caller:
// FORBIDDEN with the exact upstream message when the session belongs to a
// read-only API key. Anonymous and session-cookie callers pass (upstream:
// ctx.session?.apiKey?.readOnly is undefined for them). The procedure's own
// gates (signed-in, permission) stay responsible for the rest.
func RequireWritable(c Caller) *trpc.Error {
	if c.Session != nil && c.Session.ReadOnly {
		return trpc.NewError(trpc.CodeForbidden, ReadOnlyForbiddenMessage)
	}
	return nil
}

// Writable wraps a mutation procedure with the read-only API key gate. It is
// applied at registration time to every ported write procedure so the
// rejection keeps the upstream ordering: credentialProcedure is the base
// middleware of every procedure, ahead of permission checks and input
// validation. The check reads only the session carried in the request
// context (no database access) — the flag rides on ctxauth.Session.ReadOnly,
// set by the auth middleware when it resolves an API key (AUTH.md §6).
func Writable(next trpc.Procedure) trpc.Procedure {
	return func(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
		if err := RequireWritable(Caller{Session: ctxauth.FromContext(ctx)}); err != nil {
			return nil, err
		}
		return next(ctx, input)
	}
}
