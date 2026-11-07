package docker_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/docker"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
	dockerclient "github.com/ydtoloza/homarr/server/internal/integrations/docker"
	"github.com/ydtoloza/homarr/server/internal/ws"
)

// Test harness (house style of internal/api/router/board/harness_test.go):
// a real trpc.Router with the docker router registered and served over
// httptest, a header-driven session injection standing in for the auth
// middleware, and a fake Docker Engine API (the same routes the client
// speaks, plus STREAMING log bodies for the pump).

type recordedRequest struct {
	Method string
	Path   string
	Query  string
}

// logStream is one follow=true log stream the test can push frames into.
type logStream struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newLogStream() *logStream {
	return &logStream{frames: make(chan []byte, 16), closed: make(chan struct{})}
}

func (ls *logStream) push(frame []byte) {
	ls.frames <- frame
}

func (ls *logStream) close() {
	ls.once.Do(func() { close(ls.frames) })
}

type fakeEngine struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest

	containers   []dockerclient.ContainerInfo
	stats        dockerclient.ContainerStats
	failInspect  bool
	staticLogs   []byte // body for follow=false log queries
	actionStatus int
	actionBody   string
	statsStatus  int // overrides the 2xx answer of /stats (degraded tests)

	followMu   sync.Mutex
	follows    []*logStream
	allStreams []*logStream
	nextStream *logStream // set when a test wants to intercept the next stream
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{}
	fe.srv = httptest.NewServer(http.HandlerFunc(fe.handle))
	t.Cleanup(fe.srv.Close)
	return fe
}

// closeAllStreams ends every follow stream so pumps see EOF and release
// their requests; registered BEFORE the server cleanup (LIFO) so
// httptest.Server.Close never blocks on an open log stream.
func (fe *fakeEngine) closeAllStreams() {
	fe.followMu.Lock()
	defer fe.followMu.Unlock()
	fe.nextStream = nil
	for _, ls := range fe.allStreams {
		ls.close()
	}
	fe.follows = nil
}

// endpoint builds the tcp endpoint pointing at the fake engine.
func (fe *fakeEngine) endpoint() dockerclient.Endpoint {
	addr := strings.TrimPrefix(fe.srv.URL, "http://")
	host, port, _ := strings.Cut(addr, ":")
	portNum, _ := strconv.Atoi(port)
	return dockerclient.LegacyTCPEndpoint(host, portNum)
}

func (fe *fakeEngine) registry() *dockerclient.Registry {
	return dockerclient.NewRegistry([]dockerclient.Endpoint{fe.endpoint()}, nil)
}

func (fe *fakeEngine) record(r *http.Request) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	fe.requests = append(fe.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
}

func (fe *fakeEngine) requestCount() int {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return len(fe.requests)
}

func (fe *fakeEngine) lastRequest() recordedRequest {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if len(fe.requests) == 0 {
		return recordedRequest{}
	}
	return fe.requests[len(fe.requests)-1]
}

