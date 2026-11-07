package docker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/router/docker"
	dockerclient "github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Router tests: procedure shapes, permission gates and the Writable wrapper
// (house style of internal/api/router/board).

func TestPermissionGates(t *testing.T) {
	api := newTestAPI(t, nil)
	api.seedUser(t, testUser{id: "u-bob", name: "bob"}) // signed in, no admin
	seedAliceAdmin(t, api)

	cases := []struct{ path, input string }{
		{"/api/trpc/docker.getContainers", ""},
		{"/api/trpc/docker.getEndpoints", ""},
		{"/api/trpc/docker.logs", `{"endpointId":"ep","id":"abc"}`},
		{"/api/trpc/docker.startAll", `{"targets":[{"endpointId":"ep","id":"abc"}]}`},
		{"/api/trpc/docker.stopAll", `{"targets":[{"endpointId":"ep","id":"abc"}]}`},
		{"/api/trpc/docker.restartAll", `{"targets":[{"endpointId":"ep","id":"abc"}]}`},
		{"/api/trpc/docker.removeAll", `{"targets":[{"endpointId":"ep","id":"abc"}]}`},
		{"/api/trpc/docker.refreshInventory", "null"},
	}
	for _, tc := range cases {
		isQuery := strings.HasSuffix(tc.path, ".getContainers") ||
			strings.HasSuffix(tc.path, ".getEndpoints") || strings.HasSuffix(tc.path, ".logs")
		if isQuery {
			// Anonymous → UNAUTHORIZED (permissionRequiredProcedure extends
			// protectedProcedure, packages/api/src/trpc.ts:195-207).
			api.expectError(t, "", "GET", tc.path+encodedInput(tc.input), "", "UNAUTHORIZED")
			// Signed in without admin → FORBIDDEN "Permission denied".
			message := api.expectError(t, "bob", "GET", tc.path+encodedInput(tc.input), "", "FORBIDDEN")
			if message != "Permission denied" {
				t.Fatalf("%s: message = %q", tc.path, message)
			}
			continue
		}
		api.expectError(t, "", "POST", tc.path, `{"json":`+tc.input+`}`, "UNAUTHORIZED")
		api.expectError(t, "bob", "POST", tc.path, `{"json":`+tc.input+`}`, "FORBIDDEN")
	}

	// Admin via GROUP permissions (the upstream mechanism, not the fork
	// IsAdministrator flag).
	api.seedUser(t, testUser{id: "u-carol", name: "carol"})
	api.seedGroupAdmin(t, "u-carol")
	_, body := api.call(t, "carol", "GET", "/api/trpc/docker.getEndpoints"+encodedInput(""), "")
	unwrapData(t, body)
}

func TestDisabledDockerGate(t *testing.T) {
	api := newTestAPI(t, func(_ *fakeEngine, opts *docker.Options) {
		opts.Disabled = true
	})
	seedAliceAdmin(t, api)

	// dockerMiddleware: NOT_FOUND "Docker route is not available" for every
	// procedure (packages/api/src/middlewares/docker.ts).
	for _, path := range []string{
		"/api/trpc/docker.getContainers",
		"/api/trpc/docker.getEndpoints",
	} {
		message := api.expectError(t, "alice", "GET", path+encodedInput(""), "", "NOT_FOUND")
		if message != "Docker route is not available" {
			t.Fatalf("%s: message = %q", path, message)
		}
	}
	api.expectError(t, "alice", "POST", "/api/trpc/docker.startAll",
		`{"json":{"targets":[{"endpointId":"ep","id":"abc"}]}}`, "NOT_FOUND")

	// The subscription handler carries the same gate.
	alice := api.sessions["alice"]
	_, err := api.subscribe(t, alice, `{"endpointId":"ep","id":"abc"}`)
	code, message := trpcErrorCode(t, err)
	if code != "NOT_FOUND" || message != "Docker route is not available" {
		t.Fatalf("subscribe disabled gate: %s %q", code, message)
	}
}

