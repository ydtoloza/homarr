// Package docker is the Go port of upstream's Docker domain plumbing:
// packages/docker (the dockerode wrapper: endpoint descriptors, the
// DockerSingleton and the docker labels) plus the Engine API access of
// packages/request-handler/src/docker.ts.
//
// Upstream connects with dockerode over the local socket, TCP or TLS
// (apps/nextjs/next.config.ts marks dockerode as serverExternalPackages). The
// Go port speaks the Docker Engine API directly with net/http and a custom
// DialContext (unix socket / Windows named pipe / TCP), so no dockerode
// equivalent dependency is needed.
//
// Endpoints are NOT database rows: upstream has no docker table in
// packages/db/schema (verified: no "docker" match in schema/sqlite.ts) —
// endpoints come from environment variables, parsed in
// DockerSingleton.createInstances (packages/docker/src/singleton.ts:52-100):
//
//	DOCKER_ENDPOINTS  JSON descriptors (names, TLS, podman, capabilities)
//	DOCKER_SOCKET_PATHS  comma separated unix socket paths (legacy)
//	DOCKER_HOSTNAMES + DOCKER_PORTS  legacy plain TCP endpoints
//	DOCKER_HOST  (fork extension) docker-style URL: unix://, npipe://, tcp://
//	(fallback)   the platform default socket / named pipe
//
// Every procedure of the upstream docker tRPC router requires the "admin"
// permission and is additionally gated by the ENABLE_DOCKER env
// (packages/api/src/middlewares/docker.ts, packages/docker/src/env.ts —
// createBooleanSchema(true), enabled by default).
package docker

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// Capabilities of a docker endpoint (packages/docker/src/endpoint-descriptor.ts:3):
// inventory (list containers), logs, lifecycle (start/stop/restart) and
// remove. Every endpoint must keep inventory.
const (
	CapabilityInventory Capability = "inventory"
	CapabilityLogs      Capability = "logs"
	CapabilityLifecycle Capability = "lifecycle"
	CapabilityRemove    Capability = "remove"
)

// Capability is one DockerEndpointCapability
// (packages/docker/src/endpoint-descriptor.ts:4).
type Capability string

// allCapabilities is the default capability set when DOCKER_ENDPOINTS omits
// the field (parseCapabilities, endpoint-descriptor.ts:93-95).
var allCapabilities = []Capability{CapabilityInventory, CapabilityLogs, CapabilityLifecycle, CapabilityRemove}

// AllCapabilities returns a copy of the full capability set.
func AllCapabilities() []Capability {
	return append([]Capability(nil), allCapabilities...)
}

// TransportKind is the endpoint transport type: "socket" (unix socket or
// Windows named pipe), "tcp" (plaintext) or "tls".
type TransportKind string

// Transport types of upstream DockerEndpointTransport
// (endpoint-descriptor.ts:6-16).
const (
	TransportSocket TransportKind = "socket"
	TransportTCP    TransportKind = "tcp"
	// TransportTLS is parsed and listed (so getEndpoints reports the
	// endpoint) but NOT implemented in this port: ClientFor returns
	// ErrTLSNotImplemented. Upstream supports it with dockerode
	// (createDockerOptions, singleton.ts:139-152).
	TransportTLS TransportKind = "tls"
)

// Transport is one DockerEndpointTransport variant flattened: socket keeps
// Path, tcp/tls keep Host+Port.
type Transport struct {
	Kind TransportKind
	// Path is the socket path: a unix socket path or, on Windows, a named
	// pipe such as //./pipe/docker_engine.
	Path string
	Host string
	Port int
}

// Endpoint is one DockerEndpointDescriptor
// (endpoint-descriptor.ts:18-26): id, name, kind, transport, capabilities,
// scope ("admin") and source ("environment"|"legacy"|"default").
type Endpoint struct {
	ID           string
	Name         string
	Kind         string // "docker" or "podman"
	Transport    Transport
	Capabilities []Capability
	Source       string
	Scope        string
}

