package trpc

// SSE subscriptions over the classic tRPC HTTP endpoint — the transport of
// httpSubscriptionLink (@trpc/client 11.18.0).
//
// Upstream the SPA routes every `type === "subscription"` operation to the
// WebSocket link EXCEPT `widget.beszel.subscribeSystemStats`, which goes to
// httpSubscriptionLink: a plain HTTP GET to /api/trpc/<path> with
// `Accept: text/event-stream` (apps/nextjs trpc.tsx:194-200). The server
// answers with an event stream produced by sseStreamProducer
// (packages/server/src/unstable-core-do-not-import/stream/sse.ts): named
// events `connected`, `ping`, `serialized-error` and `return`, plus unnamed
// (default "message") events carrying each subscription value. The consumer
// is sseStreamConsumer (sse.ts of the same file): it registers listeners for
// exactly those names, so the event names and field order below are
// load-bearing.
//
// Upstream picks the SSE response by PROCEDURE TYPE (a subscription resolves
// to the subscription branch of resolveResponse.ts:458-540 regardless of the
// accept header), so homarr-go does the same by registry: a path registered
// here is a subscription and single (non-batch) requests to it stream SSE.
// Paths not registered keep the classic NOT_FOUND envelope — exactly what
// upstream answers for unknown paths. Batches containing a subscription path
// fail per-op like upstream ("Cannot batch subscription calls",
// resolveResponse.ts:373-378).
//
// The stream is always HTTP 200 with sseHeaders (sse.ts:449-454); errors
// ride inside the stream as `serialized-error` events (errorToAsyncIterable,
// resolveResponse.ts:32-36). Byte format per event (sse.ts:427-443): the
// `event:` line (only for named events), the `data:` line, the optional
// `id:` line AFTER data (tracked envelopes — homarr-go subscriptions are not
// tracked, so no id lines), then the `\n\n` block terminator.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

// SubscriptionRequest is what a SubscriptionHandler sees when a client opens
// an SSE subscription. Session comes from the request context (the auth
// middleware stores it there, exactly like regular procedures).
type SubscriptionRequest struct {
	// Path is the full tRPC procedure path, e.g. "log.subscribe".
	Path string
	// Input is the superjson-unwrapped procedure input, "null" when the
	// client sent none (the same trpc.UnwrapInput normalization the HTTP
	// side applies to queries).
	Input json.RawMessage
	// Session is the authenticated caller, nil when anonymous. Handlers
	// enforce authorization themselves (the SSE transport never rejects a
	// subscription for missing permissions; the error the handler returns
	// becomes a serialized-error event on an otherwise healthy stream).
	Session *ctxauth.Session
}

// Subscription is one accepted SSE subscription. The shape mirrors
// internal/ws.Subscription so one underlying service can serve both
// transports with thin adapters: Initial is the optional first data event,
// Pattern is the bus topic pattern whose published messages stream as data
// events ("" = no live delivery), and Decode filters/maps each bus message
// (nil = raw passthrough).
type Subscription struct {
	// Initial is the optional first data event payload (e.g. the log history
	// event of log.subscribe). nil = no initial event. JSON-serializable; a
	// SuperJSON value passes through verbatim.
	Initial any
	// Pattern is the bus pattern forwarded as live data events, e.g.
	// "events.log.*" ("" = nothing). See internal/bus for matching rules.
	Pattern string
	// Decode maps a bus message to the data event payload; ok=false drops the
	// message. json.RawMessage(msg.Payload) is the zero-copy passthrough.
	// The closure captures the request input so per-subscription filters
	// (log levels, container id) stay with the subscription.
	Decode func(msg bus.Message) (data any, ok bool)
}

// SubscriptionHandler serves one tRPC subscription path over SSE. Register it
// on the Router with RegisterSubscription.
type SubscriptionHandler interface {
	// Subscribe resolves a subscription request. Returning an error rejects
	// the subscription: the stream still opens with the connected event and
	// then carries one serialized-error event with the tRPC error shape —
	// the upstream behavior of a procedure that throws before emitting
	// (resolveResponse.ts:32-36 errorToAsyncIterable feeding the
	// producer's catch, sse.ts:209-225).
	Subscribe(ctx context.Context, req *SubscriptionRequest) (*Subscription, error)
}

// SubscriptionHandlerFunc adapts a function to SubscriptionHandler.
type SubscriptionHandlerFunc func(ctx context.Context, req *SubscriptionRequest) (*Subscription, error)

// Subscribe implements SubscriptionHandler.
func (f SubscriptionHandlerFunc) Subscribe(ctx context.Context, req *SubscriptionRequest) (*Subscription, error) {
	return f(ctx, req)
}

// RegisterSubscription registers a subscription under its full dotted path
// (e.g. "log.subscribe"). It panics on duplicates, empty names or nil
// handlers: wiring happens at startup and a collision is a programming
// error. The SSE registry is separate from the procedure registry: upstream
// resolves the response mode from the procedure type, and the SPA only
// reaches subscriptions over SSE (httpSubscriptionLink) or the WebSocket
// hub (internal/ws).
func (r *Router) RegisterSubscription(name string, handler SubscriptionHandler) {
	if name == "" {
		panic("trpc: subscription name must not be empty")
	}
	if handler == nil {
		panic("trpc: subscription " + name + " must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.subs[name]; dup {
		panic("trpc: subscription " + name + " already registered")
	}
	r.subs[name] = handler
}

// lookupSubscription returns the subscription at path, or nil.
func (r *Router) lookupSubscription(path string) SubscriptionHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.subs[path]
}