func TestGetContainersShape(t *testing.T) {
	var cache uint64 = 256
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.containers = []dockerclient.ContainerInfo{
			{
				ID:     "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
				Names:  []string{"/sonarr"},
				Image:  "lscr.io/linuxserver/sonarr:latest",
				State:  "running",
				Labels: map[string]string{},
				Ports:  []dockerclient.ContainerPort{{IP: "0.0.0.0", PrivatePort: 8989, PublicPort: 8989, Type: "tcp"}},
			},
			{
				ID:     "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
				Names:  []string{"/secret"},
				Image:  "ghcr.io/secret:1",
				State:  "running",
				Labels: map[string]string{"homarr.hide": ""},
				Ports:  nil,
			},
			{
				ID:     "1111111111111111111111111111111111111111111111111111111111111111",
				Names:  []string{"/labeled"},
				Image:  "nginx:latest",
				State:  "exited",
				Labels: map[string]string{"homarr.icon": "https://example.com/x.svg"},
				Ports:  nil,
			},
		}
		fe.stats.CPUStats.OnlineCPUs = 4
		fe.stats.CPUStats.CPUUsage.TotalUsage = 2000
		fe.stats.CPUStats.SystemCPUUsage = 10000
		fe.stats.MemoryStats.Usage = 1024
		fe.stats.MemoryStats.Stats.Cache = &cache
	})
	seedAliceAdmin(t, api)

	_, body := api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	var out struct {
		Containers []struct {
			ReleaseSourceToken string `json:"releaseSourceToken"`
			EndpointID         string `json:"endpointId"`
			EndpointName       string `json:"endpointName"`
			ResourceID         string `json:"resourceId"`
			Name               string `json:"name"`
			ID                 string `json:"id"`
			Host               string `json:"host"`
			State              string `json:"state"`
			Image              string `json:"image"`
			Ports              []struct {
				IP          string `json:"IP"`
				PrivatePort int    `json:"PrivatePort"`
				PublicPort  int    `json:"PublicPort"`
				Type        string `json:"Type"`
			} `json:"ports"`
			IconURL     *string `json:"iconUrl"`
			CPUUsage    float64 `json:"cpuUsage"`
			MemoryUsage float64 `json:"memoryUsage"`
		} `json:"containers"`
		Endpoints []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Kind   string `json:"kind"`
		} `json:"endpoints"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode output: %v (%s)", err, body)
	}

	// The homarr.hide container is filtered out (docker.ts:391).
	if len(out.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (homarr.hide filtered)", len(out.Containers))
	}
	sonarr := out.Containers[0]
	if sonarr.Name != "sonarr" || sonarr.State != "running" || sonarr.Host == "" {
		t.Fatalf("sonarr = %+v", sonarr)
	}
	if sonarr.ResourceID != sonarr.EndpointID+":"+sonarr.ID {
		t.Fatalf("resourceId = %q", sonarr.ResourceID)
	}
	// CPU 20%, memory 1024-256 = 768.
	if sonarr.CPUUsage != 20 || sonarr.MemoryUsage != 768 {
		t.Fatalf("usage = %v/%v, want 20/768", sonarr.CPUUsage, sonarr.MemoryUsage)
	}
	// The release token is a 64-char hex HMAC (lscr images map to
	// linuxServerIO).
	if len(sonarr.ReleaseSourceToken) != 64 {
		t.Fatalf("releaseSourceToken = %q", sonarr.ReleaseSourceToken)
	}
	if len(sonarr.Ports) != 1 || sonarr.Ports[0].PrivatePort != 8989 {
		t.Fatalf("ports = %+v", sonarr.Ports)
	}
	// The homarr.icon label wins over the icon table.
	if out.Containers[1].IconURL == nil || *out.Containers[1].IconURL != "https://example.com/x.svg" {
		t.Fatalf("labeled iconUrl = %v", out.Containers[1].IconURL)
	}
	// No icon table rows: iconUrl null.
	if sonarr.IconURL != nil {
		t.Fatalf("iconUrl = %v, want null", sonarr.IconURL)
	}

	if len(out.Endpoints) != 1 || out.Endpoints[0].Status != "available" || out.Endpoints[0].Kind != "docker" {
		t.Fatalf("endpoints = %+v", out.Endpoints)
	}
	if out.Timestamp == "" {
		t.Fatalf("timestamp missing")
	}
}

func TestGetContainersEndpointUnavailableAndDegraded(t *testing.T) {
	// Two endpoints: the fake engine (available) and a closed port
	// (unreachable → unavailable; docker.ts:396-421).
	fe := newFakeEngine(t)
	down := dockerclient.LegacyTCPEndpoint("127.0.0.1", closedPort(t))
	factory := dockerclient.NewRegistry([]dockerclient.Endpoint{fe.endpoint(), down}, nil)
	api := buildAPICustom(t, fe, factory)
	seedAliceAdmin(t, api)

	_, body := api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	var out struct {
		Endpoints []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	statuses := map[string]string{}
	for _, ep := range out.Endpoints {
		statuses[ep.ID] = ep.Status
	}
	if statuses[fe.endpoint().ID] != "available" || statuses[down.ID] != "unavailable" {
		t.Fatalf("statuses = %+v", statuses)
	}

	// A stats read failure DEGRADES a healthy endpoint (docker.ts:462-467,
	// 513-519): the fake engine answers 500 on /stats. refreshInventory
	// clears the 10s cache from the first call.
	fe.statsStatus = 500
	fe.containers = []dockerclient.ContainerInfo{{
		ID: "abc", Names: []string{"/app"}, Image: "nginx", State: "running", Labels: map[string]string{},
	}}
	_, body = api.call(t, "alice", "POST", "/api/trpc/docker.refreshInventory", `{"json":null}`)
	unwrapData(t, body)
	_, body = api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	out.Endpoints = nil
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	fe.statsStatus = 0
	for _, ep := range out.Endpoints {
		if ep.ID == fe.endpoint().ID && ep.Status != "degraded" {
			t.Fatalf("engine endpoint = %s, want degraded", ep.Status)
		}
	}
}

func TestGetContainersInputValidation(t *testing.T) {
	api := newTestAPI(t, nil)
	seedAliceAdmin(t, api)

	// More than 100 endpoint ids → BAD_REQUEST (zod max(100)); note the
	// normalization treats an EMPTY array as "all endpoints"
	// (normalizeGetContainersInput, docker-router.ts:28-31).
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "ep"
	}
	payload, _ := json.Marshal(map[string]any{"endpointIds": ids})
	api.expectError(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(string(payload)), "", "BAD_REQUEST")
}

func TestActionResultsAndWritable(t *testing.T) {
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.containers = []dockerclient.ContainerInfo{{
			ID: "abc", Names: []string{"/app"}, Image: "nginx", State: "running", Labels: map[string]string{},
		}}
	})
	api.seedUser(t, testUser{id: "u-alice", name: "alice", admin: true})
	api.seedUser(t, testUser{id: "u-roy", name: "roy", admin: true, ro: true}) // read-only API key

	epID := api.engine.endpoint().ID
	start := `{"json":{"targets":[{"endpointId":"` + epID + `","id":"abc"}]}}`

	// Writable gate: the read-only API key caller gets FORBIDDEN on the
	// mutation (credentialProcedure, packages/api/src/trpc.ts:130-135) while
	// queries still pass.
	api.expectError(t, "roy", "POST", "/api/trpc/docker.startAll", start, "FORBIDDEN")
	_, body := api.call(t, "roy", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	unwrapData(t, body)

	_, body = api.call(t, "alice", "POST", "/api/trpc/docker.startAll", start)
	var results []struct {
		Target struct {
			EndpointID string `json:"endpointId"`
			ID         string `json:"id"`
		} `json:"target"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode results: %v (%s)", err, body)
	}
	if len(results) != 1 || !results[0].Success || results[0].Error != "" {
		t.Fatalf("results = %+v", results)
	}

	// Input validation: targets min(1) / max(100) / non-empty fields.
	api.expectError(t, "alice", "POST", "/api/trpc/docker.startAll", `{"json":{"targets":[]}}`, "BAD_REQUEST")
	many := strings.TrimSuffix(strings.Repeat(`{"endpointId":"e","id":"c"},`, 101), ",")
	api.expectError(t, "alice", "POST", "/api/trpc/docker.startAll", `{"json":{"targets":[`+many+`]]}}`, "BAD_REQUEST")
	api.expectError(t, "alice", "POST", "/api/trpc/docker.startAll", `{"json":{"targets":[{"endpointId":"","id":"abc"}]}}`, "BAD_REQUEST")

	// Missing container: inspect 404 → per-target error string, HTTP success
	// (the mutation never throws, docker-router.ts:272-273).
	api.engine.failInspect = true
	_, body = api.call(t, "alice", "POST", "/api/trpc/docker.startAll", start)
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if results[0].Success || results[0].Error != "Container not found" {
		t.Fatalf("results = %+v", results)
	}
	api.engine.failInspect = false

	// Engine action failure → per-target error carrying the message
	// (docker-router.ts:280-287).
	api.engine.actionStatus = 409
	api.engine.actionBody = `{"message":"container abc is already started"}`
	_, body = api.call(t, "alice", "POST", "/api/trpc/docker.startAll", start)
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if results[0].Success || results[0].Error == "" {
		t.Fatalf("results = %+v", results)
	}
	api.engine.actionStatus = 0

	// Unknown endpoint → per-target error, not a thrown error.
	_, body = api.call(t, "alice", "POST", "/api/trpc/docker.removeAll",
		`{"json":{"targets":[{"endpointId":"unknown","id":"abc"}]}}`)
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode results: %v (%s)", err, body)
	}
	if results[0].Success {
		t.Fatalf("unknown endpoint should fail the target: %+v", results[0])
	}
}

