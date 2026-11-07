package docker_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Client tests against the fake Engine API: exact request shapes (methods,
// paths, query params) and error mapping. The fixtures mirror the upstream
// request options of packages/request-handler/src/docker.ts.

func TestListContainersQuery(t *testing.T) {
	fe := newFakeEngine(t)
	fe.containers = []docker.ContainerInfo{{
		ID:     "a1b2c3d4e5f6",
		Names:  []string{"/sonarr"},
		Image:  "lscr.io/linuxserver/sonarr:latest",
		State:  "running",
		Labels: map[string]string{},
		Ports:  []docker.ContainerPort{{PrivatePort: 8989, PublicPort: 8989, Type: "tcp"}},
	}}
	reg := mustRegistry(t, fe)

	containers, err := reg.Containers(context.Background(), fe.endpoint().ID, true)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name() != "sonarr" || containers[0].Ports[0].PrivatePort != 8989 {
		t.Fatalf("unexpected containers: %+v", containers)
	}
	if len(fe.requests) != 1 || fe.requests[0].Method != "GET" || fe.requests[0].Path != "/containers/json" {
		t.Fatalf("unexpected request: %+v", fe.requests)
	}
	// listContainers({all: true}) → ?all=true; no filters upstream
	// (request-handler/src/docker.ts:386).
	if fe.requests[0].Query != "all=true" {
		t.Fatalf("query = %q, want all=true", fe.requests[0].Query)
	}

	// all=false omits the parameter entirely (the Engine default).
	if _, err := reg.Containers(context.Background(), fe.endpoint().ID, false); err != nil {
		t.Fatalf("Containers(all=false): %v", err)
	}
	if fe.requests[1].Query != "" {
		t.Fatalf("query = %q, want empty", fe.requests[1].Query)
	}
}

func TestInspectAndActions(t *testing.T) {
	fe := newFakeEngine(t)
	reg := mustRegistry(t, fe)
	epID := fe.endpoint().ID
	ctx := context.Background()

	if err := reg.ContainerInspect(ctx, epID, "abc"); err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}

	if err := reg.ContainerAction(ctx, epID, "abc", docker.ActionStart); err != nil {
		t.Fatalf("ContainerAction(start): %v", err)
	}
	if err := reg.ContainerAction(ctx, epID, "abc", docker.ActionStop); err != nil {
		t.Fatalf("ContainerAction(stop): %v", err)
	}
	if err := reg.ContainerAction(ctx, epID, "abc", docker.ActionRestart); err != nil {
		t.Fatalf("ContainerAction(restart): %v", err)
	}
	if err := reg.ContainerAction(ctx, epID, "abc", docker.ActionRemove); err != nil {
		t.Fatalf("ContainerAction(remove): %v", err)
	}

	want := []struct{ method, path, query string }{
		{"GET", "/containers/abc/json", ""},
		{"POST", "/containers/abc/start", ""}, // dockerode start() with no options
		{"POST", "/containers/abc/stop", ""},  // dockerode stop() with no options: no ?t=
		{"POST", "/containers/abc/restart", ""},
		{"DELETE", "/containers/abc", ""}, // dockerode remove() with no options: no ?v=
	}
	for i, w := range want {
		got := fe.requests[i]
		if got.Method != w.method || got.Path != w.path || got.Query != w.query {
			t.Fatalf("request %d = %+v, want %v", i, got, w)
		}
	}
}

