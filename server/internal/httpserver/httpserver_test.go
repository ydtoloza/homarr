package httpserver_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/httpserver"
)

func TestHealthEndpoints(t *testing.T) {
	h := httpserver.New(httpserver.Options{Version: "test", Started: time.Now()})
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/api/health/live", "/api/health/ready"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: got status %d, want 200", path, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), `"status":"ok"`) {
			t.Errorf("GET %s: body %q does not report ok", path, body)
		}
	}
}

func TestSPAServesFilesAndFallsBackToIndex(t *testing.T) {
	dist := fstest.MapFS{
		"index.html": {Data: []byte("<html>spa-shell</html>")},
		"app.js":     {Data: []byte("console.log(1)")},
	}
	h := httpserver.New(httpserver.Options{Static: dist, Started: time.Now()})
	srv := httptest.NewServer(h)
	defer srv.Close()

	tests := []struct {
		path string
		want string
	}{
		{"/", "<html>spa-shell</html>"},
		{"/app.js", "console.log(1)"},
		// Unknown client-side route → SPA shell (client router takes over).
		{"/boards/my-board", "<html>spa-shell</html>"},
	}
	for _, tt := range tests {
		res, err := http.Get(srv.URL + tt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: got status %d, want 200", tt.path, res.StatusCode)
		}
		if !strings.Contains(string(body), tt.want) {
			t.Errorf("GET %s: body %q, want it to contain %q", tt.path, body, tt.want)
		}
	}
}

func TestLandingWithoutStatic(t *testing.T) {
	h := httpserver.New(httpserver.Options{Started: time.Now()})
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503 (no frontend embedded)", res.StatusCode)
	}
}

func TestTRPCMountAnswersProtocolErrors(t *testing.T) {
	h := httpserver.New(httpserver.Options{
		TRPC:    trpc.New().Handler(), // no procedures registered yet (M2)
		Started: time.Now(),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/trpc/board.getAll")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("got status %d, want 404 (tRPC NOT_FOUND)", res.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"NOT_FOUND"`) {
		t.Errorf("body %q does not carry the tRPC NOT_FOUND envelope", body)
	}
}