// Host is getDockerEndpointHost (endpoint-descriptor.ts:131-134): the socket
// path, or "host:port" for tcp/tls.
func (e Endpoint) Host() string {
	if e.Transport.Kind == TransportSocket {
		return e.Transport.Path
	}
	return fmt.Sprintf("%s:%d", e.Transport.Host, e.Transport.Port)
}

// HasCapability reports whether the endpoint lists the capability.
func (e Endpoint) HasCapability(c Capability) bool {
	for _, existing := range e.Capabilities {
		if existing == c {
			return true
		}
	}
	return false
}

// EndpointFailure is one endpoint that failed CONSTRUCTION (upstream
// DockerEndpointInitializationFailure, singleton.ts:24-27): today only TLS
// endpoints with unreadable CA/cert/key files fail here (readFileSync at
// singleton.ts:147-150). Failed endpoints are still listed by getEndpoints
// with status "unavailable".
type EndpointFailure struct {
	Endpoint Endpoint
	Err      error
}

// defaultSocketPath is the upstream default when no endpoint env is set
// (singleton.ts:89-97: /var/run/docker.sock with id "socket:default").
const defaultSocketPath = "/var/run/docker.sock"

// defaultWindowsPipe is the fork's Windows default. Upstream hardcodes the
// unix path even on Windows (where it can never work); the port uses the
// Docker Desktop named pipe instead, keeping the same "socket:default" id.
const defaultWindowsPipe = "//./pipe/docker_engine"

// DefaultEndpoint returns the fallback endpoint for the given GOOS
// (singleton.ts:89-97; the Windows pipe is a fork extension).
func DefaultEndpoint(goos string) Endpoint {
	path := defaultSocketPath
	if goos == "windows" {
		path = defaultWindowsPipe
	}
	return Endpoint{
		ID:           "socket:default",
		Name:         "Local Docker",
		Kind:         "docker",
		Transport:    Transport{Kind: TransportSocket, Path: path},
		Capabilities: append([]Capability(nil), allCapabilities...),
		Source:       "default",
		Scope:        "admin",
	}
}

// LegacySocketEndpoint mirrors createLegacySocketDescriptor
// (endpoint-descriptor.ts:136-144).
func LegacySocketEndpoint(path string) Endpoint {
	kind := "docker"
	if strings.Contains(strings.ToLower(path), "podman") {
		kind = "podman"
	}
	return Endpoint{
		ID:           "socket:" + path,
		Name:         path,
		Kind:         kind,
		Transport:    Transport{Kind: TransportSocket, Path: path},
		Capabilities: append([]Capability(nil), allCapabilities...),
		Source:       "legacy",
		Scope:        "admin",
	}
}

// LegacyTCPEndpoint mirrors createLegacyTcpDescriptor
// (endpoint-descriptor.ts:146-154).
func LegacyTCPEndpoint(host string, port int) Endpoint {
	return Endpoint{
		ID:           fmt.Sprintf("tcp:%s:%d", host, port),
		Name:         fmt.Sprintf("%s:%d", host, port),
		Kind:         "docker",
		Transport:    Transport{Kind: TransportTCP, Host: host, Port: port},
		Capabilities: append([]Capability(nil), allCapabilities...),
		Source:       "legacy",
		Scope:        "admin",
	}
}

