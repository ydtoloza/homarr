package widgets_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/api/ctxauth"
	"github.com/ydtoloza/homarr/server/internal/api/router/widgets"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Test harness: a real trpc.Router with the widget routers registered over
// httptest, with a header-driven session injection standing in for the auth
// middleware (the board harness pattern) and fake clients injected through
// Options factories.

type fakeResolver struct {
	rows    []widgets.Integration
	queries []widgets.ResolveRequest
	err     error
}

func (f *fakeResolver) resolve(ctx context.Context, req widgets.ResolveRequest) ([]widgets.Integration, error) {
	f.queries = append(f.queries, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeMOClient records the page sizes and answers canned results or errors.
type fakeMOClient struct {
	missing mediaorganizer.MissingResult
	queue   mediaorganizer.QueueResult
	err     error
	calls   []int
}

func (c *fakeMOClient) GetMissing(_ context.Context, pageSize int) (mediaorganizer.MissingResult, error) {
	c.calls = append(c.calls, pageSize)
	if c.err != nil {
		return mediaorganizer.MissingResult{}, c.err
	}
	return c.missing, nil
}

func (c *fakeMOClient) GetMediaQueue(_ context.Context, pageSize int) (mediaorganizer.QueueResult, error) {
	if c.err != nil {
		return mediaorganizer.QueueResult{}, c.err
	}
	return c.queue, nil
}

type fakeDCClient struct {
	jobs         downloadclient.JobsAndStatus
	jobsErr      error
	mutErr       error
	mutations    []string
	lastItem     downloadclient.Item
	lastFromDisk bool
}

func (c *fakeDCClient) GetJobsAndStatus(_ context.Context, input downloadclient.JobsInput) (downloadclient.JobsAndStatus, error) {
	if c.jobsErr != nil {
		return downloadclient.JobsAndStatus{}, c.jobsErr
	}
	return c.jobs, nil
}

func (c *fakeDCClient) record(name string, item downloadclient.Item, fromDisk bool) error {
	c.mutations = append(c.mutations, name)
	c.lastItem = item
	c.lastFromDisk = fromDisk
	return c.mutErr
}

func (c *fakeDCClient) PauseQueue(ctx context.Context) error {
	return c.record("pauseQueue", downloadclient.Item{}, false)
}
func (c *fakeDCClient) PauseItem(ctx context.Context, item downloadclient.Item) error {
	return c.record("pauseItem", item, false)
}
func (c *fakeDCClient) ResumeQueue(ctx context.Context) error {
	return c.record("resumeQueue", downloadclient.Item{}, false)
}
func (c *fakeDCClient) ResumeItem(ctx context.Context, item downloadclient.Item) error {
	return c.record("resumeItem", item, false)
}
func (c *fakeDCClient) DeleteItem(ctx context.Context, item downloadclient.Item, fromDisk bool) error {
	return c.record("deleteItem", item, fromDisk)
}

type harness struct {
	srv      *httptest.Server
	resolver *fakeResolver
	mo       *fakeMOClient
	dc       *fakeDCClient
	sessions map[string]*ctxauth.Session
}

func newHarness(t *testing.T, opts func(o *widgets.Options)) *harness {
	t.Helper()
	h := &harness{
		resolver: &fakeResolver{},
		mo:       &fakeMOClient{},
		dc:       &fakeDCClient{},
		sessions: map[string]*ctxauth.Session{},
	}
	options := widgets.Options{
		ResolveIntegrations: h.resolver.resolve,
		NewMediaOrganizer: func(widgets.Integration) (mediaorganizer.Client, error) {
			return h.mo, nil
		},
		NewDownloadClient: func(widgets.Integration) (downloadclient.Client, error) {
			return h.dc, nil
		},
		// deterministic cache behavior per test: default TTLs overridden by
		// opts below
	}
	if opts != nil {
		opts(&options)
	}
	r := trpc.New()
	widgets.Register(r, options)
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			session, ok := h.sessions[name]
			if !ok {
				t.Errorf("unknown test user %q", name)
			}
			req = req.WithContext(ctxauth.WithSession(req.Context(), session))
		}
		r.Handler().ServeHTTP(w, req)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) seedUser(name string, admin bool) {
	h.sessions[name] = &ctxauth.Session{ID: "s-" + name, UserID: "u-" + name, Name: name, IsAdministrator: admin}
}

// call performs one tRPC round trip (POST single; the wire shape is the
// classic httpLink envelope).
func (h *harness) call(t *testing.T, user, path, input string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(`{"json":`+input+`}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return res.StatusCode, string(raw)
}

// unwrapData peels {"result":{"data":{"json": X}}} into the raw X.
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
		t.Fatalf("expected success, got error %+v (body %s)", envelope.Error, body)
	}
	return envelope.Result.Data.JSON
}

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

// --- media organizer ---

func moFixture() (mediaorganizer.MissingResult, mediaorganizer.QueueResult) {
	year := int64(2024)
	season := int64(2)
	episode := int64(5)
	image := "https://img/poster.jpg"
	link := "https://ext/series/show"
	return mediaorganizer.MissingResult{
		TotalCount: 42,
		Items: []mediaorganizer.MissingItem{
			{ID: 7, Title: "S02E05", Type: mediaorganizer.TypeEpisode, Year: &year,
				SeasonNumber: &season, EpisodeNumber: &episode, ImageURL: &image, Link: link},
		},
	}, mediaorganizer.QueueResult{
		TotalCount: 1,
		Items: []mediaorganizer.QueuedItem{
			{ID: 11, Title: "Pilot", Type: mediaorganizer.TypeEpisode, Status: "downloading",
				TimeLeft: nil, PercentComplete: 75, ImageURL: &image, Link: link},
		},
	}
}

// TestMediaOrganizerGetDataSuccessShape pins the success entry byte-for-byte
// (field names AND order, error member absent).
func TestMediaOrganizerGetDataSuccessShape(t *testing.T) {
	h := newHarness(t, nil)
	h.mo.missing, h.mo.queue = moFixture()
	h.resolver.rows = []widgets.Integration{
		{ID: "int-1", Name: "My Sonarr", Kind: "sonarr", URL: "http://sonarr"},
	}
	status, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-1"],"pageSize":10}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}
	data := unwrapData(t, body)
	want := `[{"integrationId":"int-1","integrationName":"My Sonarr","missing":[{"id":7,"title":"S02E05","type":"episode","year":2024,"seasonNumber":2,"episodeNumber":5,"imageUrl":"https://img/poster.jpg","link":"https://ext/series/show"}],"missingCount":42,"queued":[{"id":11,"title":"Pilot","type":"episode","status":"downloading","timeLeft":null,"percentComplete":75,"imageUrl":"https://img/poster.jpg","link":"https://ext/series/show"}],"queuedCount":1}]`
	if string(data) != want {
		t.Fatalf("data mismatch:\n got: %s\nwant: %s", data, want)
	}
	// The resolver received the query action with the mediaMissing kind.
	if h.resolver.queries[0].Action != "query" || h.resolver.queries[0].WidgetKind != "mediaMissing" {
		t.Fatalf("resolve request = %+v", h.resolver.queries[0])
	}
}

func TestMediaOrganizerGetDataDefaultsPageSize(t *testing.T) {
	h := newHarness(t, nil)
	h.mo.missing, h.mo.queue = moFixture()
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "S", Kind: "sonarr"}}
	if _, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-1"]}`); body == "" {
		t.Fatal("empty body")
	}
	// Default pageSize 10 reaches the client twice (missing + queue share the
	// recorded sizes of GetMissing).
	if len(h.mo.calls) != 1 || h.mo.calls[0] != 10 {
		t.Fatalf("page sizes = %v, want [10]", h.mo.calls)
	}
}

func TestMediaOrganizerPageSizeValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "S", Kind: "sonarr"}}
	for _, pageSize := range []string{"0", "51", `"10"`} {
		input := fmt.Sprintf(`{"integrationIds":["int-1"],"pageSize":%s}`, pageSize)
		_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", input)
		code, message := unwrapError(t, body)
		if code != "BAD_REQUEST" {
			t.Fatalf("pageSize %s: code = %s (%s)", pageSize, code, message)
		}
	}
	// zod z.number() has no .int() here, so a fraction within the range is
	// valid upstream and truncates in the port.
	if _, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-1"],"pageSize":2.5}`); strings.Contains(body, `"error"`) {
		t.Fatalf("fractional pageSize must be accepted, got %s", body)
	}
}

func TestMediaOrganizerFallbackAndPartialFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.resolver.rows = []widgets.Integration{
		{ID: "int-ok", Name: "OK", Kind: "sonarr"},
		{ID: "int-bad", Name: "Bad", Kind: "sonarr"},
	}
	h.mo.missing, h.mo.queue = moFixture()
	// The shared fake client cannot fail per integration — use a factory
	// keyed by the integration id instead.
	options := widgets.Options{
		ResolveIntegrations: h.resolver.resolve,
		NewMediaOrganizer: func(row widgets.Integration) (mediaorganizer.Client, error) {
			if row.ID == "int-bad" {
				return &fakeMOClient{err: fmt.Errorf("boom")}, nil
			}
			missing, queue := moFixture()
			return &fakeMOClient{missing: missing, queue: queue}, nil
		},
	}
	r := trpc.New()
	widgets.Register(r, options)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	h.srv = srv

	_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-ok","int-bad"],"pageSize":10}`)
	data := unwrapData(t, body)
	want := `[{"integrationId":"int-ok","integrationName":"OK","missing":[{"id":7,"title":"S02E05","type":"episode","year":2024,"seasonNumber":2,"episodeNumber":5,"imageUrl":"https://img/poster.jpg","link":"https://ext/series/show"}],"missingCount":42,"queued":[{"id":11,"title":"Pilot","type":"episode","status":"downloading","timeLeft":null,"percentComplete":75,"imageUrl":"https://img/poster.jpg","link":"https://ext/series/show"}],"queuedCount":1},` +
		`{"integrationId":"int-bad","integrationName":"Bad","missing":[],"missingCount":0,"queued":[],"queuedCount":0,"error":"INTEGRATION_REQUEST_FAILED"}]`
	if string(data) != want {
		t.Fatalf("data mismatch:\n got: %s\nwant: %s", data, want)
	}
}

