package ws

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

// --- test scaffolding ---

// fakeValidator resolves "good-token" to a session; anything else is
// anonymous (the upstream getSessionFromTokenAsync semantics).
type fakeValidator struct {
	failOn string // token that makes Validate return an internal error
}

func (f *fakeValidator) Validate(_ context.Context, token string) (*ctxauth.Session, error) {
	if token == "" {
		return nil, nil
	}
	if f.failOn != "" && token == f.failOn {
		return nil, io.ErrUnexpectedEOF
	}
	if token != "good-token" {
		return nil, nil
	}
	return &ctxauth.Session{UserID: "user-1", IsAdministrator: true}, nil
}

// fakeHandler is a SubscriptionHandler with fixed resolution results.
type fakeHandler struct {
	initial        any
	pattern        string
	decode         func(msg bus.Message) (any, bool)
	requireSession bool

	mu       sync.Mutex
	requests []*Request
}

func (f *fakeHandler) Subscribe(_ context.Context, req *Request) (*Subscription, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.requireSession && req.Session == nil {
		return nil, trpc.NewError(trpc.CodeUnauthorized, "Not signed in")
	}
	return &Subscription{Initial: f.initial, Pattern: f.pattern, Decode: f.decode}, nil
}

// lastRequest returns the most recent captured request.
func (f *fakeHandler) lastRequest() *Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

type hubFixture struct {
	hub *Hub
	srv *httptest.Server
}

func newHub(t *testing.T, opts Options, register func(h *Hub)) *hubFixture {
	t.Helper()
	h := New(bus.New(), &fakeValidator{}, opts)
	if register != nil {
		register(h)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		h.Close()
		srv.Close()
	})
	return &hubFixture{hub: h, srv: srv}
}

type testClient struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

func dialWS(t *testing.T, srv *httptest.Server, header http.Header) *testClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/websockets"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &testClient{t: t, conn: conn, ctx: ctx}
}

func (c *testClient) send(msg string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		c.t.Fatalf("send %q: %v", msg, err)
	}
}

// readFrame reads one text frame or fails the test after the timeout.
func (c *testClient) readFrame(timeout time.Duration) string {
	c.t.Helper()
	frame, err := c.tryReadFrame(timeout)
	if err != nil {
		c.t.Fatalf("read frame: %v", err)
	}
	return frame
}

// tryReadFrame reads one text frame; a timeout (or any read error) is
// returned to the caller.
func (c *testClient) tryReadFrame(timeout time.Duration) (string, error) {
	rctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	typ, r, err := c.conn.Reader(rctx)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("expected text frame, got %v", typ)
	}
	return string(data), nil
}

// expectClose reads until the connection closes and returns the close code.
func (c *testClient) expectClose(timeout time.Duration) websocket.StatusCode {
	c.t.Helper()
	for {
		_, err := c.tryReadFrame(timeout)
		if err == nil {
			continue
		}
		if ctxErr := c.ctx.Err(); ctxErr != nil {
			c.t.Fatalf("context expired while waiting for close: %v", ctxErr)
		}
		return websocket.CloseStatus(err)
	}
}

// expectPong proves the connection is alive AND that no other frame was
// queued in front of the PONG reply (per-connection frames are FIFO): any
// unexpected frame arrives before the PONG and fails the assertion. This is
// the silent-check primitive: a read-context timeout would itself close a
// coder/websocket connection, so liveness must be probed positively.
func (c *testClient) expectPong(timeout time.Duration) {
	c.t.Helper()
	c.send(textPing)
	if got := c.readFrame(timeout); got != textPong {
		c.t.Fatalf("expected PONG probe, got %q", got)
	}
}

