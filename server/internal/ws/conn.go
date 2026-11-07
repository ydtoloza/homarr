package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

const (
	// outQueueSize bounds the per-connection outbound frames. Dashboard
	// events are ephemeral: a client that cannot keep up is dropped instead
	// of buffering without limit (the bus drops messages for slow
	// subscribers with the same rationale).
	outQueueSize = 256
	// writeTimeout caps a single socket write; upstream's client.terminate()
	// on pong timeout covers the stuck-peer case, this covers a wedged write.
	writeTimeout = 10 * time.Second
	// maxMessageSize caps one incoming frame. Upstream (ws) applies no limit;
	// 1 MiB is a safety net well above any tRPC subscription input.
	maxMessageSize = 1 << 20
)

// outFrame is one pending outbound message. closeAfter makes the writer
// perform the close handshake right after flushing data, so frames enqueued
// before it (e.g. the reconnect notification) are delivered in order.
type outFrame struct {
	data       []byte
	closeAfter bool
}

// activeSub is a client subscription registered on a connection.
type activeSub struct {
	id json.RawMessage
	// pattern/decode mirror the Subscription the handler produced.
	pattern string
	decode  func(msg bus.Message) (any, bool)
	// busSub is non-nil when pattern != "".
	busSub *bus.Subscription
	// stopCh is closed by the reader goroutine on "subscription.stop".
	stopCh chan struct{}
}

// conn owns one WebSocket connection. Goroutine ownership: the run
// goroutine reads frames and is the ONLY mutator of subs; writeLoop is the
// only socket writer; one optional keepaliveLoop; one pump goroutine per
// live bus-backed subscription. This satisfies coder/websocket's
// one-reader/one-writer rule — no concurrent writes share the conn.
type conn struct {
	hub    *Hub
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	out    chan outFrame
	// activity carries one token per inbound message; keepaliveLoop consumes
	// it to reset its timers (any message counts, ws.ts handleKeepAlive
	// onMessage).
	activity chan struct{}
	subs     map[string]*activeSub
	session  *ctxauth.Session
	// rejectAPIKey mirrors the upstream createContext rejection: one
	// UNAUTHORIZED error frame, then close (apps/websocket/src/main.ts:41-43
	// throws; ws.ts createCtxPromise responds and closes on the next tick).
	rejectAPIKey bool
	closeOnce    sync.Once
}

func (c *conn) run() {
	defer c.cleanup()
	go c.writeLoop()
	if ka := c.hub.opts.keepAlive(); ka != nil {
		go c.keepaliveLoop(ka.PingInterval, ka.PongTimeout)
	}
	if c.rejectAPIKey {
		c.enqueueError(nil, trpc.CodeUnauthorized, "API keys are not supported over WebSockets", "")
		c.enqueue(outFrame{closeAfter: true})
	}
	c.readLoop()
}

// cleanup releases every resource once, whichever exit path wins (read
// error, hub Close, write failure).
func (c *conn) cleanup() {
	c.closeOnce.Do(func() {
		for _, s := range c.subs {
			close(s.stopCh)
			if s.busSub != nil {
				s.busSub.Unsubscribe()
			}
		}
		c.subs = map[string]*activeSub{}
		c.cancel()
		c.hub.removeConn(c)
		_ = c.ws.CloseNow()
	})
}

func (c *conn) readLoop() {
	c.ws.SetReadLimit(maxMessageSize)
	for {
		msgType, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		// Any inbound message resets the keepalive timers (ws.ts
		// handleKeepAlive onMessage), including PING/PONG text.
		c.signalActivity()

		if msgType != websocket.MessageText {
			// Upstream feeds binary frames through jsonEncoder.decode which
			// throws; the catch answers PARSE_ERROR and keeps the connection
			// open (ws.ts:494-509). The message text is the encoder's.
			c.enqueueError(nil, trpc.CodeParseError,
				"jsonEncoder received binary data. JSON uses text frames. Use a binary encoder for binary data.", "")
			continue
		}

		switch string(data) {
		case textPing:
			// Server answers client PINGs (ws.ts:466-470).
			c.enqueueText([]byte(textPong))
			continue
		case textPong:
			continue
		}

		msgs, terr := parseFrames(data)
		if terr != nil {
			// One PARSE_ERROR for the whole frame, id null, connection stays
			// open (ws.ts:488-509).
			c.enqueueError(nil, terr.Code, terr.Message, "")
			continue
		}
		for i := range msgs {
			c.dispatch(&msgs[i])
		}
	}
}

