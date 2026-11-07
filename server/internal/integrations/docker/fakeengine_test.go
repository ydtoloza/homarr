package docker_test

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// fakeEngine is a minimal Docker Engine API server speaking the routes the
// client uses: /containers/json, /containers/{id}/json (inspect),
// /containers/{id}/start|stop|restart, /containers/{id}/logs and
// /containers/{id}/stats. It records the requests it saw so tests can assert
// method, path and query exactly.

type recordedRequest struct {
	Method string
	Path   string
	Query  string
}

type fakeEngine struct {
	srv        *httptest.Server
	requests   []recordedRequest
	containers []docker.ContainerInfo
	stats      docker.ContainerStats
	// logsBody is the raw stream body served by /logs (multiplexed frames
	// or raw text).
	logsBody []byte
	// failInspect makes every inspect return 404.
	failInspect bool
	// actionStatus overrides the 2xx answer of the action routes.
	actionStatus int
	actionBody   string
	// hang delays the /containers/json answer (timeout tests).
	hang time.Duration
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{}
	fe.srv = httptest.NewServer(http.HandlerFunc(fe.handle))
	t.Cleanup(fe.srv.Close)
	return fe
}

// endpoint builds a tcp endpoint pointing at the fake engine.
func (fe *fakeEngine) endpoint() docker.Endpoint {
	addr := strings.TrimPrefix(fe.srv.URL, "http://")
	host, port, _ := strings.Cut(addr, ":")
	portNum, _ := strconv.Atoi(port)
	return docker.LegacyTCPEndpoint(host, portNum)
}

func (fe *fakeEngine) handle(w http.ResponseWriter, r *http.Request) {
	fe.requests = append(fe.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
	switch {
	case r.URL.Path == "/containers/json":
		if fe.hang > 0 {
			time.Sleep(fe.hang)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fe.containers)
	case strings.HasSuffix(r.URL.Path, "/json") && strings.HasPrefix(r.URL.Path, "/containers/"):
		if fe.failInspect {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: abc"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"abc","Name":"/abc"}`))
	case strings.HasSuffix(r.URL.Path, "/logs"):
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		_, _ = w.Write(fe.logsBody)
	case strings.HasSuffix(r.URL.Path, "/stats"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fe.stats)
	case strings.HasSuffix(r.URL.Path, "/start"),
		strings.HasSuffix(r.URL.Path, "/stop"),
		strings.HasSuffix(r.URL.Path, "/restart"):
		if fe.actionStatus != 0 {
			w.WriteHeader(fe.actionStatus)
			_, _ = w.Write([]byte(fe.actionBody))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
		if fe.actionStatus != 0 {
			w.WriteHeader(fe.actionStatus)
			_, _ = w.Write([]byte(fe.actionBody))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"page not found"}`))
	}
}

// encodeDockerLogFrame builds one multiplexed stream frame
// ([type, 0, 0, 0, length BE] + payload), the wire format the Engine uses
// when the container has no TTY.
func encodeDockerLogFrame(streamType byte, message string) []byte {
	payload := []byte(message)
	frame := make([]byte, frameHeaderSize+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

const frameHeaderSize = 8

// mustRegistry builds a registry with one endpoint pointing at the engine.
func mustRegistry(t *testing.T, fe *fakeEngine) *docker.Registry {
	t.Helper()
	return docker.NewRegistry([]docker.Endpoint{fe.endpoint()}, nil)
}
