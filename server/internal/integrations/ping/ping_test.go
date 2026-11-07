package ping_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/ping"
)

// TestEventSuccessShape pins the success payload against the upstream Redis
// channel contract (packages/redis/src/index.ts pingChannel):
// {url, statusCode, durationMs}.
func TestEventSuccessShape(t *testing.T) {
	raw, err := ping.MarshalEvent(ping.Event{
		URL:         "http://x.local",
		CheckResult: integrations.CheckResult{StatusCode: 200, DurationMs: 12.5},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != 3 || m["url"] != "http://x.local" || m["statusCode"] != float64(200) || m["durationMs"] != 12.5 {
		t.Fatalf("payload = %s, want exactly url+statusCode+durationMs", raw)
	}
	if _, ok := m["error"]; ok {
		t.Fatalf("success payload must not carry an error member: %s", raw)
	}
}

// TestEventFailureShape pins the failure payload: {url, error}.
func TestEventFailureShape(t *testing.T) {
	raw, err := ping.MarshalEvent(ping.Event{
		URL:         "http://x.local",
		CheckResult: integrations.CheckResult{Error: "connect refused"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != 2 || m["url"] != "http://x.local" || m["error"] != "connect refused" {
		t.Fatalf("payload = %s, want exactly url+error", raw)
	}
	if _, ok := m["statusCode"]; ok {
		t.Fatalf("failure payload must not carry statusCode: %s", raw)
	}
}

// TestPublishTopic pins the bus topic convention events.ping.<appId> and the
// publishing behavior (nil bus = no-op).
func TestPublishTopic(t *testing.T) {
	if got := ping.Topic("app-1"); got != "events.ping.app-1" {
		t.Fatalf("Topic = %q, want events.ping.app-1", got)
	}

	b := bus.New()
	defer b.Close()
	sub := b.Subscribe("events.ping.*")
	defer sub.Unsubscribe()

	if err := ping.Publish(b, "app-1", "http://x.local", integrations.CheckResult{StatusCode: 200, DurationMs: 5}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case msg := <-sub.C:
		if msg.Topic != "events.ping.app-1" {
			t.Fatalf("topic = %q", msg.Topic)
		}
		var m map[string]any
		if err := json.Unmarshal(msg.Payload, &m); err != nil {
			t.Fatalf("payload not JSON: %v", err)
		}
		if m["statusCode"] != float64(200) {
			t.Fatalf("payload = %s", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}

	// Nil bus: publishing is a no-op, not an error.
	if err := ping.Publish(nil, "app-1", "http://x.local", integrations.CheckResult{}); err != nil {
		t.Fatalf("Publish(nil bus) = %v, want nil", err)
	}
}