// dispatch handles one parsed message (ws.ts handleRequest).
func (c *conn) dispatch(msg *incomingMessage) {
	// `id` is required (ws.ts:135-155): null (or absent, normalized to null
	// here) yields a PARSE_ERROR frame; the connection stays open.
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		c.enqueueError(nil, trpc.CodeParseError, "`id` is required", "")
		return
	}
	switch msg.Method {
	case "subscription.stop":
		c.stopSubscription(msg)
	case "subscription":
		c.startSubscription(msg)
	default:
		// query/mutation: the Homarr SPA never sends them over WS (wsLink is
		// only in the subscription branch of the client splitLink,
		// apps/nextjs/.../trpc.tsx:193-205) and the registry is
		// subscription-only, so the router's unknown-procedure error applies
		// (tRPC router.ts callProcedure: `No "<type>"-procedure on path`).
		err := errNoProcedure(msg.Method, msg.Path)
		c.enqueueError(msg.ID, err.Code, err.Message, msg.Path)
	}
}

// startSubscription resolves and registers a new subscription
// (ws.ts handleRequest subscription branch).
func (c *conn) startSubscription(msg *incomingMessage) {
	if _, dup := c.subs[string(msg.ID)]; dup {
		// Duplicate request ids are rejected once the id is live
		// (ws.ts:245-253, BAD_REQUEST "Duplicate id <id>").
		c.enqueueError(msg.ID, trpc.CodeBadRequest, "Duplicate id "+rawIDDisplay(msg.ID), msg.Path)
		return
	}

	handler := c.hub.lookup(msg.Path)
	if handler == nil {
		err := errNoProcedure(msg.Method, msg.Path)
		c.enqueueError(msg.ID, err.Code, err.Message, msg.Path)
		return
	}

	sub, err := handler.Subscribe(c.ctx, &Request{
		Path:    msg.Path,
		Input:   msg.Input,
		Session: c.session,
	})
	if err != nil {
		// The procedure threw: error frame, connection stays open.
		c.enqueueErrorFromFailure(msg.ID, err, msg.Path)
		return
	}

	as := &activeSub{
		id:      msg.ID,
		pattern: sub.Pattern,
		decode:  sub.Decode,
		stopCh:  make(chan struct{}),
	}
	c.subs[string(msg.ID)] = as

	// Wire order: "started" first, then the optional initial payload, then
	// live bus data (upstream sends started after registering the
	// subscription and the observable emits only afterwards). The bus
	// subscription is armed before the pump starts so buffered messages are
	// forwarded, never raced.
	if as.pattern != "" && c.hub.bus != nil {
		as.busSub = c.hub.bus.Subscribe(as.pattern)
	}
	c.enqueueText(frameResultState(msg.ID, "started"))
	if sub.Initial != nil {
		wrapped, werr := trpc.WrapJSON(sub.Initial)
		if werr != nil {
			slog.Error("ws: initial payload is not serializable", "path", msg.Path, "error", werr)
		} else {
			c.enqueueText(frameData(msg.ID, wrapped))
		}
	}
	if as.busSub != nil {
		go c.pumpSub(as)
	}
}

// pumpSub forwards bus messages as data frames until the client stops the
// subscription or the connection dies. One goroutine per subscription; it
// only touches the socket through enqueue (writer-owned).
func (c *conn) pumpSub(as *activeSub) {
	decode := as.decode
	if decode == nil {
		decode = func(msg bus.Message) (any, bool) { return json.RawMessage(msg.Payload), true }
	}
	for {
		select {
		case <-as.stopCh:
			return
		case <-c.ctx.Done():
			return
		case m, ok := <-as.busSub.C:
			if !ok {
				return // bus closed
			}
			data, keep := decode(m)
			if !keep {
				continue
			}
			wrapped, err := trpc.WrapJSON(data)
			if err != nil {
				slog.Error("ws: bus payload is not serializable", "topic", m.Topic, "error", err)
				continue
			}
			c.enqueueText(frameData(as.id, wrapped))
		}
	}
}