func TestRemoveAllCapabilityError(t *testing.T) {
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.containers = []dockerclient.ContainerInfo{{
			ID: "abc", Names: []string{"/app"}, Image: "nginx", State: "running", Labels: map[string]string{},
		}}
	})
	seedAliceAdmin(t, api)

	// An endpoint that exists but lacks the capability → per-target error
	// "Endpoint does not permit remove actions" (docker-router.ts:268-270).
	ep := limitedEndpoint(api.engine, dockerclient.CapabilityInventory)
	limited := dockerclient.NewRegistry([]dockerclient.Endpoint{ep}, nil)
	api2 := api.withRegistry(t, limited)

	_, body := api2.call(t, "alice", "POST", "/api/trpc/docker.removeAll",
		`{"json":{"targets":[{"endpointId":"`+ep.ID+`","id":"abc"}]}}`)
	var results []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &results); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if results[0].Success || results[0].Error != "Endpoint does not permit remove actions" {
		t.Fatalf("results = %+v", results)
	}
}

func TestLogsQuery(t *testing.T) {
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.staticLogs = concatFrames(
			encodeFrame(1, "line one\n"),
			encodeFrame(2, "err line\n"),
		)
	})
	seedAliceAdmin(t, api)
	epID := api.engine.endpoint().ID

	_, body := api.call(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+epID+`","id":"abc","tail":10}`), "")
	var out struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Logs != "line one\nerr line\n" {
		t.Fatalf("logs = %q", out.Logs)
	}

	// Default tail (input.tail ?? 200, docker-router.ts:188).
	api.call(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+epID+`","id":"abc"}`), "")
	last := api.engine.lastRequest()
	if !strings.Contains(last.Query, "tail=200") {
		t.Fatalf("default tail query = %q", last.Query)
	}

	// tail validation 1..1000.
	api.expectError(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+epID+`","id":"abc","tail":0}`), "", "BAD_REQUEST")
	api.expectError(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+epID+`","id":"abc","tail":1001}`), "", "BAD_REQUEST")

	// Unknown container → NOT_FOUND "Container not found"
	// (findDockerContainerAsync resolves null on inspect error).
	api.engine.failInspect = true
	message := api.expectError(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+epID+`","id":"zzz"}`), "", "NOT_FOUND")
	if message != "Container not found" {
		t.Fatalf("message = %q", message)
	}
	api.engine.failInspect = false

	// Missing logs capability → FORBIDDEN with the exact upstream message
	// (assertDockerEndpointCapability, docker-router.ts:297-303).
	ep := limitedEndpoint(api.engine, dockerclient.CapabilityInventory)
	limited := dockerclient.NewRegistry([]dockerclient.Endpoint{ep}, nil)
	api2 := api.withRegistry(t, limited)
	message = api2.expectError(t, "alice", "GET",
		"/api/trpc/docker.logs"+encodedInput(`{"endpointId":"`+ep.ID+`","id":"abc"}`), "",
		"FORBIDDEN")
	if message != "Docker endpoint does not permit logs access" {
		t.Fatalf("message = %q", message)
	}
}

