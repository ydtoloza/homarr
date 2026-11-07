package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Wire shapes ----------------------------------------------------------------

// service carries the dependencies shared by all procedures of this package.
type service struct {
	db      *db.DB
	factory DockerClientFactory
	opts    Options
	// stats caches CPU counters between one-shot polls (the upstream
	// cachedCpuUsageCounters of getContainersWithStatsAsync).
	stats *docker.StatsCache
	// pumps owns the log event pumps (one Engine stream per active
	// endpoint/container pair, shared by every subscriber).
	pumps *pumpRegistry
	// cache is the 10 s widget cache of docker.getContainers
	// (createWidgetRequestHandler "docker-containers").
	cache *containersCache
}

// dockerGate is the dockerMiddleware of upstream
// (packages/api/src/middlewares/docker.ts): NOT_FOUND "Docker route is not
// available" unless ENABLE_DOCKER (default true) or demo mode. Demo mode is
// deferred (DOCKER.md section 6).
func (s *service) dockerGate(_ context.Context) *trpc.Error {
	if s.opts.Disabled {
		return trpc.NewError(trpc.CodeNotFound, "Docker route is not available")
	}
	return nil
}

// requireAdmin is permissionRequiredProcedure.requiresPermission("admin"),
// the gate of every procedure of the upstream docker router.
func (s *service) requireAdmin(ctx context.Context) *trpc.Error {
	caller, terr := admin.ResolveCaller(ctx, s.db)
	if terr != nil {
		return terr
	}
	return admin.RequirePermission(caller, "admin")
}

// containerOutput is the DockerContainer mapping of the getContainers
// procedure (docker-router.ts:89-101 and :305-319).
type containerOutput struct {
	// ReleaseSourceToken is omitted when the image has no release source
	// (upstream: `releaseSourceToken` left undefined).
	ReleaseSourceToken string                 `json:"releaseSourceToken,omitempty"`
	EndpointID         string                 `json:"endpointId"`
	EndpointName       string                 `json:"endpointName"`
	ResourceID         string                 `json:"resourceId"`
	Name               string                 `json:"name"`
	ID                 string                 `json:"id"`
	Host               string                 `json:"host"`
	State              string                 `json:"state"`
	Image              string                 `json:"image"`
	Ports              []docker.ContainerPort `json:"ports,omitempty"`
	IconURL            *string                `json:"iconUrl"`
	CPUUsage           float64                `json:"cpuUsage"`
	MemoryUsage        float64                `json:"memoryUsage"`

	// cpuFailed marks a failed stats read: the endpoint degrades
	// (upstream degradedEndpointIds, docker.ts:462-463). Never serialized.
	cpuFailed bool
}

// endpointStatusOutput is DockerEndpointStatus
// (packages/docker/src/index.ts:19-28): all fields are set for the objects
// the router emits.
type endpointStatusOutput struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Status       string              `json:"status"` // available | degraded | unavailable
	Kind         string              `json:"kind"`
	Transport    string              `json:"transport"`
	Capabilities []docker.Capability `json:"capabilities"`
	Source       string              `json:"source"`
	Scope        string              `json:"scope"`
}

// dockerTarget is DockerContainerTarget
// (packages/docker/src/index.ts:14-17).
type dockerTarget struct {
	EndpointID string `json:"endpointId"`
	ID         string `json:"id"`
}

// actionResult is the per-target result of performDockerContainerActionsAsync
// (docker-router.ts:265-288): errors ride INSIDE the result objects, the
// mutation never throws.
type actionResult struct {
	Target  dockerTarget `json:"target"`
	Success bool         `json:"success"`
	Error   string       `json:"error,omitempty"`
}

// containersOutput is the getContainers response: {containers, endpoints,
// timestamp}. timestamp is an ISO-8601 string (upstream serializes a Date
// through superjson; the frontend dayjs consumer accepts both).
type containersOutput struct {
	Containers []containerOutput      `json:"containers"`
	Endpoints  []endpointStatusOutput `json:"endpoints"`
	Timestamp  string                 `json:"timestamp"`
}

