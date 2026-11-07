package docker_test

import (
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Endpoint parsing tests — the port of
// packages/docker/src/test/endpoint-descriptor.spec.ts plus the legacy env
// fallbacks of DockerSingleton.createInstances (singleton.ts:52-100).

func getenvOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestParseEndpointDescriptors(t *testing.T) {
	raw := `[
		{"id":"nas","name":"NAS","transport":{"type":"tcp","host":"192.168.1.5","port":2375,"allowInsecure":true}},
		{"id":"local","name":"Local","kind":"podman","transport":{"type":"socket","path":"/run/user/1000/podman/podman.sock"},"capabilities":["inventory","logs"]},
		{"id":"secure","name":"Secure","transport":{"type":"tls","host":"example.com","port":2376,"caPath":"/certs/ca.pem"}}
	]`
	endpoints, failures, err := docker.EndpointsFromEnv(getenvOf(map[string]string{"DOCKER_ENDPOINTS": raw}))
	if err != nil {
		t.Fatalf("EndpointsFromEnv: %v", err)
	}
	// The tls fixture points at a nonexistent CA file: construction fails
	// exactly like upstream readFileSync (singleton.ts:147-150).
	if len(failures) != 1 || failures[0].Endpoint.ID != "secure" {
		t.Fatalf("failures = %+v", failures)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v", endpoints)
	}

	tcp := endpoints[0]
	if tcp.ID != "nas" || tcp.Kind != "docker" || tcp.Source != "environment" || tcp.Scope != "admin" {
		t.Fatalf("tcp endpoint = %+v", tcp)
	}
	if tcp.Transport.Kind != docker.TransportTCP || tcp.Host() != "192.168.1.5:2375" {
		t.Fatalf("tcp transport = %+v", tcp.Transport)
	}
	if len(tcp.Capabilities) != 4 { // default: all capabilities
		t.Fatalf("capabilities = %v", tcp.Capabilities)
	}

	socket := endpoints[1]
	if socket.Kind != "podman" || !socket.HasCapability(docker.CapabilityLogs) || socket.HasCapability(docker.CapabilityRemove) {
		t.Fatalf("socket endpoint = %+v", socket)
	}
}

func TestParseEndpointDescriptorErrors(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"not json", `{`, "DOCKER_ENDPOINTS must be valid JSON"},
		{"empty array", `[]`, "non-empty JSON array"},
		{"duplicate ids", `[{"id":"a","name":"A","transport":{"type":"socket","path":"/s"}},{"id":"a","name":"A2","transport":{"type":"socket","path":"/s"}}]`, "ids must be unique"},
		{"bad kind", `[{"id":"a","name":"A","kind":"k8s","transport":{"type":"socket","path":"/s"}}]`, "kind must be docker or podman"},
		{"relative socket path", `[{"id":"a","name":"A","transport":{"type":"socket","path":"rel"}}]`, "absolute path"},
		{"insecure tcp", `[{"id":"a","name":"A","transport":{"type":"tcp","host":"h","port":2375}}]`, "allowInsecure: true"},
		{"bad port", `[{"id":"a","name":"A","transport":{"type":"tcp","host":"h","port":0,"allowInsecure":true}}]`, "1 to 65535"},
		{"tls without ca", `[{"id":"a","name":"A","transport":{"type":"tls","host":"h","port":2376}}]`, "caPath"},
		{"tls cert without key", `[{"id":"a","name":"A","transport":{"type":"tls","host":"h","port":2376,"caPath":"/c","certPath":"/cert"}}]`, "certPath and keyPath"},
		{"bad capability", `[{"id":"a","name":"A","transport":{"type":"socket","path":"/s"},"capabilities":["inventory","banana"]}]`, "unsupported capability"},
		{"inventory required", `[{"id":"a","name":"A","transport":{"type":"socket","path":"/s"},"capabilities":["logs"]}]`, "must include inventory"},
		{"empty id", `[{"id":"  ","name":"A","transport":{"type":"socket","path":"/s"}}]`, "non-empty string"},
		{"bad transport type", `[{"id":"a","name":"A","transport":{"type":"carrier-pigeon"}}]`, "socket, tcp, or tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := docker.EndpointsFromEnv(getenvOf(map[string]string{"DOCKER_ENDPOINTS": tc.raw}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLegacySocketPathsEnv(t *testing.T) {
	endpoints, failures, err := docker.EndpointsFromEnv(getenvOf(map[string]string{
		"DOCKER_SOCKET_PATHS": "/var/run/docker.sock,/run/user/1000/podman/podman.sock",
	}))
	if err != nil || len(failures) != 0 {
		t.Fatalf("err=%v failures=%+v", err, failures)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	if endpoints[0].ID != "socket:/var/run/docker.sock" || endpoints[0].Source != "legacy" {
		t.Fatalf("endpoint = %+v", endpoints[0])
	}
	// Podman paths make the kind (createLegacySocketDescriptor,
	// endpoint-descriptor.ts:139).
	if endpoints[1].Kind != "podman" {
		t.Fatalf("podman kind = %+v", endpoints[1])
	}
}

func TestLegacyHostnamesPortsEnv(t *testing.T) {
	endpoints, _, err := docker.EndpointsFromEnv(getenvOf(map[string]string{
		"DOCKER_HOSTNAMES": "localhost,host.docker.internal",
		"DOCKER_PORTS":     "2375,2376",
	}))
	if err != nil {
		t.Fatalf("EndpointsFromEnv: %v", err)
	}
	if len(endpoints) != 2 || endpoints[0].ID != "tcp:localhost:2375" || endpoints[1].ID != "tcp:host.docker.internal:2376" {
		t.Fatalf("endpoints = %+v", endpoints)
	}

	// Mismatched lengths are rejected ("The number of hosts and ports must
	// match", singleton.ts:77).
	_, _, err = docker.EndpointsFromEnv(getenvOf(map[string]string{
		"DOCKER_HOSTNAMES": "localhost,other",
		"DOCKER_PORTS":     "2375",
	}))
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultEndpoint(t *testing.T) {
	endpoints, _, err := docker.EndpointsFromEnv(getenvOf(map[string]string{}))
	if err != nil {
		t.Fatalf("EndpointsFromEnv: %v", err)
	}
	if len(endpoints) != 1 || endpoints[0].ID != "socket:default" || endpoints[0].Source != "default" {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	// Unix keeps the upstream default path (singleton.ts:91).
	if got := docker.DefaultEndpoint("linux"); got.Transport.Path != "/var/run/docker.sock" {
		t.Fatalf("linux default = %+v", got)
	}
	// Windows falls back to the Docker Desktop named pipe (fork extension).
	if got := docker.DefaultEndpoint("windows"); got.Transport.Path != "//./pipe/docker_engine" {
		t.Fatalf("windows default = %+v", got)
	}
}

func TestDockerHostURL(t *testing.T) {
	cases := []struct {
		raw     string
		wantID  string
		wantErr bool
	}{
		{"unix:///var/run/docker.sock", "socket:/var/run/docker.sock", false},
		{"/var/run/docker.sock", "socket:/var/run/docker.sock", false},
		{"npipe:////./pipe/docker_engine", "socket://./pipe/docker_engine", false},
		{`\\.\pipe\docker_engine`, "socket://./pipe/docker_engine", false},
		{"tcp://192.168.1.5:2375", "tcp:192.168.1.5:2375", false},
		{"tcp://no-port", "", true},
		{"http://host:2375", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		ep, err := docker.EndpointFromHostURL(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q: want error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if ep.ID != tc.wantID {
			t.Fatalf("%q: id = %q, want %q", tc.raw, ep.ID, tc.wantID)
		}
	}
}

func TestRegistryClientCachingAndReset(t *testing.T) {
	env := map[string]string{"DOCKER_SOCKET_PATHS": "/var/run/docker.sock"}
	reg, err := docker.RegistryFromEnv(getenvOf(env))
	if err != nil {
		t.Fatalf("RegistryFromEnv: %v", err)
	}
	first, err := reg.ClientFor("socket:/var/run/docker.sock")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	second, err := reg.ClientFor("socket:/var/run/docker.sock")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if first != second {
		t.Fatalf("clients are not cached")
	}

	// Reset re-reads the environment (DockerSingleton.reset).
	env["DOCKER_SOCKET_PATHS"] = "/run/podman.sock"
	if err := reg.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, known := reg.Endpoint("socket:/var/run/docker.sock"); known {
		t.Fatalf("stale endpoint survived Reset")
	}
	endpoints := reg.Endpoints()
	if len(endpoints) != 1 || endpoints[0].ID != "socket:/run/podman.sock" {
		t.Fatalf("endpoints after Reset = %+v", endpoints)
	}

	// An invalid DOCKER_ENDPOINTS value fails Reset but keeps the old list.
	env["DOCKER_ENDPOINTS"] = "{"
	env["DOCKER_SOCKET_PATHS"] = ""
	if err := reg.Reset(); err == nil {
		t.Fatalf("Reset should fail on invalid env")
	}
	if len(reg.Endpoints()) != 1 {
		t.Fatalf("endpoints should survive a failed Reset")
	}
}
