package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Engine API client (the dockerode replacement).
//
// Request paths are UNVERSIONED (/containers/json, not /v1.xx/containers/json):
// the Engine serves the unversioned path at its latest supported API and every
// route this client uses (containers, start/stop/restart, logs, stats, inspect)
// is compatible with any engine that ships the socket. Upstream dockerode
// prefixes its default version path; the unversioned form avoids pinning one.
//
// Query serialization: upstream serializes options with node querystring, so
// booleans become "true" (dockerode listContainers({all: true}) → all=true).
// The port emits the same strings.

// Action is one container lifecycle operation, the DockerContainerAction of
// the docker router (docker-router.ts:259): start/stop/restart map to their
// POST paths, remove to DELETE.
type Action string

// Container actions of performDockerContainerActionsAsync
// (docker-router.ts:261-295). remove is DELETE /containers/{id}; the Engine
// defaults apply (no ?v= / ?force=), matching dockerode's bare
// container.remove() with no options.
const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	ActionRemove  Action = "remove"
)

// Engine request timeout per call: dockerWidgetEndpointTimeoutMs
// (packages/request-handler/src/docker.ts:18, 5000 ms for both container
// listing and stats inside getContainersWithStatsAsync).
const DefaultTimeout = 5 * time.Second

// Errors surfaced by the client. The router maps them to the exact tRPC
// errors of upstream:
//   - inspect failures (ANY error, including unreachable) mean "container
//     unknown" for the router: findDockerContainerAsync resolves null on
//     inspect error (request-handler/src/docker.ts:241-252) → NOT_FOUND
//     "Container not found".
//   - action failures ride inside the per-target result objects, never thrown
//     (docker-router.ts:280-287).
var (
	// ErrNotFound is an Engine 404 (unknown container).
	ErrNotFound = errors.New("docker: not found")
	// ErrConflict is an Engine 409 (e.g. already started, cannot remove a
	// running container without force).
	ErrConflict = errors.New("docker: conflict")
	// ErrUnreachable wraps transport failures: missing socket, connection
	// refused, dial timeout.
	ErrUnreachable = errors.New("docker: endpoint unreachable")
	// ErrTLSNotImplemented marks the deferred TLS transport (TransportTLS).
	ErrTLSNotImplemented = errors.New("docker: TLS endpoints are NOT_IMPLEMENTED in homarr-go yet")
	// ErrFrameTooLarge is the stream demultiplexer limit: the upstream
	// processor destroys the stream when a frame exceeds the maximum
	// (request-handler/src/docker-log-decode.ts:90-95).
	ErrFrameTooLarge = errors.New("docker: log frame exceeds maximum size")
	// ErrUnknownEndpoint is returned by Registry.ClientFor.
	ErrUnknownEndpoint = errors.New("docker: unknown endpoint")
)

// RequestError is a non-mapped Engine response (status >= 400 other than
// 404/409). Message carries the Engine's JSON "message" when present.
type RequestError struct {
	Status     int
	StatusText string
	Message    string
}

func (e *RequestError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("docker: engine returned %s", e.StatusText)
}

// ContainerPort is one entry of /containers/json "Ports"
// (dockerode Port: IP, PrivatePort, PublicPort, Type). The router forwards
// the array verbatim, so the JSON tags are the wire contract.
type ContainerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// ContainerInfo is the subset of dockerode ContainerInfo used by
// getContainersWithStatsAsync (request-handler/src/docker.ts:383-508).
type ContainerInfo struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Ports  []ContainerPort   `json:"Ports"`
}

