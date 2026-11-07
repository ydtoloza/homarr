// Package bus is the in-process replacement for upstream's Redis pub/sub.
//
// Upstream needed Redis because the dashboard ran as several processes
// (next.js, websocket, tasks); homarr-go is one process, so domain events
// (ping results, docker stats, job progress) travel through this hub and the
// WebSocket package fans them out to browsers.
//
// Topic convention (the contract between producers and consumers):
//
//	events.<domain>.<id>        e.g. "events.ping.<integrationId>"
//	events.<domain>.<id>.<sub>  finer-grained sub-events when needed
//
// Producers publish JSON payloads; consumers subscribe with exact topics or
// a trailing ".*" wildcard ("events.ping.*" matches every ping event).
package bus

import (
	"strings"
	"sync"
)

// Message is one published event. Payload is raw JSON.
type Message struct {
	Topic   string
	Payload []byte
}

// Subscription receives matching messages on C until Unsubscribe is called
// or the Bus is closed. C is buffered; when a subscriber falls behind,
// messages are dropped (dashboard events are ephemeral state, not a queue).
type Subscription struct {
	C     <-chan Message
	unsub func()
	once  sync.Once
}

// Unsubscribe stops delivery and releases the subscription. Safe to call
// multiple times.
func (s *Subscription) Unsubscribe() {
	s.once.Do(func() {
		if s.unsub != nil {
			s.unsub()
		}
	})
}

// Bus fans published messages out to matching subscribers.
type Bus struct {
	mu     sync.RWMutex
	subs   map[*subState]struct{}
	closed bool
}

type subState struct {
	patterns []string
	ch       chan Message
}

// New creates an empty Bus.
func New() *Bus {
	return &Bus{subs: make(map[*subState]struct{})}
}

// Publish delivers payload to every subscriber matching topic. Publishing
// after Close is a no-op. Never blocks: slow subscribers drop messages.
func (b *Bus) Publish(topic string, payload []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for s := range b.subs {
		if !matches(s.patterns, topic) {
			continue
		}
		select {
		case s.ch <- Message{Topic: topic, Payload: payload}:
		default: // subscriber is slow; drop (see Subscription doc)
		}
	}
}

// Subscribe registers interest in one or more topic patterns. A pattern
// matches exactly, or — ending in ".*" — as a prefix over the remaining
// segments: "events.ping.*" matches "events.ping.abc" (and deeper paths),
// but neither "events.ping" nor "events.pingabc".
func (b *Bus) Subscribe(patterns ...string) *Subscription {
	if len(patterns) == 0 {
		panic("bus: Subscribe requires at least one pattern")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		panic("bus: Subscribe on closed Bus")
	}
	s := &subState{patterns: append([]string(nil), patterns...), ch: make(chan Message, 64)}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	sub := &Subscription{C: s.ch}
	sub.unsub = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s]; !ok {
			return
		}
		delete(b.subs, s)
		close(s.ch)
	}
	return sub
}

// Close stops the bus and closes every active subscription.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		close(s.ch)
	}
}

func matches(patterns []string, topic string) bool {
	for _, p := range patterns {
		if p == topic {
			return true
		}
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(topic, p[:len(p)-1]) {
			return true
		}
	}
	return false
}
