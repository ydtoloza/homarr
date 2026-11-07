// Package ws is the Go port of the Homarr WebSocket server
// (apps/websocket): the tRPC v11 WS endpoint the SPA's wsLink talks to at
// /websockets. The exact protocol contract, with upstream file:line
// citations, lives in WEBSOCKET.md next to this file.
//
// What is implemented:
//   - the tRPC WS wire: "subscription" / "subscription.stop" methods with
//     superjson payloads, "started"/"data"/"stopped" and error frames,
//     batch arrays, the "reconnect" shutdown notification
//   - session-cookie auth on upgrade (API keys rejected like upstream)
//   - server keepalive: text PING every 30s, terminate after 5s of silence
//   - a registry mapping tRPC paths to SubscriptionHandlers fed by the
//     in-process bus (the replacement for upstream's Redis channels)
//
// Not implemented (documented in WEBSOCKET.md): query/mutation execution
// over WS (the SPA never sends them) and the SSE httpSubscriptionLink
// endpoint for widget.beszel.subscribeSystemStats, which is HTTP-side.
package ws

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

// DefaultCookiePrefix is the session cookie name prefix
// (packages/auth/session.ts:11, "homarr.session-token").
const DefaultCookiePrefix = "homarr"

// KeepAlive configures the server heartbeat. The zero Duration falls back
// to the upstream value.
type KeepAlive struct {
	// PingInterval is how often the server sends the "PING" text frame
	// (upstream pingMs: 30000, apps/websocket/src/main.ts:71).
	PingInterval time.Duration
	// PongTimeout is how long the server waits for ANY inbound message after
	// a ping before terminating the connection (upstream pongWaitMs: 5000,
	// main.ts:73).
	PongTimeout time.Duration
}

func (k KeepAlive) withDefaults() KeepAlive {
	if k.PingInterval <= 0 {
		k.PingInterval = 30 * time.Second
	}
	if k.PongTimeout <= 0 {
		k.PongTimeout = 5 * time.Second
	}
	return k
}

// UpstreamKeepAlive returns the configuration apps/websocket enables
// (main.ts:68-74).
func UpstreamKeepAlive() *KeepAlive {
	return &KeepAlive{PingInterval: 30 * time.Second, PongTimeout: 5 * time.Second}
}

// Options configures the Hub.
type Options struct {
	// CookiePrefix overrides the session cookie name prefix. Default
	// DefaultCookiePrefix ("homarr"), matching AUTH_COOKIE_PREFIX.
	CookiePrefix string
	// KeepAlive enables the server heartbeat when non-nil. Upstream enables
	// it (main.ts:68-74); tests pass short intervals. nil = disabled.
	KeepAlive *KeepAlive
}

func (o *Options) cookiePrefix() string {
	if o.CookiePrefix != "" {
		return o.CookiePrefix
	}
	return DefaultCookiePrefix
}

// keepAlive normalizes the configured heartbeat; nil means disabled.
func (o *Options) keepAlive() *KeepAlive {
	if o.KeepAlive == nil {
		return nil
	}
	k := o.KeepAlive.withDefaults()
	return &k
}

// SessionValidator resolves a session token to a caller identity. The
// integrator adapts internal/auth onto it (auth.Auth.ValidateSession plus
// the permission lookup of auth.callerSession); tests use a fake.
//
// Semantics mirror upstream getSessionFromTokenAsync
// (packages/auth/session.ts:21-46): missing/expired/unknown tokens return
// (nil, nil) — the connection proceeds anonymous and each subscription
// handler enforces its own authorization. Internal failures return an
// error; the hub logs it and proceeds anonymous exactly like upstream's
// createContext catch (apps/websocket/src/main.ts:59-65).
type SessionValidator interface {
	Validate(ctx context.Context, token string) (*ctxauth.Session, error)
}

// SessionValidatorFunc adapts a function to SessionValidator.
type SessionValidatorFunc func(ctx context.Context, token string) (*ctxauth.Session, error)

// Validate implements SessionValidator.
func (f SessionValidatorFunc) Validate(ctx context.Context, token string) (*ctxauth.Session, error) {
	return f(ctx, token)
}

// Hub serves the /websockets tRPC endpoint. It owns the connection set and
// the subscription registry; the event bus provides the data that flows to
// the clients.
type Hub struct {
	bus      *bus.Bus
	sessions SessionValidator
	opts     Options

	mu       sync.Mutex
	handlers map[string]SubscriptionHandler
	conns    map[*conn]struct{}
	closed   bool
	// inflight counts handler goroutines that passed the closed-gate but may
	// not have registered their connection yet, so Close's snapshot of
	// h.conns cannot miss a client that completed its upgrade.
	inflight sync.WaitGroup
}

