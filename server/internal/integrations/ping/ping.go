// Package ping registers the "ping" pseudo-integration and defines the bus
// event contract for its results.
//
// Upstream does NOT model ping as an integration kind: the logic lives in the
// packages/ping package (sendPingRequestAsync), the "ping" cron job
// (packages/cron-jobs/src/jobs/ping.ts) and the widget.app.ping tRPC
// procedure (packages/api/src/router/widgets/app.ts). The port wraps that
// behavior as the KindPing integration so the framework has a first citizen
// and the cron job stays a thin loader/publisher; the v1 subpackage holds the
// implementation, mirroring the upstream pattern where the factory lazy-loads
// the versioned integration class (packages/integrations/src/base/creator.ts).
//
// Ping targets are apps, not integrations: widget.app.ping resolves
// app.pingUrl ?? app.href, and the onboard flow creates apps with pingUrl for
// its featured services (packages/api/src/router/onboard/onboard-router.ts).
//
// Bus contract (internal/bus/bus.go topic convention events.<domain>.<id>):
//
//	events.ping.<appId>   one event per checked app, payload = Event JSON
//
// Event reproduces the exact upstream payload union of the Redis channel
// "ping" (packages/redis/src/index.ts pingChannel):
//
//	{"url":"...","statusCode":200,"durationMs":12.5}   // success
//	{"url":"...","error":"..."}                        // failure
package ping

import (
	"bytes"
	"encoding/json"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/integrations"
	v1 "github.com/ydtoloza/homarr/server/internal/integrations/ping/v1"
)

// Kind is the registry key ("ping"). See integrations.KindPing.
const Kind = integrations.KindPing

// TopicPrefix is the bus topic domain for ping results; the event id is the
// "app"."id" of the checked target.
const TopicPrefix = "events.ping."

// Topic returns the bus topic for one ping target
// ("events.ping.<appId>").
func Topic(appID string) string { return TopicPrefix + appID }

// init self-registers the kind into the default registry (the database/sql
// driver pattern): wiring an integration is one blank import at the
// composition root.
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return v1.New(input), nil
	})
}

// Definition returns the metadata of the ping kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Ping"}
}

// New builds a ping integration for one target URL (exported for callers
// that bypass the registry, e.g. focused tests).
func New(input integrations.Input) *v1.Integration { return v1.New(input) }

// Event is the wire payload published on Topic(appID). The embedded
// CheckResult flattens to the upstream union shape
// ({url, statusCode, durationMs} on success, {url, error} on failure —
// omitempty drops the members that do not apply, exactly like the TS union
// of packages/redis/src/index.ts pingChannel).
type Event struct {
	URL string `json:"url"`
	integrations.CheckResult
}

// MarshalEvent serializes an Event without HTML escaping, matching the JSON
// conventions of the rest of the port (internal/api/trpc/superjson.go).
func MarshalEvent(e Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Publish publishes one ping result for the target appID on the bus. A nil
// bus is a no-op (the scheduler can run without a wired hub in tests).
func Publish(b *bus.Bus, appID, url string, res integrations.CheckResult) error {
	if b == nil {
		return nil
	}
	payload, err := MarshalEvent(Event{URL: url, CheckResult: res})
	if err != nil {
		return err
	}
	b.Publish(Topic(appID), payload)
	return nil
}
