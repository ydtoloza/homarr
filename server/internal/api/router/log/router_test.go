package log_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/log"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/logging"
	"github.com/ydtoloza/homarr/server/internal/ws"
)

// Test harness: the log router over a real trpc.Router (SSE side) and the
// ws.SubscriptionHandler map (WS side), with header-driven session injection
// standing in for the auth middleware.

type testAPI struct {
	db     *db.DB
	bus    *bus.Bus
	store  *logging.SQLiteStore
	router *trpc.Router
	svc    *log.Service
	srv    *httptest.Server
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	d := db.OpenTest(t)
	ctx := context.Background()
	store, err := logging.NewSQLiteStore(ctx, d)
	if err != nil {
		t.Fatalf("logging.NewSQLiteStore: %v", err)
	}
	eventBus := bus.New()
	t.Cleanup(eventBus.Close)

	r := trpc.New()
	r.Bus = eventBus
	svc := log.Register(r, d, log.Options{Store: store})

	// The WS contract (the composition root mirrors this against the hub).
	for path := range svc.SubscriptionHandlers() {
		if path != "log.subscribe" {
			t.Fatalf("unexpected ws handler path %q", path)
		}
	}

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if session := req.Header.Get("X-Test-Session"); session != "" {
			var s *ctxauth.Session
			if err := json.Unmarshal([]byte(session), &s); err != nil {
				t.Fatalf("bad test session header: %v", err)
			}
			req = req.WithContext(ctxauth.WithSession(req.Context(), s))
		}
		r.Handler().ServeHTTP(w, req)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{db: d, bus: eventBus, store: store, router: r, svc: svc, srv: srv}
}

