package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Log event pump: the bridge between the Engine log stream and the bus.
//
// Upstream streams container logs DIRECTLY inside the tRPC subscription
// observable (docker.subscribeLogs → streamContainerLogsAsync); there is no
// Redis channel for docker logs upstream. In homarr-go the WebSocket hub
// delivers data frames exclusively from the bus (internal/ws/
// subscription.go), so an active subscription needs a producer publishing to
// "events.docker.logs.<endpointId>.<containerId>". The pump registry owns
// one Engine stream per (endpoint, container) pair and refcounts nothing:
// every subscriber attaches its own bus subscription to the same topic.
//
// Backpressure: bus.Publish drops messages when a subscriber falls behind
// (64-message buffer, internal/bus/bus.go). For logs this is acceptable —
// the payload is an ephemeral terminal view (xterm), not a queue; the
// Initial frame re-fetches the last `tail` lines so a slow client only loses
// intermediate lines, never history. Dropping keeps memory bounded when a
// container floods stdout (documented in DOCKER.md section 5).

// logTopicPrefix is the topic domain; the event id is
// "<endpointId>.<containerId>" (bus.go convention events.<domain>.<id>.<sub>).
const logTopicPrefix = "events.docker.logs."

// LogTopic returns the bus topic of one container log stream.
func LogTopic(endpointID, containerID string) string {
	return logTopicPrefix + endpointID + "." + containerID
}

// Event is the wire payload of one log frame: a JSON string with the decoded
// text chunk. The frontend writes each subscription data event straight to
// xterm (terminal.tsx onData → terminal.write), so the payload shape is the
// upstream observable's: a string.
type Event string

// MarshalEvent serializes an Event without HTML escaping (house JSON
// convention, internal/api/trpc/superjson.go).
func MarshalEvent(e Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// pumpKey identifies one streamed container.
type pumpKey struct {
	endpointID  string
	containerID string
}

// pumpRegistry owns the running pumps.
type pumpRegistry struct {
	factory DockerClientFactory
	bus     *bus.Bus
	// idleTimeout stops a pump whose stream produced no frame for this long.
	// The ws.SubscriptionHandler contract has no unsubscribe callback
	// (internal/ws/subscription.go), so exact refcounting is impossible;
	// the idle TTL bounds leaks instead (DOCKER.md section 5).
	idleTimeout time.Duration

	mu    sync.Mutex
	pumps map[pumpKey]*logPump
}

func newPumpRegistry(factory DockerClientFactory, eventBus *bus.Bus, idleTimeout time.Duration, _ func() time.Time) *pumpRegistry {
	return &pumpRegistry{
		factory:     factory,
		bus:         eventBus,
		idleTimeout: idleTimeout,
		pumps:       make(map[pumpKey]*logPump),
	}
}

// acquire returns the running pump for the key or starts a new one. The
// returned error mirrors the upstream subscription initialization catch
// (docker-router.ts:237-246): a failing stream start surfaces as NOT_FOUND
// with the engine message.
func (p *pumpRegistry) acquire(endpointID, containerID string, tail int) (*logPump, error) {
	key := pumpKey{endpointID, containerID}
	p.mu.Lock()
	if pump, ok := p.pumps[key]; ok && !p.stopped(pump) {
		p.mu.Unlock()
		return pump, nil
	}
	// Drop a dead pump entry before starting fresh.
	delete(p.pumps, key)
	p.mu.Unlock()

	pump, err := p.start(key, tail)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	// A racing acquire may have started another pump; the loser stops
	// itself and adopts the winner.
	if existing, ok := p.pumps[key]; ok {
		p.mu.Unlock()
		pump.stop()
		return existing, nil
	}
	p.pumps[key] = pump
	p.mu.Unlock()
	return pump, nil
}

// stopped reports whether a pump already exited (dead pumps are replaced on
// the next acquire).
func (p *pumpRegistry) stopped(pump *logPump) bool {
	select {
	case <-pump.done:
		return true
	default:
		return false
	}
}

func (p *pumpRegistry) remove(key pumpKey, pump *logPump) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current, ok := p.pumps[key]; ok && current == pump {
		delete(p.pumps, key)
	}
}

// start opens the follow stream and spawns the publish loop.
func (p *pumpRegistry) start(key pumpKey, tail int) (*logPump, error) {
	client, err := p.factory.ClientFor(key.endpointID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	// tail=0: the subscriber's Initial frame carries the recent history
	// (a separate non-follow tail fetch in subscribeLogs), so the follow
	// stream starts live to avoid duplicating the tail.
	raw, err := client.Logs(ctx, key.containerID, docker.LogsOptions{Tail: 0, Follow: true})
	if err != nil {
		cancel()
		return nil, err
	}
	pump := &logPump{
		key:    key,
		bus:    p.bus,
		topic:  LogTopic(key.endpointID, key.containerID),
		reader: docker.NewFrameReader(raw),
		stream: raw,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go pump.run(p.idleTimeout)
	return pump, nil
}

// logPump streams one container's logs and publishes each demultiplexed
// frame to the bus.
type logPump struct {
	key    pumpKey
	bus    *bus.Bus
	topic  string
	reader *docker.FrameReader
	stream interface{ Close() error }
	cancel context.CancelFunc
	done   chan struct{}
}

// run is the publish loop. Idle timeout: a stream that produces no frame for
// the timeout duration is stopped (fork lifecycle, see pumpRegistry). Frame
// size violations stop the stream like upstream's processor does
// (request-handler/src/docker.ts:293-299: onError + destroy).
func (p *logPump) run(idleTimeout time.Duration) {
	defer close(p.done)
	defer p.cancel()
	defer func() { _ = p.stream.Close() }()
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			slog.Info("docker: log pump idle timeout", "endpoint", p.key.endpointID, "container", p.key.containerID)
			return
		default:
		}
		chunk, ok, err := p.reader.Next()
		if err != nil {
			// ErrFrameTooLarge mirrors upstream's onError → destroy; other
			// read errors are transport failures. Either way the pump stops;
			// upstream would emit an INTERNAL_SERVER_ERROR frame into the
			// stream (docker-router.ts:226-235), the bus has no error
			// channel — the deviation is documented in DOCKER.md section 5.
			slog.Warn("docker: log pump stopped", "endpoint", p.key.endpointID, "container", p.key.containerID, "error", err)
			return
		}
		if !ok {
			// EOF: container removed or daemon closed the stream.
			return
		}
		// Reset the idle timer on every frame (activity = alive).
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idleTimeout)

		p.publish(chunk)
	}
}

// publish encodes one chunk as a JSON string and pushes it to the bus.
// Publish never blocks (slow subscribers drop), so a flooding container
// cannot stall the pump.
func (p *logPump) publish(chunk string) {
	if p.bus == nil {
		return
	}
	payload, err := MarshalEvent(Event(chunk))
	if err != nil {
		// Marshaling a Go string cannot fail; defensive only.
		return
	}
	p.bus.Publish(p.topic, payload)
}

// stop cancels the stream and closes the HTTP body.
func (p *logPump) stop() {
	p.cancel()
	_ = p.stream.Close()
	<-p.done
}
