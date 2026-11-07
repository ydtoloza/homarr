// Package log is the Go port of the log tRPC router
// (packages/api/src/router/log.ts): the single `log.subscribe` subscription
// that feeds the manage/tools/logs terminal of the SPA with the binario's
// own logs.
//
// Upstream the subscription emits, over the wsLink, an initial
// `{type:"history", messages}` event (the newest 5000 entries of the Redis
// history list, filtered by the requested levels, oldest first) and then one
// `{type:"message", message}` event per live entry of the logging channel.
// In homarr-go the history lives in the log table (internal/logging, the
// additive replacement of the Redis list) and the live entries ride the bus
// on "events.log.<level>" (published by the pipeline of internal/logging).
//
// The permission gate mirrors permissionRequiredProcedure
// .requiresPermission("other-view-logs") (log.ts:20-21): anonymous callers
// get UNAUTHORIZED, signed-in callers without the expanded permission get
// FORBIDDEN "Permission denied".
//
// The subscription is exposed on BOTH transports:
//
//   - the WebSocket hub (the transport the SPA actually uses — wsLink,
//     trpc.tsx:201-204): Register returns the *Service whose
//     SubscriptionHandlers() feeds hub.Register, exactly like the docker
//     router (register.go there);
//   - the SSE registry of the trpc router (httpSubscriptionLink shape,
//     registered here directly) — upstream serves ANY subscription over
//     SSE too (resolveResponse picks the branch by procedure type), and
//     wiring it here costs one adapter.
package log

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/logging"
	"github.com/ydtoloza/homarr/server/internal/ws"
)

// Options configures the log router.
type Options struct {
	// Store is the history source (the pipeline's SQLiteStore). nil serves
	// an empty history (the pipeline without persistence).
	Store logging.Store
}

// Service carries the dependencies shared by the subscription handlers.
// Register returns it so the composition root can wire SubscriptionHandlers()
// onto the WebSocket hub.
type Service struct {
	svc *service
}

type service struct {
	store logging.Store
	db    *db.DB
}

// Register registers the log router. Call once at startup, after
// logging.NewSQLiteStore. After chi mounts trpc.Router.Handler() at
// /api/trpc/*:
//
//	svc := log.Register(r, database, log.Options{Store: logStore})
//	for path, handler := range svc.SubscriptionHandlers() {
//	    hub.Register(path, handler)
//	}
func Register(r *trpc.Router, database *db.DB, opts Options) *Service {
	s := &service{store: opts.Store, db: database}

	// log.subscribe (log.ts:19-84): a subscription, so no procedure entry —
	// the SSE registry (below) answers single HTTP requests and the hub
	// registry (SubscriptionHandlers) answers the WebSocket.
	r.RegisterSubscription("log.subscribe", s.sseHandler())

	return &Service{svc: s}
}

// SubscriptionHandlers returns the WebSocket subscription handlers keyed by
// their tRPC path (the docker router convention, register.go:115-125).
func (s *Service) SubscriptionHandlers() map[string]ws.SubscriptionHandler {
	return map[string]ws.SubscriptionHandler{
		"log.subscribe": ws.SubscriptionHandlerFunc(s.svc.wsSubscribe),
	}
}

// wsSubscribe adapts the shared subscription resolution to the ws contract.
func (s *service) wsSubscribe(ctx context.Context, req *ws.Request) (*ws.Subscription, error) {
	sub, terr := s.subscribe(ctx, req.Input, req.Session)
	if terr != nil {
		return nil, terr
	}
	return &ws.Subscription{Initial: sub.Initial, Pattern: sub.Pattern, Decode: sub.Decode}, nil
}

// sseHandler adapts the shared subscription resolution to the trpc SSE
// contract.
func (s *service) sseHandler() trpc.SubscriptionHandler {
	return trpc.SubscriptionHandlerFunc(func(ctx context.Context, req *trpc.SubscriptionRequest) (*trpc.Subscription, error) {
		sub, terr := s.subscribe(ctx, req.Input, req.Session)
		if terr != nil {
			return nil, terr
		}
		return &trpc.Subscription{Initial: sub.Initial, Pattern: sub.Pattern, Decode: sub.Decode}, nil
	})
}

// subscription is the transport-neutral result of one subscription request.
type subscription struct {
	Initial any
	Pattern string
	Decode  func(msg bus.Message) (data any, ok bool)
}

