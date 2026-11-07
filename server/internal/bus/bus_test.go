package bus

import (
	"testing"
	"time"
)

func TestExactAndWildcardMatching(t *testing.T) {
	b := New()
	defer b.Close()

	exact := b.Subscribe("events.ping.abc")
	wild := b.Subscribe("events.ping.*")
	other := b.Subscribe("events.docker.abc")

	b.Publish("events.ping.abc", []byte(`{"ok":true}`))
	b.Publish("events.ping.def", []byte(`{"ok":true}`))
	b.Publish("events.pingabc", []byte(`{"ok":false}`)) // must NOT match "events.ping.*"

	assertReceive(t, exact.C, "events.ping.abc")
	assertReceive(t, wild.C, "events.ping.abc")
	assertReceive(t, wild.C, "events.ping.def")
	assertNoMessage(t, other.C)
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := New()
	defer b.Close()

	sub := b.Subscribe("events.ping.*")
	sub.Unsubscribe()
	sub.Unsubscribe() // idempotent

	b.Publish("events.ping.abc", []byte(`{}`))
	// The channel is closed after Unsubscribe: never delivers the message.
	select {
	case got, ok := <-sub.C:
		if ok {
			t.Fatalf("received unexpected message on topic %q after Unsubscribe", got.Topic)
		}
	default:
	}
}

func TestSlowSubscriberDropsWithoutBlocking(t *testing.T) {
	b := New()
	defer b.Close()

	sub := b.Subscribe("events.ping.*")
	for i := 0; i < 200; i++ { // channel cap is 64: excess must drop, not block
		b.Publish("events.ping.abc", []byte(`{}`))
	}
	// Publishing returned (no deadlock); the buffer holds at most 64 events.
	received := 0
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				t.Fatalf("unexpected closed channel")
			}
			received++
			if received > 64 {
				t.Fatalf("buffer exceeded 64: %d", received)
			}
		default:
			if received == 0 {
				t.Fatalf("expected buffered messages, got none")
			}
			return
		}
	}
}

func TestPublishAfterCloseIsNoOp(t *testing.T) {
	b := New()
	sub := b.Subscribe("events.ping.*")
	b.Close()
	b.Publish("events.ping.abc", []byte(`{}`)) // must not panic nor block
	if got, ok := <-sub.C; ok {
		t.Fatalf("received unexpected message on topic %q after Close", got.Topic)
	}
}

func assertReceive(t *testing.T, ch <-chan Message, wantTopic string) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed while waiting for topic %q", wantTopic)
		}
		if got.Topic != wantTopic {
			t.Fatalf("got topic %q, want %q", got.Topic, wantTopic)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for topic %q", wantTopic)
	}
}

func assertNoMessage(t *testing.T, ch <-chan Message) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("received unexpected message on topic %q", got.Topic)
		}
	default:
	}
}