func TestMediaOrganizerAllFailuresThrow(t *testing.T) {
	h := newHarness(t, nil)
	h.resolver.rows = []widgets.Integration{{ID: "int-bad", Name: "Bad", Kind: "sonarr"}}
	options := widgets.Options{
		ResolveIntegrations: h.resolver.resolve,
		NewMediaOrganizer: func(widgets.Integration) (mediaorganizer.Client, error) {
			return &fakeMOClient{err: fmt.Errorf("boom")}, nil
		},
	}
	r := trpc.New()
	widgets.Register(r, options)
	h.srv = httptest.NewServer(r.Handler())
	defer h.srv.Close()

	_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-bad"]}`)
	code, message := unwrapError(t, body)
	if code != "INTERNAL_SERVER_ERROR" || message != "All integration queries failed" {
		t.Fatalf("error = %s %q", code, message)
	}
}

func TestMediaOrganizerEmptyIntegrationIDs(t *testing.T) {
	h := newHarness(t, nil)
	_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":[]}`)
	if data := string(unwrapData(t, body)); data != "[]" {
		t.Fatalf("data = %s, want []", data)
	}
}

// TestResolverNotFoundMessage pins the exact upstream NOT_FOUND message of
// the many-integration middleware. The nil-resolver default behaves as "the
// DB has no matching rows".
func TestResolverNotFoundMessage(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.ResolveIntegrations = nil })
	_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["nope"]}`)
	code, message := unwrapError(t, body)
	if code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
	want := "1 of the specified integrations not found or not of kinds sonarr,sportarr,radarr,bindery,mock: ([nope] compared to [])"
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}
}

func TestResolverForbiddenPassthrough(t *testing.T) {
	h := newHarness(t, nil)
	h.resolver.err = trpc.NewError(trpc.CodeForbidden, "User does not have permission to query at least one of the specified integration")
	_, body := h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["x"]}`)
	code, message := unwrapError(t, body)
	if code != "FORBIDDEN" || message != "User does not have permission to query at least one of the specified integration" {
		t.Fatalf("error = %s %q", code, message)
	}
}

// --- downloads ---

func dcFixture() downloadclient.JobsAndStatus {
	sent := 100.0
	up := 60.0
	return downloadclient.JobsAndStatus{
		Status: downloadclient.Status{
			Paused: false,
			Rates:  downloadclient.Rates{Down: 300, Up: &up},
			Types:  []string{"torrent"},
		},
		Items: []downloadclient.Item{
			{ID: "h1", Index: 1, Name: "One", Type: "torrent", Size: 1000, Sent: &sent,
				DownSpeed: nil, UpSpeed: &up, Time: 5000, Added: nil, State: "leeching",
				Progress: 0.5, Category: "movies"},
		},
	}
}

