package bangs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/router/bangs"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// TestBangsSearchDeferred pins the deferral documented in the package
// comment: bangs.search is NOT registered (upstream fetches the DuckDuckGo
// bang list over the network), so callers get the standard NOT_FOUND
// envelope for an unregistered procedure — the same behavior the board
// router's deferred procedures have.
func TestBangsSearchDeferred(t *testing.T) {
	d := db.OpenTest(t)
	r := trpc.New()
	bangs.Register(r, d, bangs.Options{})
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	res, err := srv.Client().Get(srv.URL + "/api/trpc/bangs.search")
	if err != nil {
		t.Fatalf("bangs.search: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("bangs.search status = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(string(raw), `"code":"NOT_FOUND"`) ||
		!strings.Contains(string(raw), `No procedure found on path \"bangs.search\"`) {
		t.Fatalf("bangs.search body = %s, want NOT_FOUND envelope", raw)
	}
}
