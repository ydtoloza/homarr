package docker

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Registry is the Go port of DockerSingleton
// (packages/docker/src/singleton.ts:31-129): the configured endpoint list
// with one cached Client per endpoint. The docker tRPC router consumes it
// through a small interface (the router's DockerClientFactory).
//
// Construction failures (TLS files unreadable — the only runtime failure
// upstream can hit while building dockerode instances, singleton.ts:35-50)
// are kept in Failures so getEndpoints/getContainers can report them as
// "unavailable" endpoints instead of dropping them.
type Registry struct {
	mu        sync.Mutex
	endpoints []Endpoint
	failures  []EndpointFailure
	clients   map[string]*Client
	getenv    func(string) string
}

// NewRegistry builds a registry over the given endpoints. The endpoints are
// the CONSTRUCTED instances (upstream getInstances()); failures lists
// descriptors that failed construction.
func NewRegistry(endpoints []Endpoint, failures []EndpointFailure) *Registry {
	return &Registry{
		endpoints: append([]Endpoint(nil), endpoints...),
		failures:  append([]EndpointFailure(nil), failures...),
		clients:   make(map[string]*Client),
	}
}

// RegistryFromEnv builds the registry from the environment
// (EndpointsFromEnv).
func RegistryFromEnv(getenv func(string) string) (*Registry, error) {
	endpoints, failures, err := EndpointsFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	r := NewRegistry(endpoints, failures)
	r.getenv = getenv
	return r, nil
}

// Endpoints lists the usable endpoints (DockerSingleton.getInstances).
func (r *Registry) Endpoints() []Endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Endpoint(nil), r.endpoints...)
}

// Failures lists the endpoints that failed construction
// (DockerSingleton.getInitializationFailures).
func (r *Registry) Failures() []EndpointFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]EndpointFailure(nil), r.failures...)
}

// Endpoint returns the descriptor with the given id
// (DockerSingleton.findInstance).
func (r *Registry) Endpoint(id string) (Endpoint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ep := range r.endpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return Endpoint{}, false
}

// HasCapability is DockerSingleton.hasCapability
// (singleton.ts:106-108): false for unknown endpoints.
func (r *Registry) HasCapability(id string, capability Capability) bool {
	ep, ok := r.Endpoint(id)
	return ok && ep.HasCapability(capability)
}

// ClientFor returns the cached client for an endpoint
// (DockerSingleton.findInstance + instance access). Errors:
// ErrUnknownEndpoint for an unconfigured id, ErrTLSNotImplemented for the
// deferred TLS transport.
func (r *Registry) ClientFor(id string) (*Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[id]; ok {
		return c, nil
	}
	for _, ep := range r.endpoints {
		if ep.ID != id {
			continue
		}
		c, err := NewClient(ep)
		if err != nil {
			return nil, err
		}
		r.clients[id] = c
		return c, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownEndpoint, id)
}

// Reset rebuilds the registry from the environment — DockerSingleton.reset()
// (singleton.ts:125-128), called by docker.refreshInventory. Existing cached
// clients are dropped; in-flight requests keep their transport until done.
func (r *Registry) Reset() error {
	if r.getenv == nil {
		return nil
	}
	endpoints, failures, err := EndpointsFromEnv(r.getenv)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = endpoints
	r.failures = failures
	r.clients = make(map[string]*Client)
	return nil
}

// ---------------------------------------------------------------------------
// Endpoint-level convenience surface (the API the tRPC router consumes).
// Each call resolves the endpoint client and forwards; the router never
// touches *Client directly except for log streaming.

// Containers lists the containers of one endpoint with upstream's flags:
// GET /containers/json?all=true, no filters (request-handler/src/docker.ts:386).
func (r *Registry) Containers(ctx context.Context, endpointID string, all bool) ([]ContainerInfo, error) {
	c, err := r.ClientFor(endpointID)
	if err != nil {
		return nil, err
	}
	return c.ListContainers(ctx, all)
}

// ContainerAction performs one lifecycle action (start/stop/restart/remove)
// with the exact upstream POST/DELETE semantics.
func (r *Registry) ContainerAction(ctx context.Context, endpointID, containerID string, action Action) error {
	c, err := r.ClientFor(endpointID)
	if err != nil {
		return err
	}
	return c.Action(ctx, containerID, action)
}

// ContainerInspect reports whether the container exists on the endpoint
// (the findDockerContainerAsync existence check).
func (r *Registry) ContainerInspect(ctx context.Context, endpointID, containerID string) error {
	c, err := r.ClientFor(endpointID)
	if err != nil {
		return err
	}
	return c.Inspect(ctx, containerID)
}

// ContainerStats fetches a one-shot stats snapshot
// (?stream=false&one-shot=true).
func (r *Registry) ContainerStats(ctx context.Context, endpointID, containerID string) (ContainerStats, error) {
	c, err := r.ClientFor(endpointID)
	if err != nil {
		return ContainerStats{}, err
	}
	return c.StatsOneShot(ctx, containerID)
}