// subscribe resolves one log.subscribe request: the upstream gates in order
// (permissionRequiredProcedure "other-view-logs", then the zod input), the
// history Initial and the bus wiring.
func (s *service) subscribe(ctx context.Context, input json.RawMessage, session *ctxauth.Session) (*subscription, *trpc.Error) {
	// permissionRequiredProcedure extends protectedProcedure: anonymous
	// callers get UNAUTHORIZED, non-privileged callers FORBIDDEN "Permission
	// denied" (packages/api/src/trpc.ts:195-207). The db lookup expands the
	// caller's group permissions exactly like the HTTP routers do.
	if session != nil {
		ctx = ctxauth.WithSession(ctx, session)
	}
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return nil, terr
	}
	if terr := admin.RequirePermission(caller, "other-view-logs"); terr != nil {
		return nil, terr
	}

	// Input: {levels: LogLevel[] default ["info"]} (log.ts:22-25). An absent
	// input applies the default; an explicit empty array filters everything
	// (zod passes it through and levels.includes is always false).
	levels := []logging.Level{logging.LevelInfo}
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	if d.Has("levels") {
		raw, _, terr := d.OptStrList("levels")
		if terr != nil {
			return nil, terr
		}
		parsed := make([]logging.Level, 0, len(raw))
		for _, l := range raw {
			level, ok := logging.ParseLevel(l)
			if !ok {
				// zodEnumFromArray(logLevels): an invalid key fails the
				// parse (BAD_REQUEST, log.ts:23).
				return nil, admin.BadRequestf(`Invalid enum value for levels: "%s"`, l)
			}
			parsed = append(parsed, level)
		}
		levels = parsed
	}

	// History event: {type:"history", messages} — oldest first, level
	// filtered (log.ts:52-55), rendered with the superjson Date annotation
	// the SPA terminal requires (timestamp.getTime(), terminal.tsx:96).
	initial, terr := s.historyEvent(levels)
	if terr != nil {
		return nil, terr
	}

	return &subscription{
		Initial: initial,
		// The live feed: every level topic, filtered per subscription by the
		// Decode closure (the same shape WEBSOCKET.md section 6 documents for
		// the ws handler).
		Pattern: "events.log.*",
		Decode:  messageDecode(levels),
	}, nil
}

// loggerMessage is one LoggerMessage with the upstream key order
// (loggerMessageSchema, constants.ts:9-14: id, timestamp, message, level).
type loggerMessage struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
}

// historyEvent loads the history and renders the {type:"history", messages}
// event as a SuperJSON payload. A nil store (no persistence configured)
// serves an empty history, like a fresh Redis list.
func (s *service) historyEvent(levels []logging.Level) (trpc.SuperJSON, *trpc.Error) {
	entries := []loggerMessage{}
	annotations := map[string]string{}
	if s.store != nil {
		recent, err := s.store.Recent(context.Background(), levels, logging.HistoryMaxEntries)
		if err != nil {
			return nil, admin.InternalError(err)
		}
		for i, e := range recent {
			entries = append(entries, messageShape(e))
			annotations["messages."+strconv.Itoa(i)+".timestamp"] = "Date"
		}
	}
	sj, err := trpc.WrapJSONWithMeta(struct {
		Type     string          `json:"type"`
		Messages []loggerMessage `json:"messages"`
	}{Type: "history", Messages: entries}, annotations)
	if err != nil {
		return nil, admin.InternalError(err)
	}
	return sj, nil
}

// messageShape renders one entry in the wire shape.
func messageShape(e logging.Entry) loggerMessage {
	return loggerMessage{
		ID:        e.ID,
		Timestamp: logging.FormatTimestamp(e.Timestamp),
		Message:   e.Message,
		Level:     string(e.Level),
	}
}

// messageDecode builds the per-subscription Decode closure: it parses the
// bus payload (the superjson LoggerMessage envelope published by the
// pipeline), drops entries outside the requested levels and renders the
// {type:"message", message} event with the Date annotation.
func messageDecode(levels []logging.Level) func(msg bus.Message) (any, bool) {
	requested := make(map[logging.Level]bool, len(levels))
	for _, l := range levels {
		requested[l] = true
	}
	return func(msg bus.Message) (any, bool) {
		entry, ok := logging.UnmarshalMessage(msg.Payload)
		if !ok {
			return nil, false // malformed payload: dropped like parseLoggerMessage
		}
		if !requested[entry.Level] {
			return nil, false // the upstream levels filter, log.ts:35-37
		}
		sj, err := trpc.WrapJSONWithMeta(struct {
			Type    string        `json:"type"`
			Message loggerMessage `json:"message"`
		}{Type: "message", Message: messageShape(entry)}, map[string]string{"message.timestamp": "Date"})
		if err != nil {
			return nil, false
		}
		return sj, true
	}
}