// New builds a Hub. eventBus is the shared pubsub (nil disables bus
// delivery; only initial payloads would reach clients). sessions resolves
// the session cookie; opts may be zero-valued.
func New(eventBus *bus.Bus, sessions SessionValidator, opts Options) *Hub {
	return &Hub{
		bus:      eventBus,
		sessions: sessions,
		opts:     opts,
		handlers: make(map[string]SubscriptionHandler),
		conns:    make(map[*conn]struct{}),
	}
}

// Register maps a tRPC subscription path (e.g. "log.subscribe") to its
// handler. Safe to call while serving; paths registered after a client
// subscribed do not retroactively affect it.
func (h *Hub) Register(path string, s SubscriptionHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[path] = s
}

func (h *Hub) lookup(path string) SubscriptionHandler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handlers[path]
}

func (h *Hub) addConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
}

func (h *Hub) removeConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

// Handler returns the HTTP handler that performs the WebSocket upgrade and
// runs the connection. The integrator mounts it at /websockets (the path
// nginx proxies to the upstream WS app, nginx.conf:13).
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count the handler BEFORE the gate so Close always waits for
		// upgrades that are in progress (Add-then-check ordering).
		h.inflight.Add(1)
		defer h.inflight.Done()

		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			// Upstream closes the whole server on SIGTERM (main.ts:87
			// wss.close()), so new handshakes fail to connect; 503 is the
			// HTTP-level equivalent once the route is still mounted.
			http.Error(w, "websocket server is shutting down", http.StatusServiceUnavailable)
			return
		}
		h.mu.Unlock()

		// No Origin check: upstream's bare WebSocketServer validates none
		// (apps/websocket/src/main.ts:13-15); access control is the session
		// cookie plus per-subscription permissions.
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return // Accept already wrote the HTTP error
		}

		c := &conn{
			hub:      h,
			ws:       wsConn,
			out:      make(chan outFrame, outQueueSize),
			activity: make(chan struct{}, 1),
			subs:     make(map[string]*activeSub),
			session:  h.sessionFromRequest(r),
			// API keys are not supported over WebSockets: upstream throws in
			// createContext, the adapter answers an UNAUTHORIZED frame and
			// closes (apps/websocket/src/main.ts:41-43, ws.ts:99-135).
			rejectAPIKey: r.Header.Get("ApiKey") != "",
		}
		c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(r.Context()))

		// Register while h.closed is still false (the gate above guaranteed
		// it) so Close's snapshot covers this connection.
		h.mu.Lock()
		h.conns[c] = struct{}{}
		h.mu.Unlock()

		go c.run()
	})
}

// sessionFromRequest resolves the session cookie after the upgrade, like
// upstream createContext (apps/websocket/src/main.ts:40-66). Anonymous and
// failed validations both yield nil (see SessionValidator).
func (h *Hub) sessionFromRequest(r *http.Request) *ctxauth.Session {
	if h.sessions == nil {
		return nil
	}
	ck, err := r.Cookie(h.opts.cookiePrefix() + ".session-token")
	if err != nil || ck.Value == "" {
		return nil
	}
	sess, err := h.sessions.Validate(r.Context(), ck.Value)
	if err != nil {
		slog.Error("ws: session validation failed", "error", err)
		return nil
	}
	return sess
}

// Close ports the SIGTERM sequence (apps/websocket/src/main.ts:84-88):
// broadcast the reconnect notification to every open connection, then close
// them. Further upgrades answer 503 until the process exits. Safe to call
// multiple times.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()

	// Every handler past the gate registers before returning, so the
	// snapshot below cannot miss an accepted connection.
	h.inflight.Wait()

	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	reconnect := frameReconnect()
	for _, c := range conns {
		// broadcastReconnectNotification (ws.ts:617-627) sends
		// {"id":null,"method":"reconnect"} to every open client; the close
		// right after replaces main.ts wss.close().
		c.enqueueText(reconnect)
		c.enqueue(outFrame{closeAfter: true})
	}
}

// sessionTokenCookieName builds the cookie name the way upstream does
// (packages/auth/session.ts:11): "<prefix>.session-token". Exported for the
// integrator's SessionValidator adapter tests.
func SessionTokenCookieName(prefix string) string {
	if prefix == "" {
		prefix = DefaultCookiePrefix
	}
	return strings.TrimSuffix(prefix, ".") + ".session-token"
}