func subscribeMsg(id int, path, input string) string {
	if input == "" {
		return `{"id":` + itoa(id) + `,"method":"subscription","params":{"path":"` + path + `"}}`
	}
	return `{"id":` + itoa(id) + `,"method":"subscription","params":{"path":"` + path + `","input":{"json":` + input + `}}}`
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// --- tests ---

// An upgrade without a session cookie is ACCEPTED (upstream builds an
// anonymous context, apps/websocket/src/main.ts:40-66); the rejection
// happens per subscription, as an UNAUTHORIZED error frame from the
// handler, and the connection stays open.
func TestAnonymousUpgradeAcceptedAndSubscriptionUnauthorized(t *testing.T) {
	handler := &fakeHandler{requireSession: true}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", handler) })

	c := dialWS(t, f.srv, nil)
	c.send(subscribeMsg(1, "log.subscribe", ""))
	got := c.readFrame(5 * time.Second)
	want := `{"id":1,"error":{"json":{"message":"Not signed in","code":-32001,"data":{"code":"UNAUTHORIZED","httpStatus":401,"path":"log.subscribe"}}}}`
	if got != want {
		t.Fatalf("unauthorized frame:\n got %s\nwant %s", got, want)
	}
	// The connection stays open: a second request is still answered.
	c.send(subscribeMsg(2, "log.subscribe", ""))
	got = c.readFrame(5 * time.Second)
	if !strings.Contains(got, `"id":2`) {
		t.Fatalf("connection did not stay open after an error frame: %s", got)
	}
}

// A validated session cookie flows into the handler as ctxauth.Session.
func TestSessionCookieValidatedOnUpgrade(t *testing.T) {
	handler := &fakeHandler{}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", handler) })

	c := dialWS(t, f.srv, http.Header{"Cookie": []string{"homarr.session-token=good-token"}})
	c.send(subscribeMsg(1, "log.subscribe", ""))
	c.readFrame(5 * time.Second) // started

	sess := handler.lastRequest().Session
	if sess == nil || sess.UserID != "user-1" {
		t.Fatalf("session not propagated: %+v", sess)
	}
}

// An invalid session token behaves like anonymous (upstream returns null
// from getSessionFromTokenAsync); a validator internal error also degrades
// to anonymous, like the createContext catch (main.ts:59-65).
func TestInvalidOrFailingTokenIsAnonymous(t *testing.T) {
	handler := &fakeHandler{}
	h := New(bus.New(), &fakeValidator{failOn: "boom"}, Options{})
	h.Register("log.subscribe", handler)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })

	for _, token := range []string{"bad-token", "boom"} {
		c := dialWS(t, srv, http.Header{"Cookie": []string{"homarr.session-token=" + token}})
		c.send(subscribeMsg(1, "log.subscribe", ""))
		c.readFrame(5 * time.Second) // started (anonymous)
		if sess := handler.lastRequest().Session; sess != nil {
			t.Fatalf("token %q: expected anonymous session, got %+v", token, sess)
		}
	}
}

// API keys are rejected: an UNAUTHORIZED error frame with id null, then the
// server closes (upstream throws in createContext; the adapter responds and
// closes, main.ts:41-43 + ws.ts createCtxPromise).
func TestAPIKeyRejected(t *testing.T) {
	f := newHub(t, Options{}, nil)
	c := dialWS(t, f.srv, http.Header{"ApiKey": []string{"1.some-token"}})
	got := c.readFrame(5 * time.Second)
	want := `{"id":null,"error":{"json":{"message":"API keys are not supported over WebSockets","code":-32001,"data":{"code":"UNAUTHORIZED","httpStatus":401}}}}`
	if got != want {
		t.Fatalf("api key frame:\n got %s\nwant %s", got, want)
	}
	if code := c.expectClose(5 * time.Second); code != websocket.StatusGoingAway {
		t.Fatalf("close code after api key rejection: %v", code)
	}
}

// Full subscription flow: started, initial payload, bus messages forwarded
// as superjson-wrapped data frames, stopped on client stop, no frames after
// stop.
func TestSubscribeInitialBusForwardingAndStop(t *testing.T) {
	handler := &fakeHandler{
		initial: json.RawMessage(`{"type":"history","messages":[]}`),
		pattern: "events.log.*",
	}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", handler) })

	c := dialWS(t, f.srv, nil)
	c.send(subscribeMsg(1, "log.subscribe", `{"levels":["info"]}`))

	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"started"}}`; got != want {
		t.Fatalf("started frame:\n got %s\nwant %s", got, want)
	}
	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"data","data":{"json":{"type":"history","messages":[]}}}}`; got != want {
		t.Fatalf("initial frame:\n got %s\nwant %s", got, want)
	}

	// A matching bus message becomes a data frame.
	f.hub.bus.Publish("events.log.info", []byte(`{"level":"info","message":"hi"}`))
	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"data","data":{"json":{"level":"info","message":"hi"}}}}`; got != want {
		t.Fatalf("bus frame:\n got %s\nwant %s", got, want)
	}

	// Stop: the server answers "stopped".
	c.send(`{"id":1,"method":"subscription.stop"}`)
	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"stopped"}}`; got != want {
		t.Fatalf("stopped frame:\n got %s\nwant %s", got, want)
	}

	// No more frames after stop: the PONG probe fails if any data frame was
	// queued after the stop.
	f.hub.bus.Publish("events.log.info", []byte(`{"level":"info"}`))
	c.expectPong(5 * time.Second)

	// Stopping an unknown id is a silent no-op (ws.ts:157-162): no "stopped"
	// frame for id 99 either.
	c.send(`{"id":99,"method":"subscription.stop"}`)
	c.expectPong(5 * time.Second)
}