// seedUser creates a user row; admin sets the fork administrator flag.
func (a *testAPI) seedUser(t *testing.T, id string, admin bool) *ctxauth.Session {
	t.Helper()
	if err := a.db.CreateUser(context.Background(), db.User{ID: id, Name: id, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
	return &ctxauth.Session{ID: "session-" + id, UserID: id, Name: id, IsAdministrator: admin}
}

// seedGroupPermission grants one permission key through a group membership.
func (a *testAPI) seedGroupPermission(t *testing.T, userID, permission string) {
	t.Helper()
	ctx := context.Background()
	groupID := "group-" + permission
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "group" (id, name, position) VALUES (?, ?, 1)`, groupID, permission); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, groupID, userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`, groupID, permission); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
}

// seedLog writes one entry through the real pipeline store.
func (a *testAPI) seedLog(t *testing.T, id string, ts time.Time, level logging.Level, message string) logging.Entry {
	t.Helper()
	e := logging.Entry{ID: id, Timestamp: ts, Message: message, Level: level}
	if err := a.store.Save(context.Background(), e); err != nil {
		t.Fatalf("seed log %s: %v", id, err)
	}
	return e
}

// sseGet opens a streaming request and returns (status, headers, body).
func (a *testAPI) sseGet(t *testing.T, target string, session *ctxauth.Session) (int, http.Header, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.srv.URL+target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if session != nil {
		raw, _ := json.Marshal(session)
		req.Header.Set("X-Test-Session", string(raw))
	}
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(raw)
}

// decodeEvent payloads: the SSE body is a sequence of event blocks; this
// helper extracts the data lines of named/unnamed events in order.
func splitEvents(t *testing.T, body string) []string {
	t.Helper()
	blocks := strings.Split(body, "\n\n\n")
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// The permission gate of permissionRequiredProcedure
// .requiresPermission("other-view-logs"): anonymous UNAUTHORIZED, signed-in
// non-privileged FORBIDDEN "Permission denied", grant via group OK.
func TestSubscribeGates(t *testing.T) {
	a := newTestAPI(t)

	// Anonymous: UNAUTHORIZED.
	status, _, body := a.sseGet(t, "/api/trpc/log.subscribe", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (errors ride the stream)", status)
	}
	if !strings.Contains(body, `"code":"UNAUTHORIZED"`) || !strings.Contains(body, `"code":-32001`) {
		t.Errorf("body = %s, want UNAUTHORIZED serialized-error", body)
	}

	// Signed-in without the permission: FORBIDDEN "Permission denied".
	plain := a.seedUser(t, "plain", false)
	status, _, body = a.sseGet(t, "/api/trpc/log.subscribe", plain)
	if !strings.Contains(body, `"message":"Permission denied"`) {
		t.Errorf("body = %s, want Permission denied", body)
	}

	// The grant via group permission (the expanded key other-view-logs).
	granted := a.seedUser(t, "granted", false)
	a.seedGroupPermission(t, "granted", "other-view-logs")
	status, _, body = a.sseGet(t, "/api/trpc/log.subscribe", granted)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if !strings.Contains(body, `"type":"history"`) {
		t.Errorf("body = %s, want history event", body)
	}

	// The fork administrator flag passes too.
	root := a.seedUser(t, "root", true)
	status, _, body = a.sseGet(t, "/api/trpc/log.subscribe", root)
	if !strings.Contains(body, `"type":"history"`) {
		t.Errorf("body = %s, want history event", body)
	}
}

// The Initial history event: level-filtered, oldest first, with the superjson
// Date annotation the SPA terminal requires.
func TestSubscribeHistoryAndFilter(t *testing.T) {
	a := newTestAPI(t)
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	a.seedLog(t, "e1", base, logging.LevelError, "first error")
	a.seedLog(t, "i1", base.Add(time.Millisecond), logging.LevelInfo, "an info line")
	a.seedLog(t, "e2", base.Add(2*time.Millisecond), logging.LevelError, "second error")

	admin := a.seedUser(t, "root", true)
	input := urlQuery(t, map[string]any{"levels": []string{"error"}})
	status, header, body := a.sseGet(t, "/api/trpc/log.subscribe?input="+input, admin)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if ct := header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	events := splitEvents(t, body)
	if len(events) < 2 {
		t.Fatalf("events = %v, want connected + history", events)
	}
	if events[0] != "event: connected\ndata: {}" {
		t.Errorf("connected event = %q", events[0])
	}
	// The history data event: only the two error entries, oldest first, each
	// timestamp annotated as Date.
	wantHistory := "data: {\"json\":{\"type\":\"history\",\"messages\":[" +
		`{"id":"e1","timestamp":"2026-11-07T10:00:00.000Z","message":"first error","level":"error"},` +
		`{"id":"e2","timestamp":"2026-11-07T10:00:00.002Z","message":"second error","level":"error"}]},` +
		`"meta":{"values":{"messages.0.timestamp":"Date","messages.1.timestamp":"Date"},"v":1}}`
	if events[1] != wantHistory {
		t.Errorf("history event =\n%s\nwant\n%s", events[1], wantHistory)
	}
}

// Live tail: bus publishes stream as {type:"message"} data events, filtered
// by the requested levels; the payload carries the Date annotation.
func TestSubscribeLiveTail(t *testing.T) {
	a := newTestAPI(t)
	admin := a.seedUser(t, "root", true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.srv.URL+"/api/trpc/log.subscribe?input="+urlQuery(t, map[string]any{"levels": []string{"info", "error"}}), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	raw, _ := json.Marshal(admin)
	req.Header.Set("X-Test-Session", string(raw))
	res, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	buf := make([]byte, 8192)
	// Read the connected + history events.
	total := 0
	for total < len("event: connected\ndata: {}\n\n\ndata: {\"json\":{\"type\":\"history\",\"messages\":[]}") {
		n, err := res.Body.Read(buf[total:])
		if n > 0 {
			total += n
		}
		if err != nil && total == 0 {
			t.Fatalf("read stream: %v", err)
		}
		if total == 0 {
			continue
		}
	}

	// Publish one info and one warn entry (warn not requested).
	infoPayload, _ := logging.MarshalMessage(logging.Entry{
		ID: "live-1", Timestamp: time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
		Message: "live info", Level: logging.LevelInfo,
	})
	warnPayload, _ := logging.MarshalMessage(logging.Entry{
		ID: "live-2", Timestamp: time.Date(2026, 11, 7, 11, 0, 1, 0, time.UTC),
		Message: "live warn", Level: logging.LevelWarn,
	})
	a.bus.Publish("events.log.info", infoPayload)
	a.bus.Publish("events.log.warn", warnPayload)

	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, err := res.Body.Read(buf[total:])
		if n > 0 {
			total += n
			got = string(buf[:total])
			if strings.Contains(got, `"type":"message"`) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, `"type":"message","message":{"id":"live-1","timestamp":"2026-11-07T11:00:00.000Z","message":"live info","level":"info"}`) {
		t.Errorf("stream = %s, want the live info message", got)
	}
	if strings.Contains(got, "live warn") {
		t.Errorf("stream = %s, the warn level must be filtered out", got)
	}
	if !strings.Contains(got, `"values":{"message.timestamp":"Date"},"v":1`) {
		t.Errorf("stream = %s, want the Date annotation of the live message", got)
	}
}

// Invalid levels fail the zod enum (BAD_REQUEST serialized-error).
func TestSubscribeInvalidLevel(t *testing.T) {
	a := newTestAPI(t)
	root := a.seedUser(t, "root", true)
	input := urlQuery(t, map[string]any{"levels": []string{"verbose"}})
	_, _, body := a.sseGet(t, "/api/trpc/log.subscribe?input="+input, root)
	if !strings.Contains(body, `"code":"BAD_REQUEST"`) || !strings.Contains(body, "Invalid enum value for levels") {
		t.Errorf("body = %s, want BAD_REQUEST for the invalid level", body)
	}
}

// The default levels filter: no input means ["info"].
func TestSubscribeDefaultLevels(t *testing.T) {
	a := newTestAPI(t)
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	a.seedLog(t, "i1", base, logging.LevelInfo, "info line")
	a.seedLog(t, "w1", base.Add(time.Millisecond), logging.LevelWarn, "warn line")

	root := a.seedUser(t, "root", true)
	_, _, body := a.sseGet(t, "/api/trpc/log.subscribe", root)
	if !strings.Contains(body, "info line") {
		t.Errorf("body = %s, want the info entry in the default history", body)
	}
	if strings.Contains(body, "warn line") {
		t.Errorf("body = %s, the warn entry must be filtered out of the default history", body)
	}
}

// The WS contract resolves the same shapes (the hub forwards Initial after
// started and Decode results as data frames — internal/ws covers the
// transport; here we pin the handler contract).
func TestWSHandlerContract(t *testing.T) {
	a := newTestAPI(t)
	a.seedLog(t, "e1", time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC), logging.LevelError, "ws error")

	handler := a.svc.SubscriptionHandlers()["log.subscribe"]
	root := a.seedUser(t, "root", true)
	sub, err := handler.Subscribe(context.Background(), &ws.Request{
		Path:    "log.subscribe",
		Input:   json.RawMessage(`{"levels":["error"]}`), // the ws transport delivers the input superjson-unwrapped
		Session: root,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.Pattern != "events.log.*" {
		t.Errorf("Pattern = %q, want events.log.*", sub.Pattern)
	}
	initial, ok := sub.Initial.(trpc.SuperJSON)
	if !ok {
		t.Fatalf("Initial type = %T, want trpc.SuperJSON", sub.Initial)
	}
	if !strings.Contains(string(initial), `"type":"history"`) ||
		!strings.Contains(string(initial), `"messages.0.timestamp":"Date"`) {
		t.Errorf("Initial = %s, want the annotated history event", initial)
	}
	// Decode drops other levels and renders the message event.
	payload, _ := logging.MarshalMessage(logging.Entry{
		ID: "x", Timestamp: time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
		Message: "ws live", Level: logging.LevelError,
	})
	data, keep := sub.Decode(bus.Message{Topic: "events.log.error", Payload: payload})
	if !keep {
		t.Fatal("Decode dropped a requested entry")
	}
	sj, ok := data.(trpc.SuperJSON)
	if !ok || !strings.Contains(string(sj), `"type":"message","message":{"id":"x"`) {
		t.Errorf("Decode = %v, want the annotated message event", data)
	}
	if _, keep = sub.Decode(bus.Message{Topic: "events.log.info", Payload: []byte(`{"json":{"id":"y","timestamp":"2026-11-07T11:00:00.000Z","message":"m","level":"info"},"meta":{"values":{"timestamp":"Date"},"v":1}}`)}); keep {
		t.Error("Decode kept a filtered level")
	}
}

// urlQuery encodes an input object as the superjson query parameter.
func urlQuery(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	envelope, err := json.Marshal(struct {
		JSON any `json:"json"`
	}{JSON: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return url.QueryEscape(string(envelope))
}