// Name is the container display name: Names[0] without the leading slash
// ("Unknown" fallback, request-handler/src/docker.ts:497).
func (c ContainerInfo) Name() string {
	if len(c.Names) == 0 {
		return "Unknown"
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// LogsOptions are the container.logs query options upstream passes
// (request-handler/src/docker.ts:260-265 and 282-287: tail, stdout, stderr,
// follow). Since/Until are accepted for completeness but upstream never sets
// them (0/empty = omitted).
type LogsOptions struct {
	Tail   int
	Follow bool
	Since  time.Time
}

// Client is one Docker Engine API connection for an endpoint. It is safe for
// concurrent use (one http.Client with a pooled transport).
type Client struct {
	endpoint Endpoint
	http     *http.Client
	// Timeout bounds each engine request unless the caller's context
	// deadline is shorter (DefaultTimeout, the upstream
	// dockerWidgetEndpointTimeoutMs).
	Timeout time.Duration
}

// NewClient builds a client for an endpoint. The transport resolves the
// endpoint transport: unix socket / Windows named pipe via a custom
// DialContext, TCP via the default dialer. TLS endpoints are rejected with
// ErrTLSNotImplemented (deferred; upstream supports them through dockerode,
// singleton.ts:143-151).
func NewClient(endpoint Endpoint) (*Client, error) {
	transport := &http.Transport{
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: DefaultTimeout,
	}
	switch endpoint.Transport.Kind {
	case TransportSocket:
		network, addr := dialParamsForSocket(endpoint.Transport.Path)
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialSocket(ctx, network, addr)
		}
	case TransportTCP:
		// Plain HTTP over TCP (allowInsecure: true upstream, protocol
		// "http" in createDockerOptions, singleton.ts:142). The request URL
		// uses a fixed authority; the dialer redirects to the endpoint
		// host:port.
		addr := net.JoinHostPort(endpoint.Transport.Host, strconv.Itoa(endpoint.Transport.Port))
		dialer := &net.Dialer{Timeout: DefaultTimeout}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}
	default:
		return nil, ErrTLSNotImplemented
	}
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Transport: transport},
		Timeout:  DefaultTimeout,
	}, nil
}

// Endpoint returns the descriptor the client was built for.
func (c *Client) Endpoint() Endpoint { return c.endpoint }

// dialParamsForSocket maps a socket path to (network, address): unix sockets
// use network "unix"; Windows named pipes (//./pipe/...) use network "pipe",
// dialed through os.OpenFile by dialSocket (no external winio dependency).
func dialParamsForSocket(path string) (network, addr string) {
	if strings.HasPrefix(path, "//./pipe/") {
		return "pipe", path
	}
	return "unix", path
}

// dialSocket dials a unix socket or a Windows named pipe. Named pipes are
// opened with os.OpenFile (CreateFile) and wrapped in a net.Conn without
// deadline support; http.Transport only exercises deadlines when the caller
// configures write/response timeouts, which the Docker client does not
// (ResponseHeaderTimeout uses a timer, not the conn deadline).
func dialSocket(ctx context.Context, network, addr string) (net.Conn, error) {
	if network == "pipe" {
		f, err := os.OpenFile(addr, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		return &pipeConn{file: f}, nil
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return conn, nil
}

// pipeConn wraps an opened named pipe as a net.Conn. Deadlines are accepted
// but not enforced (Go's os package exposes no SetHandleInformation deadline
// for pipes; the Docker Engine is a local, reliable transport).
type pipeConn struct {
	file *os.File
}

func (p *pipeConn) Read(b []byte) (int, error)         { return p.file.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error)        { return p.file.Write(b) }
func (p *pipeConn) Close() error                       { return p.file.Close() }
func (p *pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (p *pipeConn) SetDeadline(t time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "docker_engine" }

// doJSON performs one Engine request and decodes the JSON body into out (nil
// skips decoding). It maps transport errors to ErrUnreachable and 404/409 to
// ErrNotFound/ErrConflict; other non-2xx become *RequestError. The per-request
// timeout (dockerWidgetEndpointTimeoutMs) applies because the body is fully
// consumed before return.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, out any) error {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.do(cctx, method, path, query)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return engineError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker: decode %s response: %w", path, err)
	}
	return nil
}

// withTimeout applies the client timeout unless the caller already set a
// deadline. Streaming responses (logs) must NOT use it: the deadline would
// fire while the body is still being consumed.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || c.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.Timeout)
}

// do performs one Engine request and returns the response (caller closes the
// body). Streaming endpoints (logs) use it directly and own their deadline —
// the response header wait is bounded by the transport's
// ResponseHeaderTimeout.
func (c *Client) do(ctx context.Context, method, path string, query url.Values) (*http.Response, error) {
	u := url.URL{Scheme: "http", Host: "docker", Path: path}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("docker: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Caller cancellation propagates raw; every other transport failure
		// (missing socket, connection refused, deadline — upstream's
		// withDockerTimeoutAsync rejects with "Timed out ...") is an
		// unreachable engine.
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return resp, nil
}

// engineError maps a non-2xx response body to the typed errors.
func engineError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	err := &RequestError{
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Message:    payload.Message,
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, err.Error())
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, err.Error())
	default:
		return err
	}
}

