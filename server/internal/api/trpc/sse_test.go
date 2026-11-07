package trpc_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
)

// SSE tests. The fixtures pin the byte-exact event stream of
// sseStreamProducer (tRPC 11.18.0 packages/server/src/unstable-core-do-not-
// import/stream/sse.ts): every event block is the `event: <name>\n` line
// (named events only), the `data: <data>\n` line and the `\n\n` terminator.

// sseTestHandler is a controllable SubscriptionHandler: fixed Initial, fixed
// bus pattern, passthrough Decode.
type sseTestHandler struct {
	initial any
	pattern string
	err     error
}

func (h *sseTestHandler) Subscribe(_ context.Context, _ *trpc.SubscriptionRequest) (*trpc.Subscription, error) {
	if h.err != nil {
		return nil, h.err
	}
	return &trpc.Subscription{Initial: h.initial, Pattern: h.pattern}, nil
}

// sseOpen starts a streaming request and returns the response plus a cancel
// function (closing the stream like an EventSource going away).
func sseOpen(t *testing.T, r *trpc.Router, method, target, body string, headers map[string]string) (*http.Response, func()) {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequestWithContext(ctx, method, srv.URL+target, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, srv.URL+target, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return res, func() {
		cancel()
		_ = res.Body.Close()
	}
}

// readSSE accumulates the stream until at least want bytes arrived or the
// request deadline ends the body (then it returns what it got without
// failing — use with care for "nothing more arrives" assertions).
func readSSE(t *testing.T, res *http.Response, want int) string {
	t.Helper()
	var sb strings.Builder
	tmp := make([]byte, 512)
	for sb.Len() < want {
		n, err := res.Body.Read(tmp)
		if n > 0 {
			sb.Write(tmp[:n])
		}
		if err != nil {
			break // deadline/cancel ended the stream
		}
	}
	return sb.String()
}

// readSSEAll drains the stream to EOF (the request deadline bounds it).
func readSSEAll(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, _ := io.ReadAll(res.Body)
	return string(raw)
}

func connectedEvent() string { return "event: connected\ndata: {}\n\n\n" }

func TestSSEStreamByteExact(t *testing.T) {
	eventBus := bus.New()
	defer eventBus.Close()
	r := trpc.New()
	r.Bus = eventBus
	r.RegisterSubscription("sub.test", &sseTestHandler{
		initial: map[string]any{"type": "history", "messages": []any{}},
		pattern: "events.test.*",
	})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test", "", nil)
	defer cancel()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	// sseHeaders (sse.ts:449-454).
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache, no-transform" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if ab := res.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("X-Accel-Buffering = %q", ab)
	}
	if ka := res.Header.Get("Connection"); ka != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", ka)
	}

	// connected + initial data event.
	got := readSSE(t, res, len(connectedEvent())+40)
	want := connectedEvent() + "data: {\"json\":{\"messages\":[],\"type\":\"history\"}}\n\n\n"
	if got != want {
		t.Fatalf("stream =\n%q\nwant\n%q", got, want)
	}

	// A live bus message streams as an unnamed data event (passthrough).
	eventBus.Publish("events.test.abc", []byte(`{"level":"info"}`))
	got += readSSE(t, res, 30)
	want += "data: {\"json\":{\"level\":\"info\"}}\n\n\n"
	if got != want {
		t.Fatalf("stream =\n%q\nwant\n%q", got, want)
	}
}

// Upstream picks the SSE response mode from the procedure type, not the
// accept header: a GET without `Accept: text/event-stream` to a registered
// subscription still streams.
func TestSSERegardlessOfAcceptHeader(t *testing.T) {
	r := trpc.New()
	r.RegisterSubscription("sub.test", &sseTestHandler{pattern: ""})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test", "", nil)
	defer cancel()
	got := readSSE(t, res, len(connectedEvent()))
	if got != connectedEvent() {
		t.Fatalf("stream = %q, want connected event", got)
	}
}

// Unregistered paths keep the classic NOT_FOUND envelope even with the SSE
// accept header (upstream: unknown procedure -> type unknown -> httpLink
// branch, resolveResponse.ts:404-439).
func TestSSEUnknownPathClassicNotFound(t *testing.T) {
	r := trpc.New()
	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/missing.sub", "",
		map[string]string{"Accept": "text/event-stream"})
	defer cancel()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := readSSEAll(t, res)
	want := `{"error":{"json":{"message":"No procedure found on path \"missing.sub\"","code":-32004,` +
		`"data":{"code":"NOT_FOUND","httpStatus":404,"path":"missing.sub"}}}}`
	if body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

