// Package integrations is the Go port of the upstream integrations framework
// (packages/integrations): a registry of integration kinds with a factory per
// kind, mirroring upstream's integrationDefs metadata (packages/definitions/
// src/integration.ts) plus the integrationCreators factory map
// (packages/integrations/src/base/creator.ts) and the Integration base class
// (packages/integrations/src/base/integration.ts).
//
// Upstream resolves an integration instance with
// createIntegrationAsync(kind, input): the kind indexes a lazy factory that
// constructs the versioned implementation class. The Go mirror keeps the same
// shape so the ~50 remaining integrations land mechanically in M4:
//
//	integrations/<name>/        Definition + Factory + registration
//	integrations/<name>/v1/     the implementation (importable, testable)
//
// each <name> package self-registers in init (the database/sql driver
// pattern), and the composition root blank-imports the packages it needs.
//
// The smallest useful core lives here now: Check (reachability/health of the
// configured target). Upstream's richer domain interfaces (media-organizer,
// calendar, media-server, ...) become optional capability interfaces on the
// same instances as M4 ports them.
package integrations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Kind identifies an integration kind, upstream's IntegrationKind
// (packages/definitions/src/integration.ts). The reserved pseudo-kind "ping"
// (internal/integrations/ping) covers the app ping of upstream's packages/ping
// package, which is not an integration row in the database but a reachability
// check keyed by URL.
const KindPing = "ping"

// Definition describes one integration kind. It mirrors the fields of an
// upstream integrationDefs entry (name plus, over time, the rest of the
// metadata: iconUrl, secretKinds, category, httpAuth, defaultPort — added as
// M4 ports the procedures that consume them).
type Definition struct {
	// Kind is the stable identifier stored in the "integration"."kind" column
	// (e.g. "plex", "ping"). Never empty and must not contain the bus topic
	// separators (dots/spaces).
	Kind string
	// Name is the human-readable label ("Plex").
	Name string
}

// Input is the constructor input for one integration instance. It mirrors
// upstream's IntegrationInput (packages/integrations/src/base/integration.ts:
// IntegrationInput) with the fields the port actually consumes today; the
// decrypted secrets join in M4 together with the integrationSecret encryption
// layer.
type Input struct {
	// ID is the "integration"."id" (or the "app"."id" for the ping
	// pseudo-kind): it keys bus events and cache entries.
	ID string
	// Kind repeats the integration kind for implementations shared across
	// kinds (upstream passes it optionally for getIntegrationHttpAuthDefinition).
	Kind string
	// Name is the instance label from the database row.
	Name string
	// URL is the base URL of the target (integration.url / app.pingUrl).
	URL string
	// ExternalURL is the WAN-facing URL, empty when unset
	// (integration.external_url).
	ExternalURL string
}

// CheckResult is the outcome of a Check call. Its JSON form is the union
// shape the frontend already consumes for reachability results: a successful
// check marshals {"statusCode":200,"durationMs":12.5} and a failed one
// {"error":"..."} — the same members as the upstream ping payload
// (packages/redis/src/index.ts pingChannel; packages/api/src/router/widgets/
// app.ts:39-44), so one wire shape serves ping now and test-connection later.
type CheckResult struct {
	// StatusCode is the HTTP status of the check response; 0 when the check
	// failed at transport level (member omitted on the wire).
	StatusCode int `json:"statusCode,omitempty"`
	// DurationMs is the round-trip time in milliseconds up to the response
	// headers (0/omitted on failure).
	DurationMs float64 `json:"durationMs,omitempty"`
	// Error is a human-readable failure reason, empty on success.
	Error string `json:"error,omitempty"`
}

// Integration is the smallest useful core of the upstream Integration base
// class: verify the configured target is reachable. Instances must be safe
// for concurrent Check calls (the ping job runs checks in parallel).
type Integration interface {
	// Check performs one reachability/health check of the configured target.
	// It returns (result, nil) when a definitive answer was produced —
	// including "unreachable", which upstream reports as {error: message}
	// instead of a thrown error (packages/ping/src/index.ts) — and (zero,
	// err) only when the check itself could not run.
	Check(ctx context.Context) (CheckResult, error)
}

