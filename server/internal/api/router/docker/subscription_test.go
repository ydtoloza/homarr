package docker_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/docker"
	"github.com/ydtoloza/homarr/server/internal/bus"
	dockerclient "github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Subscription handler tests: the docker.subscribeLogs gates, the Initial
// tail frame, and the pump → bus forwarding on the exact topic
// (events.docker.logs.<endpointId>.<containerId>).

func subscriptionAPI(t *testing.T) (*testAPI, *ctxauth.Session, string) {
	t.Helper()
	var eventBus *bus.Bus
	api := newTestAPI(t, func(fe *fakeEngine, opts *docker.Options) {
		eventBus = bus.New()
		opts.Bus = eventBus
		// The Initial tail snapshot (follow=false).
		fe.staticLogs = concatFrames(
			encodeFrame(1, "history one\n"),
			encodeFrame(1, "history two\n"),
		)
	})
	// testAPI owns the bus through options; re-read it.
	_ = eventBus
	alice := api.seedUser(t, testUser{id: "u-alice", name: "alice", admin: true})
	return api, alice, api.engine.endpoint().ID
}

func busOf(t *testing.T, api *testAPI) *bus.Bus {
	t.Helper()
	return api.options.Bus
}

func TestSubscribeLogsGates(t *testing.T) {
	api, alice, epID := subscriptionAPI(t)

	// Anonymous → UNAUTHORIZED (protectedProcedure).
	_, err := api.subscribe(t, nil, `{"endpointId":"`+epID+`","id":"abc"}`)
	if code, _ := trpcErrorCode(t, err); code != "UNAUTHORIZED" {
		t.Fatalf("anonymous: %v", err)
	}

	// Signed in without admin → FORBIDDEN "Permission denied".
	bob := api.seedUser(t, testUser{id: "u-bob", name: "bob"})
	_, err = api.subscribe(t, bob, `{"endpointId":"`+epID+`","id":"abc"}`)
	if code, message := trpcErrorCode(t, err); code != "FORBIDDEN" || message != "Permission denied" {
		t.Fatalf("non-admin: %s %q", code, message)
	}

	// Invalid input → BAD_REQUEST.
	_, err = api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc","tail":0}`)
	if code, _ := trpcErrorCode(t, err); code != "BAD_REQUEST" {
		t.Fatalf("tail=0: %v", err)
	}
	_, err = api.subscribe(t, alice, `{"endpointId":"","id":"abc"}`)
	if code, _ := trpcErrorCode(t, err); code != "BAD_REQUEST" {
		t.Fatalf("empty endpointId: %v", err)
	}

	// Missing logs capability → FORBIDDEN with the upstream message.
	limited := dockerclient.NewRegistry([]dockerclient.Endpoint{limitedEndpoint(api.engine, dockerclient.CapabilityInventory)}, nil)
	api2 := api.withRegistry(t, limited)
	_, err = api2.subscribe(t, alice, `{"endpointId":"`+api.engine.endpoint().ID+`","id":"abc"}`)
	if code, message := trpcErrorCode(t, err); code != "FORBIDDEN" ||
		message != "Docker endpoint does not permit logs access" {
		t.Fatalf("capability: %s %q", code, message)
	}

	// Unknown container → NOT_FOUND "Container not found".
	api.engine.failInspect = true
	_, err = api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"zzz"}`)
	api.engine.failInspect = false
	if code, message := trpcErrorCode(t, err); code != "NOT_FOUND" || message != "Container not found" {
		t.Fatalf("missing container: %s %q", code, message)
	}
}

func TestSubscribeLogsInitialAndBusForward(t *testing.T) {
	api, alice, epID := subscriptionAPI(t)
	eventBus := busOf(t, api)

	sub, err := api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc","tail":2}`)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Initial = the recent tail decoded as ONE string (the tail snapshot).
	initial, ok := sub.Initial.(string)
	if !ok || initial != "history one\nhistory two\n" {
		t.Fatalf("Initial = %#v", sub.Initial)
	}

	// Pattern = the exact topic.
	topic := docker.LogTopic(epID, "abc")
	if sub.Pattern != topic {
		t.Fatalf("Pattern = %q, want %q", sub.Pattern, topic)
	}

	// The pump opened a follow stream (tail=0, live only).
	stream := api.engine.takeFollowStream(t)
	last := api.engine.lastRequest()
	if last.Query != "follow=true&stderr=true&stdout=true" {
		t.Fatalf("pump query = %q", last.Query)
	}

	// Frames pushed by the engine surface as bus messages with a JSON-string
	// payload; Decode passes them through unchanged.
	busSub := eventBus.Subscribe(sub.Pattern)
	defer busSub.Unsubscribe()
	otherSub := eventBus.Subscribe("events.docker.logs.other-endpoint.other")
	defer otherSub.Unsubscribe()

	stream.push(encodeFrame(1, "live line one\n"))
	stream.push(encodeFrame(2, "live err\n"))

	received := []string{}
	deadline := time.After(5 * time.Second)
	for len(received) < 2 {
		select {
		case msg := <-busSub.C:
			// Payload shape: a JSON string literal — the hub re-wraps
			// Decode's result with trpc.WrapJSON, so the passthrough
			// json.RawMessage becomes the "json" member verbatim (the
			// frontend writes each data frame straight to xterm).
			var decoded string
			if err := json.Unmarshal(msg.Payload, &decoded); err != nil {
				t.Fatalf("payload is not a JSON string: %s", msg.Payload)
			}
			data, ok := sub.Decode(msg)
			if !ok {
				t.Fatalf("Decode dropped the message")
			}
			passthrough, ok := data.(json.RawMessage)
			if !ok || string(passthrough) != string(msg.Payload) {
				t.Fatalf("Decode must pass the payload through, got %s", passthrough)
			}
			received = append(received, decoded)
		case <-otherSub.C:
			t.Fatalf("message leaked to a foreign topic")
		case <-deadline:
			t.Fatalf("timed out waiting for frames, got %q", received)
		}
	}
	if received[0] != "live line one\n" || received[1] != "live err\n" {
		t.Fatalf("received = %q", received)
	}

	// Payload shape: exactly the JSON string the frontend writes to xterm;
	// < > & stay unescaped (SetEscapeHTML(false), house JSON convention).
	stream.push(encodeFrame(1, "raw <b>html</b> & \"quotes\"\n"))
	select {
	case msg := <-busSub.C:
		if string(msg.Payload) != "\"raw <b>html</b> & \\\"quotes\\\"\\n\"" {
			t.Fatalf("payload escaping: %s", msg.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out")
	}

	stream.close()
}

// string0 helper removed — json.RawMessage converts with string(...) inline.

func TestPumpSharedAcrossSubscribers(t *testing.T) {
	api, alice, epID := subscriptionAPI(t)

	// First subscription starts the pump.
	if _, err := api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc"}`); err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}
	first := api.engine.takeFollowStream(t)

	// A second subscriber reuses the SAME engine stream (one pump per
	// endpoint/container pair).
	secondSub, err := api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc"}`)
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}
	_ = secondSub
	followCount := 0
	for _, req := range api.engine.requests {
		if req.Path == "/containers/abc/logs" && req.Query == "follow=true&stderr=true&stdout=true" {
			followCount++
		}
	}
	if followCount != 1 {
		t.Fatalf("follow streams = %d, want 1 (shared pump)", followCount)
	}
	_ = first
}