func TestActionErrorMapping(t *testing.T) {
	fe := newFakeEngine(t)
	fe.actionStatus = 409
	fe.actionBody = `{"message":"container abc is already started"}`
	reg := mustRegistry(t, fe)

	err := reg.ContainerAction(context.Background(), fe.endpoint().ID, "abc", docker.ActionStart)
	if err == nil || !errors.Is(err, docker.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "container abc is already started") {
		t.Fatalf("error should carry the engine message, got %q", err.Error())
	}

	fe.actionStatus = 404
	err = reg.ContainerAction(context.Background(), fe.endpoint().ID, "abc", docker.ActionStop)
	if err == nil || !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLogsQueryAndDecode(t *testing.T) {
	fe := newFakeEngine(t)
	// Multiplexed frames interleaving stdout and stderr.
	fe.logsBody = concatBytes(
		encodeDockerLogFrame(1, "hello "),
		encodeDockerLogFrame(2, "world\n"),
		encodeDockerLogFrame(1, "second line\n"),
	)
	reg := mustRegistry(t, fe)

	raw, err := reg.ContainerLogs(context.Background(), fe.endpoint().ID, "abc", false, 200, time.Time{})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	body := readAll(t, raw)
	// The follow=false query carries the upstream option set: tail, stdout,
	// stderr, follow (request-handler/src/docker.ts:260-265).
	if fe.requests[0].Query != "follow=false&stderr=true&stdout=true&tail=200" {
		t.Fatalf("logs query = %q", fe.requests[0].Query)
	}
	if got := docker.DecodeLogs(body); got != "hello world\nsecond line\n" {
		t.Fatalf("DecodeLogs = %q", got)
	}
}

func TestLogsFollowQuery(t *testing.T) {
	fe := newFakeEngine(t)
	reg := mustRegistry(t, fe)

	raw, err := reg.ContainerLogs(context.Background(), fe.endpoint().ID, "abc", true, 0, time.Time{})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	readAll(t, raw)
	// The follow stream of streamContainerLogsAsync: tail is omitted when
	// zero, follow=true.
	if fe.requests[0].Query != "follow=true&stderr=true&stdout=true" {
		t.Fatalf("logs query = %q", fe.requests[0].Query)
	}
}

func TestStatsOneShotQuery(t *testing.T) {
	fe := newFakeEngine(t)
	var cache uint64 = 512
	fe.stats = docker.ContainerStats{}
	fe.stats.CPUStats.OnlineCPUs = 4
	fe.stats.CPUStats.CPUUsage.TotalUsage = 2000
	fe.stats.CPUStats.SystemCPUUsage = 10000
	fe.stats.MemoryStats.Usage = 1024
	fe.stats.MemoryStats.Stats.Cache = &cache
	reg := mustRegistry(t, fe)

	stats, err := reg.ContainerStats(context.Background(), fe.endpoint().ID, "abc")
	if err != nil {
		t.Fatalf("ContainerStats: %v", err)
	}
	// one-shot stream=false (request-handler/src/docker.ts:455-458).
	if len(fe.requests) != 1 || fe.requests[0].Query != "one-shot=true&stream=false" {
		t.Fatalf("stats request = %+v", fe.requests)
	}
	if got := docker.CalculateCPUUsage(stats, nil); got != 20 {
		t.Fatalf("cpu usage = %v, want 20", got)
	}
	if got := docker.CalculateMemoryUsage(stats); got != 512 {
		t.Fatalf("memory usage = %v, want 512", got)
	}
}

func TestUnknownEndpointAndUnreachable(t *testing.T) {
	reg := docker.NewRegistry([]docker.Endpoint{}, nil)
	if _, err := reg.ClientFor("missing"); !errors.Is(err, docker.ErrUnknownEndpoint) {
		t.Fatalf("want ErrUnknownEndpoint, got %v", err)
	}

	// A tcp endpoint whose engine is down maps to ErrUnreachable.
	down := docker.LegacyTCPEndpoint("127.0.0.1", closedPort(t))
	reg2 := docker.NewRegistry([]docker.Endpoint{down}, nil)
	_, err := reg2.Containers(context.Background(), down.ID, true)
	if !errors.Is(err, docker.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	// The same mapping for logs and stats.
	if _, err := reg2.ContainerLogs(context.Background(), down.ID, "abc", false, 10, time.Time{}); !errors.Is(err, docker.ErrUnreachable) {
		t.Fatalf("logs: want ErrUnreachable, got %v", err)
	}
	if _, err := reg2.ContainerStats(context.Background(), down.ID, "abc"); !errors.Is(err, docker.ErrUnreachable) {
		t.Fatalf("stats: want ErrUnreachable, got %v", err)
	}
}

func TestTLSEndpointNotImplemented(t *testing.T) {
	ep := docker.Endpoint{
		ID:   "tls:host:2376",
		Name: "tls",
		Kind: "docker",
		Transport: docker.Transport{
			Kind: docker.TransportTLS,
			Host: "host",
			Port: 2376,
			Path: "/nonexistent/ca.pem",
		},
		Capabilities: docker.AllCapabilities(),
		Source:       "environment",
		Scope:        "admin",
	}
	// A well-formed TLS endpoint builds no client: NOT_IMPLEMENTED in this
	// port (upstream supports it with dockerode, singleton.ts:143-151).
	if _, err := docker.NewClient(ep); !errors.Is(err, docker.ErrTLSNotImplemented) {
		t.Fatalf("want ErrTLSNotImplemented, got %v", err)
	}

	// The missing CA file fails CONSTRUCTION (upstream readFileSync,
	// singleton.ts:147-150) → the registry keeps it as a failure and lists
	// the endpoint unavailable.
	reg := docker.NewRegistry(nil, []docker.EndpointFailure{{Endpoint: ep, Err: docker.ErrTLSNotImplemented}})
	failures := reg.Failures()
	if len(failures) != 1 || failures[0].Endpoint.ID != ep.ID {
		t.Fatalf("failures = %+v", failures)
	}
	if _, err := reg.ClientFor(ep.ID); !errors.Is(err, docker.ErrUnknownEndpoint) {
		t.Fatalf("failed endpoints have no client, got %v", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	// The client applies dockerWidgetEndpointTimeoutMs (5000 ms) as the
	// per-request timeout; a shortened timeout against a slow engine maps
	// to ErrUnreachable (upstream: the withDockerTimeoutAsync rejection
	// marks the endpoint unavailable, docker.ts:383-394).
	slow := newFakeEngine(t)
	slow.hang = 300 * time.Millisecond

	reg := mustRegistry(t, slow)
	client, err := reg.ClientFor(slow.endpoint().ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	client.Timeout = 50 * time.Millisecond
	_, err = reg.Containers(context.Background(), slow.endpoint().ID, true)
	if !errors.Is(err, docker.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

// helpers --------------------------------------------------------------------

func concatBytes(parts ...[]byte) []byte {
	out := []byte{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func readAll(t *testing.T, rc io.Reader) []byte {
	t.Helper()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return body
}

// closedPort returns a TCP port nothing listens on (bind and release).
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