// Factory builds an integration instance of one kind from its input. Errors
// are construction-time only (bad input); runtime failures surface through
// Check.
type Factory func(input Input) (Integration, error)

// ErrUnknownKind is returned by New when no factory is registered for the
// requested kind.
var ErrUnknownKind = errors.New("integrations: unknown integration kind")

// entry is one registered kind.
type entry struct {
	def     Definition
	factory Factory
}

// Registry maps kinds to factories. A package-level default registry backs
// Register/New; tests build isolated ones with NewRegistry.
type Registry struct {
	mu     sync.RWMutex
	byKind map[string]entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byKind: make(map[string]entry)}
}

func (r *Registry) register(def Definition, factory Factory) {
	if err := validKind(def.Kind); err != nil {
		panic(fmt.Sprintf("integrations: Register(%q): %v", def.Kind, err))
	}
	if def.Name == "" {
		panic(fmt.Sprintf("integrations: Register(%q): empty Name", def.Kind))
	}
	if factory == nil {
		panic(fmt.Sprintf("integrations: Register(%q): nil factory", def.Kind))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byKind[def.Kind]; ok {
		// Upstream would fail this at compile time: integrationCreators is an
		// object literal, a duplicate key is a TS error. Panic keeps the same
		// fail-fast behavior.
		panic(fmt.Sprintf("integrations: kind %q registered twice", def.Kind))
	}
	r.byKind[def.Kind] = entry{def: def, factory: factory}
}

// Register adds one kind to the registry. It panics on a duplicate kind, an
// invalid kind string, or a nil factory — mirroring the compile-time
// guarantee of upstream's integrationCreators object literal.
func (r *Registry) Register(def Definition, factory Factory) { r.register(def, factory) }

// DefinitionOf returns the metadata registered for kind.
func (r *Registry) DefinitionOf(kind string) (Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byKind[kind]
	if !ok {
		return Definition{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return e.def, nil
}

// New builds an instance of kind from input using the registered factory
// (upstream createIntegrationAsync, packages/integrations/src/base/creator.ts).
func (r *Registry) New(kind string, input Input) (Integration, error) {
	r.mu.RLock()
	e, ok := r.byKind[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if input.Kind == "" {
		input.Kind = kind
	}
	return e.factory(input)
}

// Kinds lists the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultRegistry is the process-wide registry used by Register/New. Each
// integrations/<name> package self-registers here from init.
var defaultRegistry = NewRegistry()

// Register adds one kind to the default registry. It panics on a duplicate
// kind, an invalid kind string, or a nil factory — mirroring the compile-time
// guarantee of upstream's integrationCreators object literal.
func Register(def Definition, factory Factory) { defaultRegistry.register(def, factory) }

// DefinitionOf returns the metadata registered for kind.
func DefinitionOf(kind string) (Definition, error) { return defaultRegistry.DefinitionOf(kind) }

// New builds an instance of kind from input using the registered factory
// (upstream createIntegrationAsync, packages/integrations/src/base/creator.ts).
func New(kind string, input Input) (Integration, error) { return defaultRegistry.New(kind, input) }

// Kinds lists the kinds registered in the default registry, sorted.
func Kinds() []string { return defaultRegistry.Kinds() }

// validKind enforces the invariants the kind must keep: it is stored in the
// database and used as the middle segment of bus topics (events.<domain>.<id>
// convention, internal/bus/bus.go), where dots and whitespace would corrupt
// the wildcard matching.
func validKind(kind string) error {
	if kind == "" {
		return errors.New("empty Kind")
	}
	for _, r := range kind {
		if r == '.' || r == ' ' || r == '\t' || r == '\n' {
			return errors.New("kind must not contain dots or whitespace (bus topic separator)")
		}
	}
	return nil
}