// getContainers --------------------------------------------------------------

// getContainersInput is the zod input of docker.getContainers
// (docker-router.ts:81): {endpointIds?: string[1..100]}, optional. Empty or
// absent endpointIds means ALL endpoints (normalizeGetContainersInput,
// docker-router.ts:28-31).
type getContainersInput struct {
	EndpointIDs []string `json:"endpointIds"`
}

func (s *service) dockerGetContainers(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	if terr := s.requireAdmin(ctx); terr != nil {
		return nil, terr
	}
	var in getContainersInput
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, trpc.NewError(trpc.CodeBadRequest, "Failed to parse input: "+err.Error())
		}
	}
	// zod: endpointIds: z.array(z.string().min(1)).max(100).optional()
	if len(in.EndpointIDs) > 100 {
		return nil, trpc.NewError(trpc.CodeBadRequest, `Input "endpointIds" must contain at most 100 endpoints`)
	}
	for _, id := range in.EndpointIDs {
		if id == "" {
			return nil, trpc.NewError(trpc.CodeBadRequest, `Input "endpointIds" entries must contain at least 1 character(s)`)
		}
	}

	if out, ok := s.cache.get(in.EndpointIDs); ok {
		return out, nil
	}

	out, terr := s.fetchContainers(ctx, in.EndpointIDs)
	if terr != nil {
		return nil, terr
	}
	s.cache.set(in.EndpointIDs, out)
	return out, nil
}

// fetchContainers is getContainersWithStatsAsync
// (packages/request-handler/src/docker.ts:364-520) with the release token
// mapping of the router (docker-router.ts:82-102).
func (s *service) fetchContainers(ctx context.Context, endpointIDs []string) (*containersOutput, *trpc.Error) {
	var selected map[string]bool
	if len(endpointIDs) > 0 {
		selected = make(map[string]bool, len(endpointIDs))
		for _, id := range endpointIDs {
			selected[id] = true
		}
	}
	includesAll := selected == nil

	endpoints := filterEndpoints(s.factory.Endpoints(), selected, includesAll)
	failures := filterFailures(s.factory.Failures(), selected, includesAll)

	type listing struct {
		endpoint   docker.Endpoint
		containers []docker.ContainerInfo
		err        error
	}

	// Promise.allSettled over the endpoints: an unreachable engine means an
	// "unavailable" endpoint, NOT a failed query (docker.ts:383-394, 423-437).
	listings := make([]listing, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(i int, ep docker.Endpoint) {
			defer wg.Done()
			ls := listing{endpoint: ep}
			ls.containers, ls.err = s.listContainers(ctx, ep.ID)
			listings[i] = ls
		}(i, ep)
	}
	wg.Wait()

	statuses := make([]endpointStatusOutput, 0, len(endpoints)+len(failures))
	degradedEndpointIDs := map[string]bool{}
	for _, ls := range listings {
		status := "available"
		if ls.err != nil {
			status = "unavailable"
		}
		statuses = append(statuses, endpointStatus(ls.endpoint, status))
	}

	var infos []listedContainer
	for _, ls := range listings {
		if ls.err != nil {
			continue // failed endpoints contribute no containers (flatMap skip)
		}
		for _, c := range ls.containers {
			// The homarr.hide label removes a container from the listing
			// (docker.ts:391).
			if _, hidden := c.Labels[dockerLabelsHide]; hidden {
				continue
			}
			infos = append(infos, listedContainer{endpoint: ls.endpoint, info: c})
		}
	}

	// Icon candidates from the icon table: one LIKE query per distinct image
	// name (upstream builds one findMany with OR of LIKEs, docker.ts:438-445;
	// the result set is equivalent).
	icons, terr := s.loadIconCandidates(ctx, infos)
	if terr != nil {
		return nil, terr
	}

	// One-shot stats per container, in parallel (upstream Promise.all,
	// docker.ts:448-509). Failures mark the endpoint degraded and yield zero
	// usage.
	outputs := make([]containerOutput, len(infos))
	var statsWG sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, lc := range infos {
		statsWG.Add(1)
		go func(i int, lc listedContainer) {
			defer statsWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outputs[i] = s.buildContainerOutput(ctx, lc, icons)
			if outputs[i].cpuFailed {
				degradedEndpointIDs[lc.endpoint.ID] = true
			}
		}(i, lc)
	}
	statsWG.Wait()

	for _, f := range failures {
		statuses = append(statuses, endpointStatus(f.Endpoint, "unavailable"))
	}
	for i := range statuses {
		if statuses[i].Status == "available" && degradedEndpointIDs[statuses[i].ID] {
			statuses[i].Status = "degraded"
		}
	}

	return &containersOutput{
		Containers: outputs,
		Endpoints:  statuses,
		Timestamp:  s.opts.now()().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}, nil
}

