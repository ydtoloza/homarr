package ws

import (
	"context"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

// Request is what a SubscriptionHandler sees when the client subscribes.
type Request struct {
	// Path is the full tRPC procedure path, e.g. "log.subscribe"
	// (packages/api/src/root.ts nests routers with dots).
	Path string
	// Input is the superjson-unwrapped procedure input, "null" when the
	// client sent none (the same normalization the HTTP side applies via
	// trpc.UnwrapInput; parseTRPCMessage applies transformer.input.deserialize
	// per message, tRPC packages/server/src/.../rpc/parseTRPCMessage.ts:77).
	Input json.RawMessage
	// Session is the validated caller, nil when anonymous. Handlers enforce
	// their own authorization exactly like upstream tRPC middlewares do
	// (permissionRequiredProcedure, packages/api/src/trpc.ts): the transport
	// never rejects a subscription for lack of permissions, it only delivers
	// the error the handler returns.
	Session *ctxauth.Session
}

// Subscription is one live client subscription, created per request by a
// SubscriptionHandler. The flow mirrors an upstream tRPC observable
// subscription: on subscribe the procedure may emit one initial value and
// then forwards channel events until the client stops
// (packages/api/src/router/log.ts:26-84, docker/docker-router.ts:210-256,
// cron-jobs.ts:62-82).
type Subscription struct {
	// Initial is the optional first data frame payload, sent right after the
	// "started" frame (e.g. the log history event of log.subscribe). nil = no
	// initial frame. It must be a JSON-serializable value; json.RawMessage
	// passes through without re-encoding.
	Initial any
	// Pattern is the bus topic pattern whose published messages are forwarded
	// to the client as data frames, e.g. "events.log.*" ("" = no bus
	// delivery). See the bus package for the matching rules and the
	// events.<domain>.<id> topic convention.
	Pattern string
	// Decode maps a bus message to the result-frame payload. ok=false drops
	// the message (e.g. level filters). json.RawMessage(msg.Payload) is the
	// zero-copy passthrough. nil Decode forwards the raw payload unchanged.
	// The closure captures the request input so per-subscription filters
	// (log levels, container id) stay with the subscription.
	Decode func(msg bus.Message) (data any, ok bool)
}

// SubscriptionHandler serves one tRPC subscription path over the WebSocket
// endpoint. Register it on the Hub with Register.
type SubscriptionHandler interface {
	// Subscribe resolves a subscription request. Returning an error rejects
	// the subscription with a tRPC error frame (the connection stays open —
	// upstream behaves the same: the procedure throws inside the connection
	// handler and the adapter answers with an error envelope,
	// tRPC packages/server/src/adapters/ws.ts handleRequest catch).
	Subscribe(ctx context.Context, req *Request) (*Subscription, error)
}

// SubscriptionHandlerFunc adapts a function to SubscriptionHandler.
type SubscriptionHandlerFunc func(ctx context.Context, req *Request) (*Subscription, error)

// Subscribe implements SubscriptionHandler.
func (f SubscriptionHandlerFunc) Subscribe(ctx context.Context, req *Request) (*Subscription, error) {
	return f(ctx, req)
}