func (fe *fakeEngine) handle(w http.ResponseWriter, r *http.Request) {
	fe.record(r)
	switch {
	case r.URL.Path == "/containers/json":
		fe.mu.Lock()
		containers := fe.containers
		fe.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	case strings.HasSuffix(r.URL.Path, "/json") && strings.HasPrefix(r.URL.Path, "/containers/"):
		fe.mu.Lock()
		fail := fe.failInspect
		fe.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: abc"}`))
			return
		}
		_, _ = w.Write([]byte(`{"Id":"abc","Name":"/abc"}`))
	case strings.HasSuffix(r.URL.Path, "/logs"):
		isFollow := strings.Contains(r.URL.RawQuery, "follow=true")
		if !isFollow {
			fe.mu.Lock()
			body := fe.staticLogs
			fe.mu.Unlock()
			_, _ = w.Write(body)
			return
		}
		fe.followMu.Lock()
		ls := fe.nextStream
		fe.nextStream = nil
		if ls == nil {
			ls = newLogStream()
			fe.follows = append(fe.follows, ls)
		}
		fe.allStreams = append(fe.allStreams, ls)
		fe.followMu.Unlock()
		fe.serveStream(w, r, ls)
	case strings.HasSuffix(r.URL.Path, "/stats"):
		fe.mu.Lock()
		stats, status := fe.stats, fe.statsStatus
		fe.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"stats boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/containers/"),
		r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
		fe.mu.Lock()
		status, body := fe.actionStatus, fe.actionBody
		fe.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"page not found"}`))
	}
}

// serveStream writes frames as the test pushes them; the stream stays open
// until the test closes it or the client disconnects.
func (fe *fakeEngine) serveStream(w http.ResponseWriter, r *http.Request, ls *logStream) {
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case frame, open := <-ls.frames:
			if !open {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// takeFollowStream waits for the pump to open the next follow stream.
func (fe *fakeEngine) takeFollowStream(t *testing.T) *logStream {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		fe.followMu.Lock()
		if len(fe.follows) > 0 {
			ls := fe.follows[0]
			fe.follows = fe.follows[1:]
			fe.followMu.Unlock()
			return ls
		}
		fe.followMu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("no follow stream opened")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// encodeFrame builds one multiplexed log frame.
func encodeFrame(streamType byte, message string) []byte {
	payload := []byte(message)
	frame := make([]byte, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

// API harness -----------------------------------------------------------------

type testUser struct {
	id    string
	name  string
	admin bool // fork administrator flag on the session
	ro    bool // read-only API key flag
}

// newTestAPI builds the tRPC server with the docker router over the fake
// engine, plus the ws subscription handlers.
type testAPI struct {
	db       *db.DB
	engine   *fakeEngine
	srv      *httptest.Server
	sessions map[string]*ctxauth.Session
	handlers map[string]wsHandler
	options  docker.Options
}

// wsHandler mirrors the SubscriptionHandler signature without importing the
// ws package types into every assertion.
type wsHandler interface {
	Subscribe(ctx context.Context, req *wsRequest) (*wsSubscription, error)
}

// The real types are aliased through the ws package at compile time below;
// the aliases keep the harness readable.
type wsRequest = ws.Request
type wsSubscription = ws.Subscription

// newTestAPI boots the API. sessions maps user names to sessions injected by
// the X-Test-User header.
func newTestAPI(t *testing.T, mutate func(fe *fakeEngine, opts *docker.Options)) *testAPI {
	t.Helper()
	d := db.OpenTest(t)
	fe := newFakeEngine(t)
	opts := docker.Options{}
	if mutate != nil {
		mutate(fe, &opts)
	}
	return buildAPI(t, d, fe, fe.registry(), opts)
}

func buildAPI(t *testing.T, d *db.DB, fe *fakeEngine, factory docker.DockerClientFactory, opts docker.Options) *testAPI {
	t.Helper()
	api := &testAPI{db: d, engine: fe, options: opts, sessions: map[string]*ctxauth.Session{}}

	r := trpc.New()
	svc := docker.Register(r, d, factory, opts)
	handlers := svc.SubscriptionHandlers()
	wsHandlers := make(map[string]wsHandler, len(handlers))
	for path, h := range handlers {
		wsHandlers[path] = h
	}
	api.handlers = wsHandlers

	// Runs before t.Cleanup(srv.Close) (LIFO): pumps must see EOF before the
	// server waits for their requests.
	t.Cleanup(fe.closeAllStreams)

	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			session, ok := api.sessions[name]
			if !ok {
				t.Errorf("unknown test user %q", name)
			}
			req = req.WithContext(ctxauth.WithSession(req.Context(), session))
		}
		r.Handler().ServeHTTP(w, req)
	})
	api.srv = httptest.NewServer(mux)
	t.Cleanup(api.srv.Close)
	return api
}

// withRegistry rebuilds the API on the same DB/engine but with a custom
// endpoint factory (capability-restricted registries). The seeded sessions
// carry over.
func (api *testAPI) withRegistry(t *testing.T, factory docker.DockerClientFactory) *testAPI {
	t.Helper()
	next := buildAPI(t, api.db, api.engine, factory, api.options)
	for name, session := range api.sessions {
		next.sessions[name] = session
	}
	return next
}

// buildAPICustom boots a fresh API around an explicit engine + factory.
func buildAPICustom(t *testing.T, fe *fakeEngine, factory docker.DockerClientFactory) *testAPI {
	t.Helper()
	return buildAPI(t, db.OpenTest(t), fe, factory, docker.Options{})
}

// seedUser creates the user row and the session.
func (api *testAPI) seedUser(t *testing.T, u testUser) *ctxauth.Session {
	t.Helper()
	ctx := context.Background()
	if err := api.db.CreateUser(ctx, db.User{ID: u.id, Name: u.name, Provider: "credentials"}); err != nil {
		t.Fatalf("seed user %s: %v", u.name, err)
	}
	session := &ctxauth.Session{ID: "session-" + u.id, UserID: u.id, Name: u.name, IsAdministrator: u.admin, ReadOnly: u.ro}
	api.sessions[u.name] = session
	return session
}

// seedAliceAdmin seeds the canonical admin caller named "alice".
func seedAliceAdmin(t *testing.T, api *testAPI) *ctxauth.Session {
	t.Helper()
	return api.seedUser(t, testUser{id: "u-alice", name: "alice", admin: true})
}

// seedGroupAdmin puts the user in a group carrying the "admin" permission key
// (the upstream way of being admin).
func (api *testAPI) seedGroupAdmin(t *testing.T, userID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := api.db.ExecContext(ctx,
		`INSERT INTO "group" (id, name, position) VALUES (?, ?, 0)`, "g-"+userID, "admins-"+userID); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := api.db.ExecContext(ctx,
		`INSERT INTO "groupMember" (group_id, user_id) VALUES (?, ?)`, "g-"+userID, userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := api.db.ExecContext(ctx,
		`INSERT INTO "groupPermission" (group_id, permission) VALUES (?, ?)`, "g-"+userID, "admin"); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
}

// call performs one tRPC round trip: method GET (path with ?input=) or POST
// (body).
func (api *testAPI) call(t *testing.T, user string, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, api.srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	res, err := api.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return res.StatusCode, string(raw)
}

// unwrapData peels {"result":{"data":{"json": X}}} into X.
func unwrapData(t *testing.T, body string) json.RawMessage {
	t.Helper()
	var envelope struct {
		Result struct {
			Data struct {
				JSON json.RawMessage `json:"json"`
			} `json:"data"`
		} `json:"result"`
		Error *struct {
			JSON struct {
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("expected success, got error %+v (body %s)", envelope.Error.JSON.Data.Code, body)
	}
	return envelope.Result.Data.JSON
}

// unwrapError peels the error envelope of a failed single call.
func unwrapError(t *testing.T, body string) (code, message string) {
	t.Helper()
	var envelope struct {
		Error *struct {
			JSON struct {
				Message string `json:"message"`
				Data    struct {
					Code string `json:"code"`
				} `json:"data"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Error == nil {
		t.Fatalf("expected error envelope, got %s", body)
	}
	return envelope.Error.JSON.Data.Code, envelope.Error.JSON.Message
}