// A procedure that throws before emitting opens the stream and then carries
// one serialized-error event with the tRPC error shape (errorToAsyncIterable
// + the producer catch, sse.ts:209-225).
func TestSSEHandlerErrorSerializedError(t *testing.T) {
	r := trpc.New()
	r.RegisterSubscription("sub.test", &sseTestHandler{
		err: trpc.NewError(trpc.CodeForbidden, "Permission denied"),
	})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test", "", nil)
	defer cancel()
	got := readSSEAll(t, res)
	want := connectedEvent() +
		"event: serialized-error\ndata: {\"json\":{\"message\":\"Permission denied\",\"code\":-32003," +
		"\"data\":{\"code\":\"FORBIDDEN\",\"httpStatus\":403,\"path\":\"sub.test\"}}}\n\n\n"
	if got != want {
		t.Fatalf("stream =\n%q\nwant\n%q", got, want)
	}
}

// Subscriptions accept GET only; the error rides inside the stream
// (resolveResponse.ts:350-353 throwing inside the subscription branch).
func TestSSEPostToSubscription(t *testing.T) {
	r := trpc.New()
	r.RegisterSubscription("sub.test", &sseTestHandler{})

	res, cancel := sseOpen(t, r, http.MethodPost, "/api/trpc/sub.test", `{"json":null}`,
		map[string]string{"Content-Type": "application/json"})
	defer cancel()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (errors ride the stream)", res.StatusCode)
	}
	got := readSSEAll(t, res)
	want := connectedEvent() +
		"event: serialized-error\ndata: {\"json\":{\"message\":\"Unsupported POST-request to subscription " +
		"procedure at path \\\"sub.test\\\"\",\"code\":-32005," +
		"\"data\":{\"code\":\"METHOD_NOT_SUPPORTED\",\"httpStatus\":405,\"path\":\"sub.test\"}}}\n\n\n"
	if got != want {
		t.Fatalf("stream =\n%q\nwant\n%q", got, want)
	}
}

// Batch calls never stream SSE: the subscription path fails per-op with the
// upstream messages (resolveResponse.ts:348-378 — method pairing first, then
// the batch guard).
func TestSSEBatchSubscriptionRejected(t *testing.T) {
	r := trpc.New()
	r.RegisterSubscription("sub.test", &sseTestHandler{})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test?batch=1&input=%7B%220%22%3Anull%7D", "", nil)
	defer cancel()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET batch status %d, want 400", res.StatusCode)
	}
	body := readSSEAll(t, res)
	want := `[{"error":{"json":{"message":"Cannot batch subscription calls","code":-32600,` +
		`"data":{"code":"BAD_REQUEST","httpStatus":400,"path":"sub.test"}}}}]`
	if body != want {
		t.Fatalf("body = %s, want %s", body, want)
	}

	res2, cancel2 := sseOpen(t, r, http.MethodPost, "/api/trpc/sub.test?batch=1", `{"0":null}`,
		map[string]string{"Content-Type": "application/json"})
	defer cancel2()
	if res2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST batch status %d, want 405", res2.StatusCode)
	}
	body2 := readSSEAll(t, res2)
	want2 := `[{"error":{"json":{"message":"Unsupported POST-request to subscription procedure at path \"sub.test\"","code":-32005,` +
		`"data":{"code":"METHOD_NOT_SUPPORTED","httpStatus":405,"path":"sub.test"}}}}]`
	if body2 != want2 {
		t.Fatalf("body = %s, want %s", body2, want2)
	}
}

// Idle pings: event ping with empty data (sse.ts:179-181 + the withPing
// wrapper), enabled through Router.SSEPingInterval (off by default like the
// upstream route handler that configures no sse options).
func TestSSEIdlePing(t *testing.T) {
	r := trpc.New()
	r.SSEPingInterval = 30 * time.Millisecond
	r.RegisterSubscription("sub.test", &sseTestHandler{pattern: ""})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test", "", nil)
	defer cancel()
	got := readSSE(t, res, len(connectedEvent())+len("event: ping\ndata: \n\n\n"))
	want := connectedEvent() + "event: ping\ndata: \n\n\n"
	if !strings.Contains(got, "event: ping\ndata: \n\n\n") {
		t.Fatalf("stream = %q, want ping event %q", got, want)
	}
	if !strings.HasPrefix(got, connectedEvent()) {
		t.Fatalf("stream = %q, want connected first", got)
	}
}