// Decode filters bus messages (the log levels filter) and nil Initial sends
// no initial frame.
func TestDecodeFilterDropsMessages(t *testing.T) {
	handler := &fakeHandler{
		pattern: "events.cronjobs.status.*",
		decode: func(msg bus.Message) (any, bool) {
			var probe struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(msg.Payload, &probe); err != nil {
				return nil, false
			}
			if probe.Name != "ping" {
				return nil, false // drop everything else (levels-style filter)
			}
			return json.RawMessage(msg.Payload), true
		},
	}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("cronJobs.subscribeToStatusUpdates", handler) })

	c := dialWS(t, f.srv, nil)
	c.send(subscribeMsg(7, "cronJobs.subscribeToStatusUpdates", ""))
	if got, want := c.readFrame(5*time.Second), `{"id":7,"result":{"type":"started"}}`; got != want {
		t.Fatalf("started frame:\n got %s\nwant %s", got, want)
	}

	f.hub.bus.Publish("events.cronjobs.status.docker", []byte(`{"name":"docker","status":"running"}`))
	f.hub.bus.Publish("events.cronjobs.status.ping", []byte(`{"name":"ping","status":"success"}`))
	got := c.readFrame(5 * time.Second)
	want := `{"id":7,"result":{"type":"data","data":{"json":{"name":"ping","status":"success"}}}}`
	if got != want {
		t.Fatalf("filtered frame:\n got %s\nwant %s", got, want)
	}
	// The dropped message never surfaces: the next frame after the ping one
	// is the PONG probe, not another data frame.
	c.expectPong(5 * time.Second)
}

// Unknown subscription path → the router's NOT_FOUND error, byte-exact.
func TestUnknownSubscriptionPath(t *testing.T) {
	f := newHub(t, Options{}, nil)
	c := dialWS(t, f.srv, nil)
	c.send(subscribeMsg(3, "missing.sub", ""))
	got := c.readFrame(5 * time.Second)
	want := `{"id":3,"error":{"json":{"message":"No \"subscription\"-procedure on path \"missing.sub\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"missing.sub"}}}}`
	if got != want {
		t.Fatalf("unknown path frame:\n got %s\nwant %s", got, want)
	}
}

// query/mutation are never sent over WS by the SPA; the registry is
// subscription-only, so they get the router's NOT_FOUND for their type.
func TestQueryAndMutationOverWSAreNotFound(t *testing.T) {
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", &fakeHandler{}) })
	c := dialWS(t, f.srv, nil)

	c.send(`{"id":1,"method":"query","params":{"path":"log.subscribe","input":{"json":{}}}}`)
	got := c.readFrame(5 * time.Second)
	want := `{"id":1,"error":{"json":{"message":"No \"query\"-procedure on path \"log.subscribe\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"log.subscribe"}}}}`
	if got != want {
		t.Fatalf("query frame:\n got %s\nwant %s", got, want)
	}

	c.send(`{"id":2,"method":"mutation","params":{"path":"log.subscribe"}}`)
	got = c.readFrame(5 * time.Second)
	want = `{"id":2,"error":{"json":{"message":"No \"mutation\"-procedure on path \"log.subscribe\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"log.subscribe"}}}}`
	if got != want {
		t.Fatalf("mutation frame:\n got %s\nwant %s", got, want)
	}
}