// stopSubscription aborts one subscription (ws.ts:157-162: the
// subscription.stop notification aborts by id, unknown ids are a no-op) and
// answers "stopped" like the adapter does when its iterator is closed
// (ws.ts:337-345).
func (c *conn) stopSubscription(msg *incomingMessage) {
	as, ok := c.subs[string(msg.ID)]
	if !ok {
		return
	}
	delete(c.subs, string(msg.ID))
	close(as.stopCh)
	if as.busSub != nil {
		as.busSub.Unsubscribe()
	}
	c.enqueueText(frameResultState(msg.ID, "stopped"))
}

// enqueueErrorFromFailure maps a handler error to a tRPC error frame
// (trpc.Error → its code/message; anything else → INTERNAL_SERVER_ERROR,
// like getTRPCErrorFromUnknown).
func (c *conn) enqueueErrorFromFailure(id json.RawMessage, err error, path string) {
	if terr, ok := err.(*trpc.Error); ok {
		c.enqueueError(id, terr.Code, terr.Message, path)
		return
	}
	c.enqueueError(id, trpc.CodeInternalServerError, err.Error(), path)
}

func (c *conn) enqueueError(id json.RawMessage, code trpc.Code, message, path string) {
	frame, err := frameError(id, code, message, path)
	if err != nil {
		slog.Error("ws: failed to encode error frame", "error", err)
		return
	}
	c.enqueueText(frame)
}

func (c *conn) enqueueText(data []byte) {
	c.enqueue(outFrame{data: data})
}

// enqueue hands a frame to the writer goroutine. Never blocks: a full queue
// means the client cannot keep up, so the connection is dropped.
func (c *conn) enqueue(f outFrame) {
	select {
	case c.out <- f:
	case <-c.ctx.Done():
	default:
		slog.Warn("ws: outbound queue full, dropping slow connection")
		c.closeNow()
	}
}

// signalActivity pokes the keepalive loop (non-blocking, coalesced).
func (c *conn) signalActivity() {
	select {
	case c.activity <- struct{}{}:
	default:
	}
}

func (c *conn) closeNow() {
	_ = c.ws.CloseNow()
}

// writeLoop is the only goroutine that writes to the socket.
func (c *conn) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case f := <-c.out:
			wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, f.data)
			cancel()
			if err != nil {
				c.closeNow()
				return
			}
			if f.closeAfter {
				// Upstream closes without a status code (ws client.close(),
				// peer sees 1005); coder/websocket cannot send an empty
				// status, so the shutdown path uses GoingAway.
				_ = c.ws.Close(websocket.StatusGoingAway, "")
				return
			}
		}
	}
}

// keepaliveLoop ports handleKeepAlive (ws.ts:541-577): every PingInterval a
// text "PING" goes out; if no message at all arrives within PongTimeout of
// the ping, the connection is terminated. Any inbound message resets both
// timers.
func (c *conn) keepaliveLoop(pingInterval, pongTimeout time.Duration) {
	ping := time.NewTimer(pingInterval)
	defer ping.Stop()
	var pong *time.Timer
	defer func() {
		if pong != nil {
			pong.Stop()
		}
	}()
	var pongC <-chan time.Time
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.activity:
			if pong != nil {
				pong.Stop()
				pong = nil
				pongC = nil
			}
			if !ping.Stop() {
				select {
				case <-ping.C:
				default:
				}
			}
			ping.Reset(pingInterval)
		case <-ping.C:
			c.enqueueText([]byte(textPing))
			pong = time.NewTimer(pongTimeout)
			pongC = pong.C
		case <-pongC:
			// client.terminate() equivalent.
			c.closeNow()
			return
		}
	}
}

// rawIDDisplay renders a request id the way upstream interpolates it into
// messages: strings unquoted, everything else verbatim.
func rawIDDisplay(id json.RawMessage) string {
	raw := strings.TrimSpace(string(id))
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(id, &s); err == nil {
			return s
		}
	}
	return raw
}