// expectError runs one call and asserts its tRPC error code.
func (api *testAPI) expectError(t *testing.T, user string, method, path, body, wantCode string) string {
	t.Helper()
	_, resBody := api.call(t, user, method, path, body)
	gotCode, message := unwrapError(t, resBody)
	if gotCode != wantCode {
		t.Fatalf("%s %s: got error %s (%s), want %s", method, path, gotCode, message, wantCode)
	}
	return message
}

// encodedInput builds the ?input= query value of a GET call.
func encodedInput(payload string) string {
	if payload == "" {
		payload = "null"
	}
	return "?input=" + `{"json":` + payload + `}`
}

// subscribe invokes the ws subscription handler directly (the hub is owned
// by the integrator; the handler contract is what this package provides).
func (api *testAPI) subscribe(t *testing.T, session *ctxauth.Session, input string) (*ws.Subscription, error) {
	t.Helper()
	handler, ok := api.handlers["docker.subscribeLogs"]
	if !ok {
		t.Fatalf("docker.subscribeLogs handler not registered")
	}
	return handler.Subscribe(context.Background(), &ws.Request{
		Path:    "docker.subscribeLogs",
		Input:   json.RawMessage(input),
		Session: session,
	})
}

// trpcErrorCode extracts the tRPC code/message from a handler error.
func trpcErrorCode(t *testing.T, err error) (code, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	terr, ok := err.(*trpc.Error)
	if !ok {
		t.Fatalf("expected *trpc.Error, got %T", err)
	}
	return string(terr.Code), terr.Message
}

// concatFrames joins multiplexed frames into one stream body.
func concatFrames(frames ...[]byte) []byte {
	out := []byte{}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// closedPort returns a TCP port nothing listens on.
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

// limitedEndpoint returns the engine endpoint stripped to one capability.
func limitedEndpoint(fe *fakeEngine, keep dockerclient.Capability) dockerclient.Endpoint {
	ep := fe.endpoint()
	ep.Capabilities = []dockerclient.Capability{keep}
	return ep
}