// Event names pinned from sse.ts:80-84 (PING_EVENT, SERIALIZED_ERROR_EVENT,
// CONNECTED_EVENT, RETURN_EVENT). Value events carry no event name: unnamed
// SSE events dispatch as "message", which is what sseStreamConsumer listens
// to for data.
const (
	sseConnectedEvent       = "connected"
	ssePingEvent            = "ping"
	sseSerializedErrorEvent = "serialized-error"
	sseReturnEvent          = "return"
	sseConnectedData        = "{}" // JSON.stringify(client) with no client options configured (sse.ts:158)
	sseEmptyData            = ""   // ping/return events carry an empty data payload (sse.ts:180, 207)
)

// renderSSE renders one event in the exact wire format of the upstream
// TransformStream (sse.ts:427-443): optional "event:" line, "data:" line,
// then the blank block terminator. The empty-data case renders `data: ` with
// the trailing space, like the producer's `data: ${”}\n`.
func renderSSE(event, data string) string {
	out := ""
	if event != "" {
		out += "event: " + event + "\n"
	}
	out += "data: " + data + "\n"
	out += "\n\n"
	return out
}

// serveSSE answers one subscription request with the tRPC SSE event stream.
// Control flow mirrors the subscription branch of resolveResponse
// (resolveResponse.ts:458-540) feeding sseStreamProducer (sse.ts:157-226).
func (r *Router) serveSSE(w http.ResponseWriter, req *http.Request, path string, handler SubscriptionHandler, input json.RawMessage, parseErr *Error) {
	// sseHeaders (sse.ts:449-454) + HTTP 200 (initResponse with
	// untransformedJSON null, resolveResponse.ts:512-522). CORS and Vary are
	// already set by ServeHTTP.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeRaw := func(event, data string) bool {
		if _, err := io.WriteString(w, renderSSE(event, data)); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// writeData serializes one subscription value like the producer does
	// (sse.ts:181-190: JSON.stringify(serialize(value)) — the transformer
	// output, {"json": <value>} for plain payloads).
	writeData := func(value any) bool {
		payload, err := WrapJSON(value)
		if err != nil {
			return false
		}
		return writeRaw("", string(payload))
	}
	writeError := func(terr *Error) bool {
		payload, err := WrapJSON(terr.shape(path))
		if err != nil {
			return false
		}
		return writeRaw(sseSerializedErrorEvent, string(payload))
	}

	// First event: connected with the client options (always {} here —
	// upstream configures none, sse.ts:158).
	if !writeRaw(sseConnectedEvent, sseConnectedData) {
		return
	}

	// Shared input parse failure: the lazy per-op parse inside the call —
	// surfaced as a serialized-error event, not a JSON envelope
	// (resolveResponse.ts:461-463).
	if parseErr != nil {
		writeError(parseErr)
		return
	}
	unwrapped, uerr := UnwrapInput(input)
	if uerr != nil {
		writeError(NewError(CodeBadRequest, "Failed to parse input: "+uerr.Error()))
		return
	}

	// Method pairing (resolveResponse.ts:350-353): subscriptions accept GET
	// only; the SPA always uses GET. A non-GET call rejects inside the
	// subscription branch, so the stream opens and then errors.
	if req.Method != http.MethodGet {
		writeError(NewError(CodeMethodNotSupported,
			`Unsupported `+req.Method+`-request to subscription procedure at path "`+path+`"`))
		return
	}

	sub, serr := handler.Subscribe(req.Context(), &SubscriptionRequest{
		Path:    path,
		Input:   unwrapped,
		Session: ctxauth.FromContext(req.Context()),
	})
	if serr != nil {
		if terr, ok := serr.(*Error); ok {
			writeError(terr)
		} else {
			writeError(Internal(serr))
		}
		return
	}

	// Arm the bus subscription BEFORE the initial event so no live message
	// is lost in the gap (the same ordering internal/ws uses).
	var busSub *bus.Subscription
	if sub.Pattern != "" && r.Bus != nil {
		busSub = r.Bus.Subscribe(sub.Pattern)
	}
	if busSub != nil {
		defer busSub.Unsubscribe()
	}

	if sub.Initial != nil && !writeData(sub.Initial) {
		return
	}

	// Idle ping: the producer wraps the value iterable with withPing
	// (sse.ts:166-172) — a ping when no value arrived for the interval, the
	// timer restarting after every forwarded value. Upstream disables pings
	// by default (ping.enabled ?? false; the homarr route handler configures
	// no sse options), so Router.SSEPingInterval defaults to 0 = off.
	var pingTimer *time.Timer
	if r.SSEPingInterval > 0 {
		pingTimer = time.NewTimer(r.SSEPingInterval)
		defer pingTimer.Stop()
	}

	decode := sub.Decode
	if decode == nil {
		decode = func(msg bus.Message) (any, bool) { return json.RawMessage(msg.Payload), true }
	}

	var msgCh <-chan bus.Message
	if busSub != nil {
		msgCh = busSub.C
	}

	for {
		var pingC <-chan time.Time
		if pingTimer != nil {
			pingC = pingTimer.C
		}
		select {
		case <-req.Context().Done():
			// Client went away: upstream cancels the response stream without
			// further events.
			return
		case <-pingC:
			if !writeRaw(ssePingEvent, sseEmptyData) {
				return
			}
			pingTimer.Reset(r.SSEPingInterval)
		case msg, ok := <-msgCh:
			if !ok {
				// Bus closed (shutdown): the subscription completed — the
				// generator's return event (sse.ts:205-209).
				writeRaw(sseReturnEvent, sseEmptyData)
				return
			}
			if pingTimer != nil {
				if !pingTimer.Stop() {
					select {
					case <-pingTimer.C:
					default:
					}
				}
				pingTimer.Reset(r.SSEPingInterval)
			}
			data, keep := decode(msg)
			if !keep {
				continue
			}
			if !writeData(data) {
				return
			}
		}
	}
}