// EndpointsFromEnv builds the endpoint list from the environment the same way
// DockerSingleton.createInstances does (singleton.ts:52-100):
//
//  1. DOCKER_ENDPOINTS: JSON descriptors (parseDockerEndpointDescriptors).
//  2. legacy: DOCKER_SOCKET_PATHS (comma separated) and
//     DOCKER_HOSTNAMES+DOCKER_PORTS (comma separated, lengths must match).
//  3. DOCKER_HOST (fork extension): docker-CLI-style URL, unix://, npipe://
//     or tcp:// (a bare path is treated as a unix socket / pipe).
//  4. the platform default socket ("Local Docker").
//
// getenv is injected for tests; pass os.Getenv at the call sites.
func EndpointsFromEnv(getenv func(string) string) ([]Endpoint, []EndpointFailure, error) {
	if raw := getenv("DOCKER_ENDPOINTS"); raw != "" {
		endpoints, err := ParseEndpointDescriptors(raw)
		if err != nil {
			return nil, nil, err
		}
		// Split construction failures (TLS files missing) from usable
		// endpoints, mirroring DockerSingleton.createInstanceSafely
		// (singleton.ts:35-50).
		var ok []Endpoint
		var failures []EndpointFailure
		for _, ep := range endpoints {
			if err := checkTLSFiles(ep); err != nil {
				failures = append(failures, EndpointFailure{Endpoint: ep, Err: err})
				continue
			}
			ok = append(ok, ep)
		}
		return ok, failures, nil
	}

	var endpoints []Endpoint
	if raw := getenv("DOCKER_SOCKET_PATHS"); raw != "" {
		for _, path := range strings.Split(raw, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			endpoints = append(endpoints, LegacySocketEndpoint(path))
		}
	}
	if hosts, ports := getenv("DOCKER_HOSTNAMES"), getenv("DOCKER_PORTS"); hosts != "" && ports != "" {
		hostList := strings.Split(hosts, ",")
		portList := strings.Split(ports, ",")
		if len(hostList) != len(portList) {
			// "The number of hosts and ports must match" (singleton.ts:77).
			return nil, nil, fmt.Errorf("docker: the number of hosts and ports must match")
		}
		for i, host := range hostList {
			host = strings.TrimSpace(host)
			port := strings.TrimSpace(portList[i])
			n, err := parsePort(port)
			if err != nil {
				return nil, nil, fmt.Errorf("docker: invalid DOCKER_PORTS entry %q: %w", port, err)
			}
			endpoints = append(endpoints, LegacyTCPEndpoint(host, n))
		}
	}
	// Legacy sources with only one half configured are ignored upstream
	// (both variables must be set, singleton.ts:72); fall through to the
	// default in that case.
	if len(endpoints) == 0 {
		if raw := getenv("DOCKER_HOST"); raw != "" {
			ep, err := EndpointFromHostURL(raw)
			if err != nil {
				return nil, nil, err
			}
			endpoints = append(endpoints, ep)
		}
	}
	if len(endpoints) == 0 {
		endpoints = append(endpoints, DefaultEndpoint(runtime.GOOS))
	}
	return endpoints, nil, nil
}