// A duplicate request id on a live subscription → BAD_REQUEST "Duplicate id".
func TestDuplicateSubscriptionID(t *testing.T) {
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", &fakeHandler{pattern: "events.log.*"}) })
	c := dialWS(t, f.srv, nil)

	c.send(subscribeMsg(1, "log.subscribe", ""))
	c.readFrame(5 * time.Second) // started
	c.send(subscribeMsg(1, "log.subscribe", ""))
	got := c.readFrame(5 * time.Second)
	want := `{"id":1,"error":{"json":{"message":"Duplicate id 1","code":-32600,"data":{"code":"BAD_REQUEST","httpStatus":400,"path":"log.subscribe"}}}}`
	if got != want {
		t.Fatalf("duplicate id frame:\n got %s\nwant %s", got, want)
	}
}

// A batch array of subscribe messages is answered per message (the client
// batches flushes, wsClient.ts:432 send of an array).
func TestBatchArrayFrames(t *testing.T) {
	f := newHub(t, Options{}, func(h *Hub) {
		h.Register("a.sub", &fakeHandler{})
		h.Register("b.sub", &fakeHandler{})
	})
	c := dialWS(t, f.srv, nil)
	c.send(`[` + subscribeMsg(1, "a.sub", "") + `,` + subscribeMsg(2, "b.sub", "") + `]`)
	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"started"}}`; got != want {
		t.Fatalf("batch frame 0:\n got %s\nwant %s", got, want)
	}
	if got, want := c.readFrame(5*time.Second), `{"id":2,"result":{"type":"started"}}`; got != want {
		t.Fatalf("batch frame 1:\n got %s\nwant %s", got, want)
	}
}

// Malformed JSON → a single PARSE_ERROR frame with id null and the
// connection stays usable (ws.ts:488-509).
func TestMalformedJSONFrame(t *testing.T) {
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", &fakeHandler{}) })
	c := dialWS(t, f.srv, nil)

	c.send(`{"id":1,"method":`)
	got := c.readFrame(5 * time.Second)
	if !strings.HasPrefix(got, `{"id":null,"error":{"json":{"message":"`) ||
		!strings.Contains(got, `"code":-32700`) ||
		!strings.Contains(got, `"data":{"code":"PARSE_ERROR","httpStatus":400}}}}`) {
		t.Fatalf("parse error frame: %s", got)
	}

	// Still alive: a valid request works afterwards.
	c.send(subscribeMsg(1, "log.subscribe", ""))
	if got, want := c.readFrame(5*time.Second), `{"id":1,"result":{"type":"started"}}`; got != want {
		t.Fatalf("frame after malformed input:\n got %s\nwant %s", got, want)
	}
}

// Protocol-level structural errors, byte-exact against parseTRPCMessage and
// handleRequest.
func TestProtocolErrorFrames(t *testing.T) {
	f := newHub(t, Options{}, nil)
	c := dialWS(t, f.srv, nil)

	cases := []struct {
		name string
		send string
		want string
	}{
		{
			name: "null id",
			send: `{"id":null,"method":"subscription","params":{"path":"log.subscribe"}}`,
			want: `{"id":null,"error":{"json":{"message":"` + "`id` is required" + `","code":-32700,"data":{"code":"PARSE_ERROR","httpStatus":400}}}}`,
		},
		{
			name: "invalid method",
			send: `{"id":1,"method":"bogus","params":{"path":"log.subscribe"}}`,
			want: `{"id":null,"error":{"json":{"message":"Invalid procedure type","code":-32700,"data":{"code":"PARSE_ERROR","httpStatus":400}}}}`,
		},
		{
			name: "params not an object",
			send: `{"id":1,"method":"subscription","params":"nope"}`,
			want: `{"id":null,"error":{"json":{"message":"Not an object","code":-32700,"data":{"code":"PARSE_ERROR","httpStatus":400}}}}`,
		},
		{
			name: "path not a string",
			send: `{"id":1,"method":"subscription","params":{"path":42}}`,
			want: `{"id":null,"error":{"json":{"message":"Invalid string","code":-32700,"data":{"code":"PARSE_ERROR","httpStatus":400}}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.send(tc.send)
			if got := c.readFrame(5 * time.Second); got != tc.want {
				t.Fatalf("frame:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
	// Connection survived all of them.
	c.send(`{"id":5,"method":"subscription","params":{"path":"missing"}}`)
	if got := c.readFrame(5 * time.Second); !strings.Contains(got, `-32004`) {
		t.Fatalf("connection did not survive protocol errors: %s", got)
	}
}

// lastEventId on resume is merged into the input object
// (ws.ts:171-180) — visible to the handler.
func TestLastEventIDMergedIntoInput(t *testing.T) {
	handler := &fakeHandler{}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", handler) })
	c := dialWS(t, f.srv, nil)

	c.send(`{"id":1,"method":"subscription","params":{"path":"log.subscribe","input":{"json":{"levels":["info"]}},"lastEventId":"evt-42"}}`)
	c.readFrame(5 * time.Second) // started

	req := handler.lastRequest()
	if req == nil || string(req.Input) != `{"levels":["info"],"lastEventId":"evt-42"}` {
		t.Fatalf("input after lastEventId merge: %+v", req)
	}
}

// Server keepalive: text PING arrives, a PONG reply keeps the connection
// alive (the next PING arrives too).
func TestKeepalivePingPongExchange(t *testing.T) {
	f := newHub(t, Options{KeepAlive: &KeepAlive{PingInterval: 100 * time.Millisecond, PongTimeout: 5 * time.Second}}, nil)
	c := dialWS(t, f.srv, nil)

	first := c.readFrame(5 * time.Second)
	if first != textPing {
		t.Fatalf("first keepalive frame: %q", first)
	}
	c.send(textPong) // the client answers PONG (wsConnection.ts:178-182)

	second := c.readFrame(5 * time.Second)
	if second != textPing {
		t.Fatalf("second keepalive frame: %q", second)
	}
}

// Silence after a ping terminates the connection (pongWaitMs, main.ts:73).
func TestKeepalivePongTimeoutTerminates(t *testing.T) {
	f := newHub(t, Options{KeepAlive: &KeepAlive{PingInterval: 50 * time.Millisecond, PongTimeout: 100 * time.Millisecond}}, nil)
	c := dialWS(t, f.srv, nil)

	if got := c.readFrame(5 * time.Second); got != textPing {
		t.Fatalf("expected PING, got %q", got)
	}
	// Do not answer: the server must terminate the connection. terminate()
	// sends no close frame, so the client sees an abrupt close — coder's
	// CloseStatus reports -1 when no close frame arrived (a browser reports
	// 1006 here).
	code := c.expectClose(5 * time.Second)
	if code != websocket.StatusAbnormalClosure && code != -1 {
		t.Fatalf("close status after pong timeout: %v", code)
	}
}

// Hub.Close broadcasts the reconnect notification and closes every
// connection (SIGTERM sequence, main.ts:84-88).
func TestCloseBroadcastsReconnectNotification(t *testing.T) {
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", &fakeHandler{}) })
	c1 := dialWS(t, f.srv, nil)
	c2 := dialWS(t, f.srv, nil)

	f.hub.Close()

	for _, c := range []*testClient{c1, c2} {
		if got, want := c.readFrame(5*time.Second), `{"id":null,"method":"reconnect"}`; got != want {
			t.Fatalf("reconnect frame:\n got %s\nwant %s", got, want)
		}
		if code := c.expectClose(5 * time.Second); code != websocket.StatusGoingAway {
			t.Fatalf("close code after hub close: %v", code)
		}
	}

	// A closed hub refuses further upgrades with 503.
	resp, err := http.Get(f.srv.URL + "/websockets")
	if err != nil {
		t.Fatalf("upgrade after close: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade after close status: %d", resp.StatusCode)
	}
}

// Disconnecting cleans up: after the client vanishes, a bus publish must
// not panic and the subscription must be released (unsubscribe on
// disconnect).
func TestDisconnectUnsubscribesFromBus(t *testing.T) {
	handler := &fakeHandler{pattern: "events.log.*"}
	f := newHub(t, Options{}, func(h *Hub) { h.Register("log.subscribe", handler) })

	c := dialWS(t, f.srv, nil)
	c.send(subscribeMsg(1, "log.subscribe", ""))
	c.readFrame(5 * time.Second) // started

	_ = c.conn.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.hub.mu.Lock()
		n := len(f.hub.conns)
		f.hub.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.hub.mu.Lock()
	n := len(f.hub.conns)
	f.hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("connection not cleaned up after disconnect (%d left)", n)
	}
	// Publishing to the bus with the dead subscription removed must be safe.
	f.hub.bus.Publish("events.log.info", []byte(`{"level":"info"}`))
}