// Invalid input JSON: the lazy per-op parse failure becomes a
// serialized-error BAD_REQUEST on the stream.
func TestSSEInputParseError(t *testing.T) {
	r := trpc.New()
	r.RegisterSubscription("sub.test", &sseTestHandler{})

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test?input=%7Bbroken", "", nil)
	defer cancel()
	got := readSSEAll(t, res)
	if !strings.HasPrefix(got, connectedEvent()) {
		t.Fatalf("stream = %q, want connected first", got)
	}
	if !strings.Contains(got, `event: serialized-error`) || !strings.Contains(got, `"code":-32600`) {
		t.Fatalf("stream = %q, want serialized-error BAD_REQUEST", got)
	}
}

// The Decode filter drops messages (like the upstream level filter of
// log.subscribe) and a client disconnect cleans the bus subscription without
// affecting further publishes.
func TestSSEDecodeFilterAndDisconnect(t *testing.T) {
	eventBus := bus.New()
	defer eventBus.Close()
	r := trpc.New()
	r.Bus = eventBus
	r.RegisterSubscription("sub.test", trpc.SubscriptionHandlerFunc(
		func(_ context.Context, _ *trpc.SubscriptionRequest) (*trpc.Subscription, error) {
			return &trpc.Subscription{
				Pattern: "events.test.*",
				Decode: func(msg bus.Message) (any, bool) {
					var payload struct {
						Level string `json:"level"`
					}
					if err := json.Unmarshal(msg.Payload, &payload); err != nil {
						return nil, false
					}
					if payload.Level != "error" {
						return nil, false // dropped, like upstream's levels filter
					}
					return json.RawMessage(msg.Payload), true
				},
			}, nil
		}))

	res, cancel := sseOpen(t, r, http.MethodGet, "/api/trpc/sub.test", "", nil)

	got := readSSE(t, res, len(connectedEvent()))
	eventBus.Publish("events.test.a", []byte(`{"level":"info"}`))
	eventBus.Publish("events.test.b", []byte(`{"level":"error"}`))
	got += readSSE(t, res, len("data: {\"json\":{\"level\":\"error\"}}\n\n\n"))
	if got != connectedEvent()+"data: {\"json\":{\"level\":\"error\"}}\n\n\n" {
		t.Fatalf("stream = %q, want only the error-level event", got)
	}

	// Disconnect: publishing afterwards must not panic (subscription cleaned).
	cancel()
	time.Sleep(20 * time.Millisecond)
	eventBus.Publish("events.test.c", []byte(`{"level":"error"}`))
}

// A SuperJSON value (WrapJSONWithMeta) reaches the wire verbatim: the Date
// annotation travels in meta so the SPA observes real Date objects.
func TestSuperJSONValueOnWire(t *testing.T) {
	r := trpc.New()
	r.Register("meta.echo", func(_ context.Context, _ json.RawMessage) (any, *trpc.Error) {
		sj, err := trpc.WrapJSONWithMeta(map[string]any{"ts": "2026-11-07T10:00:00.000Z"},
			map[string]string{"ts": "Date"})
		if err != nil {
			return nil, trpc.Internal(err)
		}
		return sj, nil
	})

	res := do(t, r, http.MethodGet, "/api/trpc/meta.echo", nil, nil)
	want := `{"result":{"data":{"json":{"ts":"2026-11-07T10:00:00.000Z"},` +
		`"meta":{"values":{"ts":"Date"},"v":1}}}}`
	if res.body != want {
		t.Fatalf("body = %s, want %s", res.body, want)
	}

	// The streaming (httpBatchStreamLink) encoding rebases the annotation to
	// the line-level meta, where jsonlStreamConsumer applies it.
	res2 := do(t, r, http.MethodGet, "/api/trpc/meta.echo?batch=1&input=%7B%220%22%3Anull%7D",
		nil, map[string]string{"trpc-accept": "application/jsonl"})
	want2 := `{"json":{"0":[[{"result":{"data":{"ts":"2026-11-07T10:00:00.000Z"}}}]]},` +
		`"meta":{"values":{"0.0.0.result.data.ts":"Date"},"v":1}}` + "\n"
	if res2.body != want2 {
		t.Fatalf("body = %q, want %q", res2.body, want2)
	}
}