// listedContainer pairs a ContainerInfo with its endpoint.
type listedContainer struct {
	endpoint docker.Endpoint
	info     docker.ContainerInfo
}

func (s *service) buildContainerOutput(ctx context.Context, lc listedContainer, icons map[string][]iconRow) containerOutput {
	out := containerOutput{
		EndpointID:   lc.endpoint.ID,
		EndpointName: lc.endpoint.Name,
		ResourceID:   lc.endpoint.ID + ":" + lc.info.ID,
		Name:         lc.info.Name(),
		ID:           lc.info.ID,
		Host:         lc.endpoint.Host(),
		State:        lc.info.State,
		Image:        lc.info.Image,
		Ports:        lc.info.Ports,
	}

	// Icon resolution: docker/homepage labels first, then the icon table
	// best match (docker.ts:500-503).
	if labelIcon := resolveContainerIcon(lc.info.Labels); labelIcon != nil {
		out.IconURL = labelIcon
	} else if url := bestMatch(icons[extractImageName(lc.info.Image)], extractImageName(lc.info.Image)); url != "" {
		out.IconURL = &url
	}

	// Stats: one-shot per container; zeroed fallback on failure (the exact
	// zero value upstream substitutes, docker.ts:471-474).
	stats := docker.ContainerStats{}
	client, err := s.factory.ClientFor(lc.endpoint.ID)
	switch {
	case err != nil:
		out.cpuFailed = true
	default:
		got, err := client.StatsOneShot(ctx, lc.info.ID)
		if err != nil {
			out.cpuFailed = true
		} else {
			stats = got
		}
	}

	cacheKey := lc.endpoint.ID + ":" + lc.info.ID
	var previous *docker.CpuUsageCounters
	if cached, ok := s.stats.Get(cacheKey); ok {
		previous = &cached
	}
	out.CPUUsage = docker.CalculateCPUUsage(stats, previous)
	out.MemoryUsage = docker.CalculateMemoryUsage(stats)

	// Remember this poll's counters as the next one-shot's baseline
	// (docker.ts:486-488).
	if stats.CPUStats.CPUUsage.TotalUsage > 0 && stats.CPUStats.SystemCPUUsage > 0 {
		s.stats.Set(cacheKey, docker.CpuUsageCounters{
			TotalUsage:  stats.CPUStats.CPUUsage.TotalUsage,
			SystemUsage: stats.CPUStats.SystemCPUUsage,
		})
	}

	out.ReleaseSourceToken = docker.ReleaseSourceToken(lc.info.Image, s.releaseTokenKey())
	return out
}

// listContainers runs one endpoint listing under the upstream per-request
// timeout (withDockerTimeoutAsync, docker.ts:20-42; 5000 ms,
// dockerWidgetEndpointTimeoutMs).
func (s *service) listContainers(ctx context.Context, endpointID string) ([]docker.ContainerInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, docker.DefaultTimeout)
	defer cancel()
	client, err := s.factory.ClientFor(endpointID)
	if err != nil {
		return nil, err
	}
	return client.ListContainers(cctx, true)
}