// ContainerLogs opens the raw log stream (multiplexed unless TTY). follow
// mirrors the upstream query flag; the caller must close the reader.
func (r *Registry) ContainerLogs(ctx context.Context, endpointID, containerID string, follow bool, tail int, since time.Time) (io.ReadCloser, error) {
	c, err := r.ClientFor(endpointID)
	if err != nil {
		return nil, err
	}
	return c.Logs(ctx, containerID, LogsOptions{Tail: tail, Follow: follow, Since: since})
}

// ---------------------------------------------------------------------------
// Stats helpers shared by the router (ports of calculateCpuUsage and
// calculateMemoryUsage plus the cross-poll CPU counter cache).

// CpuUsageCounters are CPU counters kept as the delta baseline between
// one-shot polls (request-handler/src/docker.ts:341-357): Docker's one-shot
// stats response has no usable precpu baseline, so the previous poll's
// counters serve as baseline instead.
type CpuUsageCounters struct {
	TotalUsage  uint64
	SystemUsage uint64
}

// StatsCache is the process-local counter cache
// (cachedCpuUsageCounters, request-handler/src/docker.ts:356-357): keyed by
// "<endpointId>:<containerId>", entries expire after cpuUsageCountersCacheTtlMs
// (10 minutes, docker.ts:357).
type StatsCache struct {
	mu   sync.Mutex
	byID map[string]cachedCounters
	now  func() time.Time
}

type cachedCounters struct {
	counters CpuUsageCounters
	cachedAt time.Time
}

// StatsCacheTTL is cpuUsageCountersCacheTtlMs (docker.ts:357).
const StatsCacheTTL = 10 * time.Minute

// NewStatsCache builds an empty cache. now is injected for tests.
func NewStatsCache(now func() time.Time) *StatsCache {
	if now == nil {
		now = time.Now
	}
	return &StatsCache{byID: make(map[string]cachedCounters), now: now}
}

// Get returns the cached counters for the key, dropping expired entries
// (the eviction loop at the top of getContainersWithStatsAsync,
// docker.ts:368-373).
func (sc *StatsCache) Get(key string) (CpuUsageCounters, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.byID[key]
	if !ok {
		return CpuUsageCounters{}, false
	}
	if sc.now().Sub(entry.cachedAt) >= StatsCacheTTL {
		delete(sc.byID, key)
		return CpuUsageCounters{}, false
	}
	return entry.counters, true
}

// Set remembers this poll's counters so the next one-shot response has a
// delta baseline (docker.ts:486-488).
func (sc *StatsCache) Set(key string, counters CpuUsageCounters) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.byID[key] = cachedCounters{counters: counters, cachedAt: sc.now()}
}

// CalculateCPUUsage is calculateCpuUsage (docker.ts:522-548): the share of
// system CPU the container used since the previous sample. Missing/invalid
// stats (exited containers, Podman) yield 0. One-shot responses carry a
// zeroed precpu_stats, so the previous poll's counters are the fallback
// baseline.
func CalculateCPUUsage(stats ContainerStats, previous *CpuUsageCounters) float64 {
	// Missing or invalid cpu_stats → 0 (Podman responses).
	if stats.CPUStats.OnlineCPUs == 0 || stats.CPUStats.CPUUsage.TotalUsage == 0 {
		return 0
	}
	hasPrecpuBaseline := stats.PreCPUStats.SystemCPUUsage > 0
	previousTotalUsage := stats.PreCPUStats.CPUUsage.TotalUsage
	previousSystemUsage := stats.PreCPUStats.SystemCPUUsage
	if !hasPrecpuBaseline && previous != nil {
		previousTotalUsage = previous.TotalUsage
		previousSystemUsage = previous.SystemUsage
	}
	// Signed deltas: counters are monotonic in practice, but a container
	// restart can regress them; upstream computes on JS numbers (negative
	// deltas then fail the checks below), so the port subtracts in int64 to
	// avoid the uint64 wrap.
	cpuDelta := int64(stats.CPUStats.CPUUsage.TotalUsage) - int64(previousTotalUsage)
	systemDelta := int64(stats.CPUStats.SystemCPUUsage) - int64(previousSystemUsage)
	if systemDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	return float64(cpuDelta) / float64(systemDelta) * 100
}

// CalculateMemoryUsage is calculateMemoryUsage (docker.ts:550-567): usage
// minus the cache/inactive-file accounting (cgroup v1 reports cache, cgroup
// v2 total_inactive_file, Podman inactive_file).
func CalculateMemoryUsage(stats ContainerStats) float64 {
	if stats.MemoryStats.Usage == 0 {
		return 0
	}
	inactive := uint64(0)
	if stats.MemoryStats.Stats.Cache != nil {
		inactive = *stats.MemoryStats.Stats.Cache
	} else if stats.MemoryStats.Stats.TotalInactiveFile != nil {
		inactive = *stats.MemoryStats.Stats.TotalInactiveFile
	} else if stats.MemoryStats.Stats.InactiveFile != nil {
		inactive = *stats.MemoryStats.Stats.InactiveFile
	}
	return float64(stats.MemoryStats.Usage) - float64(inactive)
}
