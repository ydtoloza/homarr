// Package docker is the Go port of the docker-domain tRPC router of
// packages/api/src/router/docker/docker-router.ts. Procedures are registered
// under their exact upstream dotted paths ("docker.getContainers", ...)
// so the Homarr frontend keeps calling the same endpoints.
//
// Permission semantics mirror upstream exactly: every procedure is
// permissionRequiredProcedure.requiresPermission("admin") — anonymous callers
// get UNAUTHORIZED, signed-in callers without the admin permission get
// FORBIDDEN "Permission denied" — and mutations are additionally wrapped with
// admin.Writable (the credentialProcedure read-only API key gate, house rule
// of register.go in the board package).
//
// The dockerMiddleware gate (packages/api/src/middlewares/docker.ts) throws
// NOT_FOUND "Docker route is not available" when ENABLE_DOCKER is false
// (default true); the fork models it with Options.Disabled.
//
// Two upstream procedures are deferred (documented in DOCKER.md section 6):
// docker.reconcileServices and docker.getServiceHealth depend on the
// integration matching engine (packages/definitions/src/
// docker-integration-match.ts + docker-service-discovery.ts) and on
// integration rows with decrypted secrets, which the db layer ports in M4.
//
// The log subscription lives on the WebSocket hub, not on the HTTP router:
// upstream subscribes through wsLink (apps/nextjs trpc.tsx), and the hub
// registry is separate from the HTTP procedure set. Register returns the
// *Service whose SubscriptionHandlers() feeds hub.Register.
package docker

import (
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
	"github.com/ydtoloza/homarr/server/internal/ws"
)

// DockerClientFactory is the seam between the router and the Engine API
// layer (internal/integrations/docker). *docker.Registry implements it.
type DockerClientFactory interface {
	// Endpoints lists the usable endpoints (DockerSingleton.getInstances).
	Endpoints() []docker.Endpoint
	// Failures lists endpoints that failed construction
	// (DockerSingleton.getInitializationFailures).
	Failures() []docker.EndpointFailure
	// Endpoint resolves one descriptor (DockerSingleton.findInstance).
	Endpoint(id string) (docker.Endpoint, bool)
	// ClientFor returns the Engine API client of an endpoint.
	ClientFor(id string) (*docker.Client, error)
	// Reset rebuilds the endpoint list from the environment
	// (DockerSingleton.reset, used by docker.refreshInventory).
	Reset() error
}

// Options configures the docker-domain routers. The zero value is valid and
// mirrors the upstream defaults (docker enabled, 10 s containers cache,
// 30 min log pump idle timeout).
type Options struct {
	// Disabled is the ENABLE_DOCKER=false gate: every procedure answers
	// NOT_FOUND "Docker route is not available" (dockerMiddleware,
	// packages/api/src/middlewares/docker.ts). Docker is enabled by default
	// upstream (packages/docker/src/env.ts createBooleanSchema(true)), hence
	// the inverted flag keeps the zero value valid.
	Disabled bool
	// Bus receives the demultiplexed log line events of the log pumps on
	// topics "events.docker.logs.<endpointId>.<containerId>". nil disables
	// publishing (the log subscription then only delivers its Initial frame).
	Bus *bus.Bus
	// Now overrides the clock for the containers cache timestamp and the
	// pump idle timers (tests).
	Now func() time.Time
	// ContainersCacheTTL overrides the 10 s widget cache TTL
	// (DEFAULT_TTL_MS, packages/request-handler/src/lib/request-handler.ts:33).
	ContainersCacheTTL time.Duration
	// LogPumpIdleTimeout overrides the idle stop of log pumps (fork
	// lifecycle, DOCKER.md section 5).
	LogPumpIdleTimeout time.Duration
	// ReleaseTokenKey overrides the HMAC key of releaseSourceToken (tests).
	ReleaseTokenKey []byte
}

// Service carries the dependencies shared by the docker procedures. Register
// returns it so the composition root can wire SubscriptionHandlers() onto
// the WebSocket hub.
type Service struct {
	svc *service
}

// Register registers every (ported) procedure of the docker router under its
// exact upstream path. After chi mounts trpc.Router.Handler() at /api/trpc/*,
// call
//
//	svc := docker.Register(r, database, registry, docker.Options{Bus: bus})
//	for path, handler := range svc.SubscriptionHandlers() {
//	    hub.Register(path, handler)
//	}
//
// exactly once at startup (Register panics on duplicate paths, like
// trpc.Router.Register).
func Register(r *trpc.Router, database *db.DB, clientFactory DockerClientFactory, opts Options) *Service {
	s := &service{
		db:      database,
		factory: clientFactory,
		opts:    opts,
		stats:   docker.NewStatsCache(opts.now()),
		pumps:   newPumpRegistry(clientFactory, opts.Bus, opts.logPumpIdleTimeout(), opts.now()),
		cache:   newContainersCache(opts.containersCacheTTL(), opts.now()),
	}
	registerDockerRouter(r, s)
	return &Service{svc: s}
}

// SubscriptionHandlers returns the WebSocket subscription handlers of the
// docker router keyed by their tRPC path. The only docker subscription
// upstream is docker.subscribeLogs (docker-router.ts:201-256, consumed by the
// container logs terminal, apps/nextjs .../docker/logs/[containerId]/
// terminal.tsx:34).
func (s *Service) SubscriptionHandlers() map[string]ws.SubscriptionHandler {
	return map[string]ws.SubscriptionHandler{
		"docker.subscribeLogs": ws.SubscriptionHandlerFunc(s.svc.subscribeLogs),
	}
}

// registerDockerRouter mirrors dockerRouter (docker-router.ts:33-257).
// Mutations are wrapped with admin.Writable (the upstream credentialProcedure
// base middleware). Deferred: reconcileServices, getServiceHealth (package
// doc).
func registerDockerRouter(r *trpc.Router, s *service) {
	r.Register("docker.refreshInventory", admin.Writable(s.dockerRefreshInventory))
	r.Register("docker.getContainers", s.dockerGetContainers)
	r.Register("docker.getEndpoints", s.dockerGetEndpoints)
	r.Register("docker.startAll", admin.Writable(s.dockerStartAll))
	r.Register("docker.stopAll", admin.Writable(s.dockerStopAll))
	r.Register("docker.restartAll", admin.Writable(s.dockerRestartAll))
	r.Register("docker.removeAll", admin.Writable(s.dockerRemoveAll))
	r.Register("docker.logs", s.dockerLogs)
}

// option defaults -----------------------------------------------------------

func (o *Options) now() func() time.Time {
	if o != nil && o.Now != nil {
		return o.Now
	}
	return time.Now
}

func (o *Options) containersCacheTTL() time.Duration {
	if o != nil && o.ContainersCacheTTL > 0 {
		return o.ContainersCacheTTL
	}
	return 10 * time.Second
}

// logPumpIdleTimeout defaults to 30 minutes: the pump stops when the engine
// stream produces no frame for this long (fork lifecycle — the ws
// SubscriptionHandler contract has no unsubscribe callback).
func (o *Options) logPumpIdleTimeout() time.Duration {
	if o != nil && o.LogPumpIdleTimeout > 0 {
		return o.LogPumpIdleTimeout
	}
	return 30 * time.Minute
}