func TestRefreshInventoryInvalidatesCache(t *testing.T) {
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.containers = []dockerclient.ContainerInfo{{
			ID: "abc", Names: []string{"/app"}, Image: "nginx", State: "running", Labels: map[string]string{},
		}}
	})
	seedAliceAdmin(t, api)

	api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	first := api.engine.requestCount()
	api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	if api.engine.requestCount() != first {
		t.Fatalf("second getContainers should hit the 10s cache")
	}

	// refreshInventory resets the endpoint registry and invalidates the
	// cache (docker-router.ts:67-70).
	_, body := api.call(t, "alice", "POST", "/api/trpc/docker.refreshInventory", `{"json":null}`)
	unwrapData(t, body)
	api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	if api.engine.requestCount() == first {
		t.Fatalf("cache was not invalidated by refreshInventory")
	}
}

func TestGetEndpoints(t *testing.T) {
	api := newTestAPI(t, nil)
	seedAliceAdmin(t, api)

	_, body := api.call(t, "alice", "GET", "/api/trpc/docker.getEndpoints"+encodedInput(""), "")
	var endpoints []struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Status       string   `json:"status"`
		Kind         string   `json:"kind"`
		Transport    string   `json:"transport"`
		Capabilities []string `json:"capabilities"`
		Source       string   `json:"source"`
		Scope        string   `json:"scope"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &endpoints); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(endpoints) != 1 || endpoints[0].Status != "available" || endpoints[0].Transport != "tcp" ||
		endpoints[0].Source != "legacy" || endpoints[0].Scope != "admin" || len(endpoints[0].Capabilities) != 4 {
		t.Fatalf("endpoints = %+v", endpoints)
	}
}

func TestIconFromTable(t *testing.T) {
	// Containers without label icons fall back to the icon table best match
	// (docker.ts:438-445, 500-503): the icon whose name contains the image
	// name at the earliest index wins.
	api := newTestAPI(t, func(fe *fakeEngine, _ *docker.Options) {
		fe.containers = []dockerclient.ContainerInfo{{
			ID: "abc", Names: []string{"/app"}, Image: "ghcr.io/app:1", State: "running", Labels: map[string]string{},
		}}
	})
	seedAliceAdmin(t, api)
	ctx := context.Background()
	if _, err := api.db.ExecContext(ctx,
		`INSERT INTO "iconRepository" (id, slug) VALUES ('r1', 'test')`); err != nil {
		t.Fatalf("seed icon repository: %v", err)
	}
	for _, seed := range []struct{ name, url string }{
		{"app", "https://icons/app.svg"},           // exact containment, index 0
		{"another-app", "https://icons/other.png"}, // later containment
	} {
		if _, err := api.db.ExecContext(ctx,
			`INSERT INTO "icon" (id, name, url, checksum, icon_repository_id) VALUES (?, ?, ?, ?, ?)`,
			seed.name, seed.name, seed.url, "c", "r1"); err != nil {
			t.Fatalf("seed icon: %v", err)
		}
	}

	_, body := api.call(t, "alice", "GET", "/api/trpc/docker.getContainers"+encodedInput(""), "")
	var out struct {
		Containers []struct {
			IconURL *string `json:"iconUrl"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(unwrapData(t, body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out.Containers) != 1 || out.Containers[0].IconURL == nil || *out.Containers[0].IconURL != "https://icons/app.svg" {
		t.Fatalf("iconUrl = %+v", out.Containers)
	}
}