func endpointStatus(ep docker.Endpoint, status string) endpointStatusOutput {
	return endpointStatusOutput{
		ID:           ep.ID,
		Name:         ep.Name,
		Status:       status,
		Kind:         ep.Kind,
		Transport:    string(ep.Transport.Kind),
		Capabilities: ep.Capabilities,
		Source:       ep.Source,
		Scope:        ep.Scope,
	}
}

func filterEndpoints(endpoints []docker.Endpoint, selected map[string]bool, includesAll bool) []docker.Endpoint {
	out := make([]docker.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if includesAll || selected[ep.ID] {
			out = append(out, ep)
		}
	}
	return out
}

func filterFailures(failures []docker.EndpointFailure, selected map[string]bool, includesAll bool) []docker.EndpointFailure {
	out := make([]docker.EndpointFailure, 0, len(failures))
	for _, f := range failures {
		if includesAll || selected[f.Endpoint.ID] {
			out = append(out, f)
		}
	}
	return out
}

func (s *service) releaseTokenKey() []byte {
	if s.opts.ReleaseTokenKey != nil {
		return s.opts.ReleaseTokenKey
	}
	return docker.ReleaseTokenKey()
}

// getEndpoints ---------------------------------------------------------------

func (s *service) dockerGetEndpoints(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	if terr := s.requireAdmin(ctx); terr != nil {
		return nil, terr
	}
	// getDockerEndpointsAsync (request-handler/src/docker.ts:311-339):
	// instances are "available", construction failures "unavailable".
	endpoints := make([]endpointStatusOutput, 0)
	for _, ep := range s.factory.Endpoints() {
		endpoints = append(endpoints, endpointStatus(ep, "available"))
	}
	for _, f := range s.factory.Failures() {
		endpoints = append(endpoints, endpointStatus(f.Endpoint, "unavailable"))
	}
	return endpoints, nil
}

// refreshInventory -----------------------------------------------------------

func (s *service) dockerRefreshInventory(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	if terr := s.requireAdmin(ctx); terr != nil {
		return nil, terr
	}
	// The upstream mutation is DockerSingleton.reset() +
	// dockerContainersRequestHandler.invalidateCache() (docker-router.ts:67-70).
	if err := s.factory.Reset(); err != nil {
		return nil, trpc.Internal(err)
	}
	s.cache.invalidate()
	return map[string]any{}, nil
}

// actions --------------------------------------------------------------------

// containerTargetsInput is the zod input of the action mutations
// (docker-router.ts:22-26): {targets: [{endpointId, id}]} with 1..100
// targets.
type containerTargetsInput struct {
	Targets []dockerTarget `json:"targets"`
}

func parseTargetsInput(input json.RawMessage) (containerTargetsInput, *trpc.Error) {
	var in containerTargetsInput
	if len(input) == 0 || string(input) == "null" {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "targets" is required`)
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return in, trpc.NewError(trpc.CodeBadRequest, "Failed to parse input: "+err.Error())
	}
	if len(in.Targets) < 1 {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "targets" must contain at least 1 target`)
	}
	if len(in.Targets) > 100 {
		return in, trpc.NewError(trpc.CodeBadRequest, `Input "targets" must contain at most 100 targets`)
	}
	for i, t := range in.Targets {
		if t.EndpointID == "" || t.ID == "" {
			return in, trpc.NewError(trpc.CodeBadRequest,
				fmt.Sprintf(`Input "targets[%d]" requires non-empty "endpointId" and "id"`, i))
		}
	}
	return in, nil
}