// EndpointFromHostURL parses a DOCKER_HOST style URL (fork extension —
// upstream only knows the socket/hostname env vars): unix:///path,
// npipe:////./pipe/docker_engine, tcp://host:port, or a bare path
// (/var/run/docker.sock, \\.\pipe\docker_engine).
func EndpointFromHostURL(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("docker: empty DOCKER_HOST")
	}
	withoutScheme := raw
	switch {
	case strings.HasPrefix(raw, "unix://"):
		withoutScheme = "unix://" + strings.TrimPrefix(raw, "unix://")
		withoutScheme = strings.TrimPrefix(withoutScheme, "unix://")
		return LegacySocketEndpoint(withoutScheme), nil
	case strings.HasPrefix(raw, "npipe://"):
		// docker-CLI convention: npipe:////./pipe/docker_engine (the value
		// after the scheme is the pipe path itself).
		path := strings.TrimPrefix(raw, "npipe://")
		if !strings.HasPrefix(path, "//./pipe/") && !strings.HasPrefix(path, `\\.\pipe\`) {
			return Endpoint{}, fmt.Errorf("docker: npipe:// DOCKER_HOST must point at //./pipe/<name>: %q", raw)
		}
		return LegacySocketEndpoint(normalizePipePath(path)), nil
	case strings.HasPrefix(raw, "tcp://"):
		host, port, err := splitHostPort(strings.TrimPrefix(raw, "tcp://"))
		if err != nil {
			return Endpoint{}, fmt.Errorf("docker: invalid tcp:// DOCKER_HOST %q: %w", raw, err)
		}
		return LegacyTCPEndpoint(host, port), nil
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		return Endpoint{}, fmt.Errorf("docker: DOCKER_HOST %q must use unix://, npipe:// or tcp://", raw)
	default:
		// Bare path: unix socket on unix, named pipe on Windows.
		return LegacySocketEndpoint(normalizePipePath(raw)), nil
	}
}

// normalizePipePath converts \\.\pipe\name to the //./pipe/name form used by
// Go's os.OpenFile-based pipe dialing.
func normalizePipePath(path string) string {
	if strings.HasPrefix(path, `\\.\pipe\`) {
		return "//./pipe/" + strings.TrimPrefix(path, `\\.\pipe\`)
	}
	return path
}

func splitHostPort(s string) (string, int, error) {
	idx := strings.LastIndex(s, ":")
	if idx == -1 {
		return "", 0, fmt.Errorf("missing port")
	}
	host, port := s[:idx], s[idx+1:]
	n, err := parsePort(port)
	if err != nil {
		return "", 0, err
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty host")
	}
	return host, n, nil
}

func parsePort(raw string) (int, error) {
	var n int
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("must be numeric")
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("must be an integer from 1 to 65535")
		}
	}
	if n < 1 {
		return 0, fmt.Errorf("must be an integer from 1 to 65535")
	}
	return n, nil
}

// endpointJSON is the DOCKER_ENDPOINTS entry schema
// (parseDescriptor, endpoint-descriptor.ts:47-58).
type endpointJSON struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Transport    transportJSON `json:"transport"`
	Capabilities []Capability  `json:"capabilities"`
}

type transportJSON struct {
	Type          string `json:"type"`
	Path          string `json:"path"`
	Host          string `json:"host"`
	Port          *int   `json:"port"`
	AllowInsecure *bool  `json:"allowInsecure"`
	CAPath        string `json:"caPath"`
	CertPath      string `json:"certPath"`
	KeyPath       string `json:"keyPath"`
}

// ParseEndpointDescriptors ports parseDockerEndpointDescriptors
// (endpoint-descriptor.ts:30-45): a non-empty JSON array with unique ids.
func ParseEndpointDescriptors(raw string) ([]Endpoint, error) {
	var input []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return nil, fmt.Errorf("docker: DOCKER_ENDPOINTS must be valid JSON: %w", err)
	}
	if len(input) == 0 {
		return nil, fmt.Errorf("docker: DOCKER_ENDPOINTS must be a non-empty JSON array")
	}
	seen := map[string]bool{}
	endpoints := make([]Endpoint, 0, len(input))
	for i, entry := range input {
		ep, err := parseDescriptorJSON(entry, i)
		if err != nil {
			return nil, err
		}
		if seen[ep.ID] {
			return nil, fmt.Errorf("docker: DOCKER_ENDPOINTS ids must be unique")
		}
		seen[ep.ID] = true
		endpoints = append(endpoints, ep)
	}
	return endpoints, nil
}

func parseDescriptorJSON(entry json.RawMessage, index int) (Endpoint, error) {
	where := fmt.Sprintf("DOCKER_ENDPOINTS[%d]", index)
	var parsed endpointJSON
	if err := json.Unmarshal(entry, &parsed); err != nil {
		return Endpoint{}, fmt.Errorf("docker: %s must be an object", where)
	}
	id := strings.TrimSpace(parsed.ID)
	if id == "" {
		return Endpoint{}, fmt.Errorf("docker: %s.id must be a non-empty string", where)
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		return Endpoint{}, fmt.Errorf("docker: %s.name must be a non-empty string", where)
	}
	kind := parsed.Kind
	switch kind {
	case "", "docker", "podman":
		if kind == "" {
			kind = "docker"
		}
	default:
		return Endpoint{}, fmt.Errorf("docker: %s.kind must be docker or podman", where)
	}
	transport, err := parseTransportJSON(parsed.Transport, where)
	if err != nil {
		return Endpoint{}, err
	}
	capabilities, err := parseCapabilitiesJSON(parsed.Capabilities, where)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{
		ID:           id,
		Name:         name,
		Kind:         kind,
		Transport:    transport,
		Capabilities: capabilities,
		Source:       "environment",
		Scope:        "admin",
	}, nil
}

func parseTransportJSON(t transportJSON, where string) (Transport, error) {
	switch t.Type {
	case "socket":
		path := strings.TrimSpace(t.Path)
		if path == "" {
			return Transport{}, fmt.Errorf("docker: %s.transport.path must be a non-empty string", where)
		}
		// getAbsolutePath requires a leading "/" (endpoint-descriptor.ts:115-119);
		// Windows named pipes are a fork extension and start with //./pipe/.
		if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//./pipe/") {
			return Transport{}, fmt.Errorf("docker: %s.transport.path must be an absolute path", where)
		}
		return Transport{Kind: TransportSocket, Path: normalizePipePath(path)}, nil
	case "tcp":
		// Plaintext TCP requires allowInsecure: true
		// (endpoint-descriptor.ts:66-74).
		if t.AllowInsecure == nil || !*t.AllowInsecure {
			return Transport{}, fmt.Errorf("docker: %s plaintext TCP requires allowInsecure: true", where)
		}
		if strings.TrimSpace(t.Host) == "" {
			return Transport{}, fmt.Errorf("docker: %s.transport.host must be a non-empty string", where)
		}
		if t.Port == nil || *t.Port < 1 || *t.Port > 65535 {
			return Transport{}, fmt.Errorf("docker: %s.transport.port must be an integer from 1 to 65535", where)
		}
		return Transport{Kind: TransportTCP, Host: strings.TrimSpace(t.Host), Port: *t.Port}, nil
	case "tls":
		// Parsed for metadata; the client refuses to connect until the TLS
		// port is implemented (see TransportTLS).
		if strings.TrimSpace(t.Host) == "" {
			return Transport{}, fmt.Errorf("docker: %s.transport.host must be a non-empty string", where)
		}
		if t.Port == nil || *t.Port < 1 || *t.Port > 65535 {
			return Transport{}, fmt.Errorf("docker: %s.transport.port must be an integer from 1 to 65535", where)
		}
		ca := strings.TrimSpace(t.CAPath)
		if ca == "" {
			return Transport{}, fmt.Errorf("docker: %s.transport.caPath must be an absolute path", where)
		}
		cert, key := strings.TrimSpace(t.CertPath), strings.TrimSpace(t.KeyPath)
		if (cert == "") != (key == "") {
			return Transport{}, fmt.Errorf("docker: %s TLS certPath and keyPath must be provided together", where)
		}
		return Transport{Kind: TransportTLS, Host: strings.TrimSpace(t.Host), Port: *t.Port, Path: ca}, nil
	default:
		return Transport{}, fmt.Errorf("docker: %s.transport.type must be socket, tcp, or tls", where)
	}
}

func parseCapabilitiesJSON(raw []Capability, where string) ([]Capability, error) {
	if len(raw) == 0 {
		// undefined → all capabilities (endpoint-descriptor.ts:93-95).
		return append([]Capability(nil), allCapabilities...), nil
	}
	seen := map[Capability]bool{}
	capabilities := make([]Capability, 0, len(raw))
	hasInventory := false
	for _, c := range raw {
		switch c {
		case CapabilityInventory, CapabilityLogs, CapabilityLifecycle, CapabilityRemove:
		default:
			return nil, fmt.Errorf("docker: %s.capabilities contains an unsupported capability", where)
		}
		if seen[c] {
			continue // [...new Set(value)] dedupe
		}
		seen[c] = true
		if c == CapabilityInventory {
			hasInventory = true
		}
		capabilities = append(capabilities, c)
	}
	if !hasInventory {
		return nil, fmt.Errorf("docker: %s.capabilities must include inventory", where)
	}
	return capabilities, nil
}