// ListContainers is instance.listContainers({all}) —
// GET /containers/json?all=true (request-handler/src/docker.ts:386 lists with
// all: true; upstream passes no filters).
func (c *Client) ListContainers(ctx context.Context, all bool) ([]ContainerInfo, error) {
	query := url.Values{}
	if all {
		query.Set("all", "true")
	}
	var containers []ContainerInfo
	if err := c.doJSON(ctx, http.MethodGet, "/containers/json", query, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// Inspect is GET /containers/{id}/json. The router only checks existence
// (findDockerContainerAsync, request-handler/src/docker.ts:241-252); any
// error — including ErrUnreachable — means "unknown container" there.
func (c *Client) Inspect(ctx context.Context, containerID string) error {
	return c.doJSON(ctx, http.MethodGet, "/containers/"+containerID+"/json", nil, nil)
}

// Action performs one container action with dockerode's defaults: POST
// /containers/{id}/start|stop|restart (no timeout parameter — upstream calls
// container.start()/stop()/restart() without options) and DELETE
// /containers/{id} for remove.
func (c *Client) Action(ctx context.Context, containerID string, action Action) error {
	switch action {
	case ActionStart, ActionStop, ActionRestart:
		return c.doJSON(ctx, http.MethodPost, "/containers/"+containerID+"/"+string(action), nil, nil)
	case ActionRemove:
		return c.doJSON(ctx, http.MethodDelete, "/containers/"+containerID, nil, nil)
	default:
		return fmt.Errorf("docker: unknown action %q", action)
	}
}

// Logs opens GET /containers/{id}/logs with the upstream option set
// (tail, stdout, stderr, follow) and returns the raw stream —
// multiplexed frames when the container has no TTY. Use DecodeLogs or a
// FrameReader to demultiplex. The stream lives as long as the caller's
// context (follow streams must not carry the one-shot timeout).
func (c *Client) Logs(ctx context.Context, containerID string, o LogsOptions) (io.ReadCloser, error) {
	query := url.Values{}
	query.Set("stdout", "true")
	query.Set("stderr", "true")
	query.Set("follow", strconv.FormatBool(o.Follow))
	if o.Tail > 0 {
		query.Set("tail", strconv.Itoa(o.Tail))
	}
	if !o.Since.IsZero() {
		query.Set("since", strconv.FormatInt(o.Since.Unix(), 10))
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+containerID+"/logs", query)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, engineError(resp)
	}
	return resp.Body, nil
}

// ContainerStats is the subset of GET /containers/{id}/stats?one-shot=true
// consumed by calculateCpuUsage / calculateMemoryUsage
// (request-handler/src/docker.ts:522-567). Pointers reproduce the optional
// fields of Podman and one-shot responses (upstream handles missing fields
// with optional chaining).
type ContainerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Stats struct {
			Cache             *uint64 `json:"cache"`
			TotalInactiveFile *uint64 `json:"total_inactive_file"`
			InactiveFile      *uint64 `json:"inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
}

// StatsOneShot is instance.getContainer(id).stats({stream: false,
// "one-shot": true}) — GET /containers/{id}/stats?stream=false&one-shot=true
// (request-handler/src/docker.ts:455-458).
func (c *Client) StatsOneShot(ctx context.Context, containerID string) (ContainerStats, error) {
	query := url.Values{}
	query.Set("stream", "false")
	query.Set("one-shot", "true")
	var stats ContainerStats
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+containerID+"/stats", query, &stats); err != nil {
		return ContainerStats{}, err
	}
	return stats, nil
}

// checkTLSFiles reports the construction failure of a TLS endpoint: upstream
// reads ca/cert/key with readFileSync inside createDockerOptions
// (singleton.ts:147-150), so a missing file fails instance creation and the
// endpoint is listed as unavailable (initializationFailures).
func checkTLSFiles(ep Endpoint) error {
	if ep.Transport.Kind != TransportTLS {
		return nil
	}
	for _, path := range []string{ep.Transport.Path} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("docker: failed to initialize Docker endpoint: %w", err)
		}
	}
	return nil
}