// dockerAction is the shared body of startAll/stopAll/restartAll/removeAll —
// performDockerContainerActionsAsync (docker-router.ts:261-295).
func (s *service) dockerAction(ctx context.Context, input json.RawMessage, action docker.Action) (any, *trpc.Error) {
	if terr := s.dockerGate(ctx); terr != nil {
		return nil, terr
	}
	if terr := s.requireAdmin(ctx); terr != nil {
		return nil, terr
	}
	in, terr := parseTargetsInput(input)
	if terr != nil {
		return nil, terr
	}

	results := make([]actionResult, len(in.Targets))
	anySuccess := false
	for i, target := range in.Targets {
		results[i] = s.performAction(ctx, target, action)
		if results[i].Success {
			anySuccess = true
		}
	}
	// The cache invalidates ONCE when at least one action succeeded
	// (docker-router.ts:290-292).
	if anySuccess {
		s.cache.invalidate()
	}
	return results, nil
}

// performAction ports one target of performDockerContainerActionsAsync:
// capability gate, container lookup (inspect), then the action; every
// failure becomes a per-target error string, never a thrown tRPC error.
func (s *service) performAction(ctx context.Context, target dockerTarget, action docker.Action) actionResult {
	capability := docker.CapabilityLifecycle
	if action == docker.ActionRemove {
		capability = docker.CapabilityRemove
	}
	endpoint, known := s.factory.Endpoint(target.EndpointID)
	if !known || !endpoint.HasCapability(capability) {
		return actionResult{
			Target: target,
			Error:  fmt.Sprintf("Endpoint does not permit %s actions", capability),
		}
	}
	// findDockerContainerAsync (request-handler/src/docker.ts:241-252):
	// inspect error → the container is "not found".
	cctx, cancel := context.WithTimeout(ctx, docker.DefaultTimeout)
	defer cancel()
	client, err := s.factory.ClientFor(target.EndpointID)
	if err != nil || client.Inspect(cctx, target.ID) != nil {
		return actionResult{Target: target, Error: "Container not found"}
	}
	cctx, cancel = context.WithTimeout(ctx, docker.DefaultTimeout)
	defer cancel()
	if err := client.Action(cctx, target.ID, action); err != nil {
		return actionResult{Target: target, Error: err.Error()}
	}
	return actionResult{Target: target, Success: true}
}

func (s *service) dockerStartAll(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.dockerAction(ctx, input, docker.ActionStart)
}

func (s *service) dockerStopAll(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.dockerAction(ctx, input, docker.ActionStop)
}

func (s *service) dockerRestartAll(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.dockerAction(ctx, input, docker.ActionRestart)
}

func (s *service) dockerRemoveAll(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	return s.dockerAction(ctx, input, docker.ActionRemove)
}

// containers cache -----------------------------------------------------------

type cacheEntry struct {
	output    *containersOutput
	expiresAt time.Time
}

// containersCache is the in-process slice of the upstream widget request
// handler cache (DEFAULT_TTL_MS = 10 s, invalidateCache on refreshInventory
// and after successful actions). No single-flight: concurrent misses fetch
// concurrently, which upstream allows too (MAX_INFLIGHT_REQUESTS = 100).
type containersCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	byKey map[string]cacheEntry
}

func newContainersCache(ttl time.Duration, now func() time.Time) *containersCache {
	return &containersCache{ttl: ttl, now: now, byKey: make(map[string]cacheEntry)}
}

func cacheKey(endpointIDs []string) string {
	sorted := append([]string(nil), endpointIDs...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func (c *containersCache) get(endpointIDs []string) (*containersOutput, bool) {
	key := cacheKey(endpointIDs)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byKey[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.byKey, key)
		return nil, false
	}
	return entry.output, true
}

func (c *containersCache) set(endpointIDs []string, out *containersOutput) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[cacheKey(endpointIDs)] = cacheEntry{output: out, expiresAt: c.now().Add(c.ttl)}
}

func (c *containersCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey = make(map[string]cacheEntry)
}

// labels ---------------------------------------------------------------------

// dockerLabelsHide is dockerLabels.hide ("homarr.hide",
// packages/docker/src/labels.ts:2).
const dockerLabelsHide = "homarr.hide"