func TestPumpIdleStop(t *testing.T) {
	// A pump whose stream stays silent for LogPumpIdleTimeout stops; the
	// next subscriber starts a fresh stream (fork lifecycle: the ws
	// SubscriptionHandler has no unsubscribe callback).
	var idle time.Duration = 120 * time.Millisecond
	api := newTestAPI(t, func(fe *fakeEngine, opts *docker.Options) {
		opts.Bus = bus.New()
		opts.LogPumpIdleTimeout = idle
		fe.staticLogs = nil
	})
	alice := api.seedUser(t, testUser{id: "u-alice", name: "alice", admin: true})
	epID := api.engine.endpoint().ID

	if _, err := api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc"}`); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	stream := api.engine.takeFollowStream(t)
	// Keep the stream silent; wait past the idle timeout.
	time.Sleep(idle + 300*time.Millisecond)

	before := api.engine.requestCount()
	if _, err := api.subscribe(t, alice, `{"endpointId":"`+epID+`","id":"abc"}`); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	_ = stream
	if api.engine.requestCount() == before {
		t.Fatalf("a fresh follow stream should have been opened after the idle stop")
	}
}

func TestSubscribeTailSnapshotError(t *testing.T) {
	// A dead engine at subscribe time rejects the subscription like
	// upstream's initialization catch: the container lookup fails first
	// (findDockerContainerAsync resolves null) → NOT_FOUND.
	fe := newFakeEngine(t)
	ep := fe.endpoint()
	reg := dockerclient.NewRegistry([]dockerclient.Endpoint{ep}, nil)
	api := buildAPICustom(t, fe, reg)
	alice := api.seedUser(t, testUser{id: "u-alice", name: "alice", admin: true})

	fe.srv.Close() // engine dies: inspect and tail both fail → NOT_FOUND

	_, err := api.subscribe(t, alice, `{"endpointId":"`+ep.ID+`","id":"abc"}`)
	if code, _ := trpcErrorCode(t, err); code != "NOT_FOUND" {
		t.Fatalf("dead engine: %v", err)
	}
}