// TestDownloadsGetJobsAndStatusesSuccessShape pins the success entry: the
// integration info with updatedAt (ISO convention for the upstream Date) and
// the JobsAndStatus data.
func TestDownloadsGetJobsAndStatusesSuccessShape(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.dc.jobs = dcFixture()
	h.resolver.rows = []widgets.Integration{
		{ID: "int-q", Name: "My qBit", Kind: "qBittorrent", URL: "http://qbit"},
	}
	status, body := h.call(t, "", "/api/trpc/widget.downloads.getJobsAndStatuses",
		`{"integrationIds":["int-q"],"limitPerIntegration":50,"historyWindowDays":7,"includeArchivedHistory":false}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}
	data := unwrapData(t, body)
	var parsed []struct {
		IntegrationID   string `json:"integrationId"`
		IntegrationName string `json:"integrationName"`
		Integration     struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"integration"`
		Data *struct {
			Status downloadclient.Status `json:"status"`
			Items  []downloadclient.Item `json:"items"`
		} `json:"data"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(parsed) != 1 {
		t.Fatalf("entries = %d", len(parsed))
	}
	entry := parsed[0]
	if entry.IntegrationID != "int-q" || entry.IntegrationName != "My qBit" ||
		entry.Integration.ID != "int-q" || entry.Integration.Name != "My qBit" ||
		entry.Integration.Kind != "qBittorrent" {
		t.Fatalf("entry = %+v", entry)
	}
	// updatedAt is the ISO-millis convention of the port (ADMIN.md §4).
	if !strings.HasSuffix(entry.Integration.UpdatedAt, "Z") ||
		!strings.Contains(entry.Integration.UpdatedAt, ".") {
		t.Fatalf("updatedAt = %q, want ISO with millis and Z", entry.Integration.UpdatedAt)
	}
	if entry.Data == nil || entry.Error != nil {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Data.Status.Rates.Down != 300 {
		t.Fatalf("status = %+v", entry.Data.Status)
	}
	raw, _ := json.Marshal(entry.Data.Items)
	wantItems := `[{"id":"h1","index":1,"name":"One","type":"torrent","size":1000,"sent":100,"upSpeed":60,"time":5000,"state":"leeching","progress":0.5,"category":"movies"}]`
	if string(raw) != wantItems {
		t.Fatalf("items mismatch:\n got: %s\nwant: %s", raw, wantItems)
	}
}

// TestDownloadsFailureShape pins the error entry: data null, error constant,
// integration info without updatedAt.
func TestDownloadsFailureShape(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{
		{ID: "int-ok", Name: "ok", Kind: "deluge"},
		{ID: "int-q", Name: "q", Kind: "qBittorrent"},
	}
	options := widgets.Options{
		ResolveIntegrations: h.resolver.resolve,
		NewDownloadClient: func(row widgets.Integration) (downloadclient.Client, error) {
			if row.ID == "int-q" {
				return &fakeDCClient{jobsErr: fmt.Errorf("down")}, nil
			}
			return &fakeDCClient{jobs: dcFixture()}, nil
		},
	}
	r := trpc.New()
	widgets.Register(r, options)
	h.srv = httptest.NewServer(r.Handler())
	defer h.srv.Close()

	_, body := h.call(t, "", "/api/trpc/widget.downloads.getJobsAndStatuses", `{"integrationIds":["int-ok","int-q"]}`)
	data := unwrapData(t, body)
	// First entry succeeds (deluge fixture), second one fails.
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("entries = %s", data)
	}
	want := `{"integrationId":"int-q","integrationName":"q","integration":{"id":"int-q","name":"q","kind":"qBittorrent"},"data":null,"error":"INTEGRATION_REQUEST_FAILED"}`
	if string(entries[1]) != want {
		t.Fatalf("failure entry mismatch:\n got: %s\nwant: %s", entries[1], want)
	}
	if !strings.Contains(string(entries[0]), `"data":{"status"`) {
		t.Fatalf("success entry = %s", entries[0])
	}
}

func TestDownloadsSelectionValidation(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{{ID: "int-q", Name: "q", Kind: "qBittorrent"}}
	cases := []string{
		`{"integrationIds":["int-q"],"selection":{"sort":"bogus","descending":false,"categoryFilter":[],"filterIsWhitelist":true,"showCompletedTorrent":true,"activeTorrentThreshold":0}}`,
		`{"integrationIds":["int-q"],"selection":{"sort":"name","descending":false,"categoryFilter":[],"filterIsWhitelist":true,"showCompletedTorrent":true,"activeTorrentThreshold":-1}}`,
		`{"integrationIds":["int-q"],"selection":{"sort":"name","categoryFilter":[],"filterIsWhitelist":true,"showCompletedTorrent":true,"activeTorrentThreshold":0}}`,
		`{"integrationIds":["int-q"],"historyWindowDays":0}`,
		`{"integrationIds":["int-q"],"historyWindowDays":1.5}`,
	}
	for _, input := range cases {
		_, body := h.call(t, "", "/api/trpc/widget.downloads.getJobsAndStatuses", input)
		code, _ := unwrapError(t, body)
		if code != "BAD_REQUEST" {
			t.Fatalf("input %s: code = %s", input, code)
		}
	}
}

// --- mutations ---

func TestDownloadsMutationsRequireSession(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{{ID: "int-q", Name: "q", Kind: "qBittorrent"}}
	for _, path := range []string{"pause", "resume"} {
		_, body := h.call(t, "", "/api/trpc/widget.downloads."+path, `{"integrationIds":["int-q"]}`)
		code, message := unwrapError(t, body)
		if code != "UNAUTHORIZED" || message != "UNAUTHORIZED" {
			t.Fatalf("%s anonymous: %s %q", path, code, message)
		}
	}
	_, body := h.call(t, "", "/api/trpc/widget.downloads.pauseItem", `{"integrationIds":["int-q"],"item":{"id":"h1","index":1,"name":"One","type":"torrent","size":1000,"time":0,"state":"leeching","progress":0.5}}`)
	if code, _ := unwrapError(t, body); code != "UNAUTHORIZED" {
		t.Fatalf("pauseItem anonymous: %s", code)
	}
}

func TestDownloadsMutationsReadOnlyKeyForbidden(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{{ID: "int-q", Name: "q", Kind: "qBittorrent"}}
	h.sessions["ro"] = &ctxauth.Session{ID: "s", UserID: "u", Name: "ro", ReadOnly: true}
	_, body := h.call(t, "ro", "/api/trpc/widget.downloads.pause", `{"integrationIds":["int-q"]}`)
	code, message := unwrapError(t, body)
	if code != "FORBIDDEN" || message != "This credential only permits read-only operations" {
		t.Fatalf("error = %s %q", code, message)
	}
}

func TestDownloadsMutationsRunOnAllIntegrations(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{
		{ID: "int-1", Name: "A", Kind: "qBittorrent"},
		{ID: "int-2", Name: "B", Kind: "deluge"},
	}
	h.seedUser("alice", false)
	// pause/resume run the queue mutation for EVERY resolved integration.
	if _, body := h.call(t, "alice", "/api/trpc/widget.downloads.pause", `{"integrationIds":["int-1","int-2"]}`); strings.Contains(body, "error") {
		t.Fatalf("pause body = %s", body)
	}
	if len(h.dc.mutations) != 2 {
		t.Fatalf("mutations = %v", h.dc.mutations)
	}
	// The resolver saw the interact action.
	if h.resolver.queries[0].Action != "interact" {
		t.Fatalf("action = %s", h.resolver.queries[0].Action)
	}

	// pauseItem forwards the item.
	item := `{"integrationIds":["int-1"],"item":{"id":"h1","index":2,"name":"One","type":"torrent","size":10,"time":5,"state":"leeching","progress":0.5}}`
	if _, body := h.call(t, "alice", "/api/trpc/widget.downloads.pauseItem", item); strings.Contains(body, `"error"`) {
		t.Fatalf("pauseItem body = %s", body)
	}
	if h.dc.lastItem.ID != "h1" || h.dc.lastItem.Index != 2 || h.dc.lastItem.Name != "One" {
		t.Fatalf("last item = %+v", h.dc.lastItem)
	}

	// deleteItem carries fromDisk.
	deleteInput := `{"integrationIds":["int-1"],"item":{"id":"h1","index":2,"name":"One","type":"torrent","size":10,"time":5,"state":"paused","progress":1},"fromDisk":true}`
	if _, body := h.call(t, "alice", "/api/trpc/widget.downloads.deleteItem", deleteInput); strings.Contains(body, `"error"`) {
		t.Fatalf("deleteItem body = %s", body)
	}
	if !h.dc.lastFromDisk {
		t.Fatal("fromDisk must reach the client")
	}

	// An invalid item (bad type / progress) is a BAD_REQUEST.
	badItem := `{"integrationIds":["int-1"],"item":{"id":"h1","index":2,"name":"One","type":"wrong","size":10,"time":5,"state":"paused","progress":1}}`
	_, badBody := h.call(t, "alice", "/api/trpc/widget.downloads.pauseItem", badItem)
	if code, _ := unwrapError(t, badBody); code != "BAD_REQUEST" {
		t.Fatalf("bad item code = %s", code)
	}
}

func TestDownloadsMutationFailureSurfacesIntegrationMessage(t *testing.T) {
	h := newHarness(t, func(o *widgets.Options) { o.MediaOrganizerCacheTTLMS = -1 })
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "A", Kind: "qBittorrent"}}
	h.seedUser("alice", false)
	h.dc.mutErr = downloadclient.NewRequestError(fmt.Errorf("refused"))
	_, body := h.call(t, "alice", "/api/trpc/widget.downloads.pause", `{"integrationIds":["int-1"]}`)
	code, message := unwrapError(t, body)
	if code != "INTERNAL_SERVER_ERROR" || message != "Request to integration failed" {
		t.Fatalf("error = %s %q", code, message)
	}
}

// TestDownloadsCacheInvalidation pins the 10s cache with a 30ms override and
// the mutation invalidation: a mutation dropped mid-TTL forces the next query
// to rebuild the client.
func TestDownloadsCacheInvalidation(t *testing.T) {
	builds := 0
	dc := &fakeDCClient{}
	h := newHarness(t, func(o *widgets.Options) {
		o.DownloadsCacheTTLMS = 30
		o.MediaOrganizerCacheTTLMS = -1
		o.NewDownloadClient = func(widgets.Integration) (downloadclient.Client, error) {
			builds++
			return dc, nil
		}
	})
	h.dc = dc
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "A", Kind: "qBittorrent"}}
	h.seedUser("alice", false)

	for i := 0; i < 3; i++ {
		if _, body := h.call(t, "", "/api/trpc/widget.downloads.getJobsAndStatuses", `{"integrationIds":["int-1"]}`); strings.Contains(body, "error\":{") {
			t.Fatalf("body = %s", body)
		}
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1 (single-flight within the TTL)", builds)
	}
	// A mutation invalidates the cache entry even though the TTL has not
	// expired.
	if _, body := h.call(t, "alice", "/api/trpc/widget.downloads.pause", `{"integrationIds":["int-1"]}`); strings.Contains(body, "error\":{") {
		t.Fatalf("pause body = %s", body)
	}
	h.call(t, "", "/api/trpc/widget.downloads.getJobsAndStatuses", `{"integrationIds":["int-1"]}`)
	// builds: query 1 + the mutation's own client build + the post-invalidation
	// query (the mutation path never uses the cache, like upstream's
	// createIntegrationAsync).
	if builds != 3 {
		t.Fatalf("builds after mutation = %d, want 3", builds)
	}
}

// TestMediaOrganizerCacheTTL pins the 60s cache with a 30ms override: many
// queries inside the window produce ONE client build per (integration,input).
func TestMediaOrganizerCacheTTL(t *testing.T) {
	builds := 0
	h := newHarness(t, func(o *widgets.Options) {
		o.MediaOrganizerCacheTTLMS = 30
		o.DownloadsCacheTTLMS = -1
		o.NewMediaOrganizer = func(widgets.Integration) (mediaorganizer.Client, error) {
			builds++
			missing, queue := moFixture()
			return &fakeMOClient{missing: missing, queue: queue}, nil
		}
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "S", Kind: "sonarr"}}
	for i := 0; i < 5; i++ {
		h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-1"],"pageSize":10}`)
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1 (single-flight within the TTL)", builds)
	}
	// A different pageSize is a different cache key.
	h.call(t, "", "/api/trpc/widget.mediaOrganizer.getData", `{"integrationIds":["int-1"],"pageSize":20}`)
	if builds != 2 {
		t.Fatalf("builds after new input = %d, want 2", builds)
	}
}
