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
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
	"github.com/ydtoloza/homarr/server/internal/integrations/network"
)

// Harness for the media-server and dns-hole widget routers: same pattern as
// widgets_test.go (real trpc.Router over httptest, header-driven session,
// fake clients injected through the Options factories).

type fakeMSClient struct {
	sessions []mediaserver.StreamSession
	err      error
	inputs   []mediaserver.CurrentSessionsInput
}

func (c *fakeMSClient) GetCurrentSessions(_ context.Context, input mediaserver.CurrentSessionsInput) ([]mediaserver.StreamSession, error) {
	c.inputs = append(c.inputs, input)
	if c.err != nil {
		return nil, c.err
	}
	return c.sessions, nil
}

type fakeDNSClient struct {
	summary    network.DnsHoleSummary
	summaryErr error
	enableErr  error
	disableErr error
	lastDur    *network.DisableDurationSeconds
	enables    int
	disables   int
}

func (c *fakeDNSClient) GetSummary(_ context.Context) (network.DnsHoleSummary, error) {
	if c.summaryErr != nil {
		return network.DnsHoleSummary{}, c.summaryErr
	}
	return c.summary, nil
}

func (c *fakeDNSClient) Enable(_ context.Context) error {
	c.enables++
	return c.enableErr
}

func (c *fakeDNSClient) Disable(_ context.Context, duration *network.DisableDurationSeconds) error {
	c.disables++
	c.lastDur = duration
	return c.disableErr
}

type msHarness struct {
	srv      *httptest.Server
	resolver *fakeResolver
	ms       *fakeMSClient
	dns      *fakeDNSClient
	sessions map[string]*ctxauth.Session
}

func newMSHarness(t *testing.T, opts func(o *widgets.Options)) *msHarness {
	t.Helper()
	h := &msHarness{
		resolver: &fakeResolver{},
		ms:       &fakeMSClient{},
		dns:      &fakeDNSClient{},
		sessions: map[string]*ctxauth.Session{},
	}
	options := widgets.Options{
		ResolveIntegrations: h.resolver.resolve,
		NewMediaServer: func(widgets.Integration) (mediaserver.Client, error) {
			return h.ms, nil
		},
		NewNetwork: func(_ context.Context, _ widgets.Integration) (network.Client, error) {
			return h.dns, nil
		},
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

func (h *msHarness) seedUser(name string, admin bool) {
	h.sessions[name] = &ctxauth.Session{ID: "s-" + name, UserID: "u-" + name, Name: name, IsAdministrator: admin}
}

// swap replaces the server with one built from custom options (the shared
// fake clients cannot fail per integration).
func (h *msHarness) swap(t *testing.T, opts widgets.Options) {
	t.Helper()
	opts.ResolveIntegrations = h.resolver.resolve
	r := trpc.New()
	widgets.Register(r, opts)
	h.srv = httptest.NewServer(r.Handler())
	t.Cleanup(h.srv.Close)
}

// call performs one tRPC round trip (POST single).
func (h *msHarness) call(t *testing.T, user, path, input string) (int, string) {
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

func localStrPtr(v string) *string { return &v }

func msSessionFixture() mediaserver.StreamSession {
	position := 600000.0
	duration := 2700000.0
	state := "playing"
	seasonName := "Season 5"
	seasonNumber := 5.0
	episodeNumber := 14.0
	name := "Ozymandias"
	thumb := "https://img/user.jpg"
	return mediaserver.StreamSession{
		SessionID:   "sess-1",
		SessionName: "Plex Web (Chrome)",
		User: mediaserver.SessionUser{
			UserID:            "u-1",
			Username:          "john",
			ProfilePictureURL: &thumb,
		},
		CurrentlyPlaying: &mediaserver.CurrentlyPlaying{
			Type:          mediaserver.TypeVideo,
			Name:          "Breaking Bad",
			SeasonName:    &seasonName,
			SeasonNumber:  &seasonNumber,
			EpisodeName:   mediaserver.OfValue(name),
			EpisodeNumber: &episodeNumber,
			AlbumName:     nil,
			EpisodeCount:  mediaserver.NullValue(),
			Playback: mediaserver.Playback{
				State:      &state,
				PositionMs: &position,
				DurationMs: &duration,
			},
			Location: localStrPtr(mediaserver.LocationWAN),
			Metadata: &mediaserver.Metadata{
				Video: mediaserver.MetadataVideo{Resolution: &mediaserver.Resolution{Width: 1920, Height: 1080}},
				Audio: mediaserver.MetadataAudio{},
				Transcoding: mediaserver.Transcoding{
					Target:        mediaserver.Target{},
					IsVideoDirect: true,
					IsAudioDirect: true,
				},
			},
		},
	}
}

// TestMediaServerGetCurrentStreamsSuccessShape pins the success entry
// byte-for-byte (field names AND order, error absent).
func TestMediaServerGetCurrentStreamsSuccessShape(t *testing.T) {
	h := newMSHarness(t, nil)
	h.ms.sessions = []mediaserver.StreamSession{msSessionFixture()}
	h.resolver.rows = []widgets.Integration{
		{ID: "int-1", Name: "My Plex", Kind: "plex", URL: "http://plex"},
	}
	status, body := h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-1"],"showOnlyPlaying":true}`)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}
	data := unwrapData(t, body)
	want := `[{"integrationId":"int-1","integrationName":"My Plex","integrationKind":"plex","sessions":[{"sessionId":"sess-1","sessionName":"Plex Web (Chrome)","user":{"userId":"u-1","username":"john","profilePictureUrl":"https://img/user.jpg"},"currentlyPlaying":{"type":"video","name":"Breaking Bad","seasonName":"Season 5","seasonNumber":5,"episodeName":"Ozymandias","episodeNumber":14,"albumName":null,"episodeCount":null,"playback":{"state":"playing","positionMs":600000,"durationMs":2700000},"location":"wan","metadata":{"video":{"resolution":{"width":1920,"height":1080},"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":null}}}]}]`
	if string(data) != want {
		t.Fatalf("data mismatch:\n got: %s\nwant: %s", data, want)
	}
	if h.resolver.queries[0].Action != "query" || h.resolver.queries[0].WidgetKind != "mediaServer" {
		t.Fatalf("resolve request = %+v", h.resolver.queries[0])
	}
	if !h.ms.inputs[0].ShowOnlyPlaying {
		t.Fatal("showOnlyPlaying must reach the client")
	}
}

// TestMediaServerInputValidation pins the required boolean and the
// middleware-first parse order (a bad showOnlyPlaying with unknown ids
// answers NOT_FOUND).
func TestMediaServerInputValidation(t *testing.T) {
	h := newMSHarness(t, nil)
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "P", Kind: "plex"}}
	// Missing showOnlyPlaying -> BAD_REQUEST.
	_, body := h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-1"]}`)
	if code, _ := unwrapError(t, body); code != "BAD_REQUEST" {
		t.Fatalf("missing showOnlyPlaying code = %s", code)
	}
	// showOnlyPlaying of the wrong type -> BAD_REQUEST.
	_, body = h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-1"],"showOnlyPlaying":"yes"}`)
	if code, _ := unwrapError(t, body); code != "BAD_REQUEST" {
		t.Fatalf("wrong showOnlyPlaying code = %s", code)
	}
	// Unknown ids beat the missing boolean (middleware order): with a nil
	// resolver the NOT_FOUND comes before the showOnlyPlaying parse.
	h2 := newMSHarness(t, func(o *widgets.Options) { o.ResolveIntegrations = nil })
	_, body2 := h2.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["nope"]}`)
	code, message := unwrapError(t, body2)
	if code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
	want := "1 of the specified integrations not found or not of kinds jellyfin,emby,plex,navidrome,mock: ([nope] compared to [])"
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}
}

// TestMediaServerFallbackAndAllFailures pins the per-integration fallback
// and the throwOnAllFailures behavior.
func TestMediaServerFallbackAndAllFailures(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = -1
	})
	h.resolver.rows = []widgets.Integration{
		{ID: "int-ok", Name: "OK", Kind: "jellyfin"},
		{ID: "int-bad", Name: "Bad", Kind: "emby"},
	}
	h.swap(t, widgets.Options{
		NewMediaServer: func(row widgets.Integration) (mediaserver.Client, error) {
			if row.ID == "int-bad" {
				return &fakeMSClient{err: fmt.Errorf("boom")}, nil
			}
			return &fakeMSClient{sessions: []mediaserver.StreamSession{msSessionFixture()}}, nil
		},
		NewNetwork: func(context.Context, widgets.Integration) (network.Client, error) {
			return h.dns, nil
		},
		MediaServerCacheTTLMS: -1,
		DNSHoleCacheTTLMS:     -1,
	})

	_, body := h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-ok","int-bad"],"showOnlyPlaying":false}`)
	data := unwrapData(t, body)
	if !strings.Contains(string(data), `"error":"INTEGRATION_REQUEST_FAILED"`) ||
		!strings.Contains(string(data), `"integrationKind":"emby"`) ||
		!strings.Contains(string(data), `"integrationKind":"jellyfin"`) {
		t.Fatalf("entries = %s", data)
	}

	// All failures -> INTERNAL_SERVER_ERROR "All integration queries failed".
	h.swap(t, widgets.Options{
		NewMediaServer: func(widgets.Integration) (mediaserver.Client, error) {
			return &fakeMSClient{err: fmt.Errorf("boom")}, nil
		},
		NewNetwork: func(context.Context, widgets.Integration) (network.Client, error) {
			return h.dns, nil
		},
		MediaServerCacheTTLMS: -1,
		DNSHoleCacheTTLMS:     -1,
	})
	_, body = h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-bad"],"showOnlyPlaying":false}`)
	code, message := unwrapError(t, body)
	if code != "INTERNAL_SERVER_ERROR" || message != "All integration queries failed" {
		t.Fatalf("error = %s %q", code, message)
	}
}

// TestMediaServerEmptyIntegrationIDs pins [] -> [].
func TestMediaServerEmptyIntegrationIDs(t *testing.T) {
	h := newMSHarness(t, nil)
	_, body := h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":[],"showOnlyPlaying":false}`)
	if data := string(unwrapData(t, body)); data != "[]" {
		t.Fatalf("data = %s, want []", data)
	}
}

// TestMediaServerCacheTTL pins the 10s cache with an override and the input
// sensitivity (showOnlyPlaying is part of the key).
func TestMediaServerCacheTTL(t *testing.T) {
	builds := 0
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = 30
		o.DNSHoleCacheTTLMS = -1
		o.NewMediaServer = func(widgets.Integration) (mediaserver.Client, error) {
			builds++
			return &fakeMSClient{sessions: []mediaserver.StreamSession{msSessionFixture()}}, nil
		}
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-1", Name: "P", Kind: "plex"}}
	for i := 0; i < 3; i++ {
		h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-1"],"showOnlyPlaying":true}`)
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1 (single-flight within the TTL)", builds)
	}
	h.call(t, "", "/api/trpc/widget.mediaServer.getCurrentStreams", `{"integrationIds":["int-1"],"showOnlyPlaying":false}`)
	if builds != 2 {
		t.Fatalf("builds after new input = %d, want 2", builds)
	}
}

// --- dns hole ---

func dnsSummaryFixture() network.DnsHoleSummary {
	status := "enabled"
	return network.DnsHoleSummary{
		Status:                    &status,
		DomainsBeingBlocked:       100001,
		AdsBlockedToday:           205,
		AdsBlockedTodayPercentage: 4.1,
		DNSQueriesToday:           5023,
	}
}

// TestDNSHoleSummarySuccessShape pins the success entry (updatedAt ISO-ms in
// the integration info, summary object).
func TestDNSHoleSummarySuccessShape(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
	})
	h.dns.summary = dnsSummaryFixture()
	h.resolver.rows = []widgets.Integration{
		{ID: "int-p", Name: "My Pi-hole", Kind: "piHole", URL: "http://pihole"},
	}
	_, body := h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-p"]}`)
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
		Summary *network.DnsHoleSummary `json:"summary"`
		Error   *string                 `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("decode: %v (%s)", err, data)
	}
	if len(parsed) != 1 {
		t.Fatalf("entries = %d", len(parsed))
	}
	entry := parsed[0]
	if entry.IntegrationID != "int-p" || entry.Integration.Kind != "piHole" ||
		entry.Integration.Name != "My Pi-hole" {
		t.Fatalf("entry = %+v", entry)
	}
	if !strings.HasSuffix(entry.Integration.UpdatedAt, "Z") || !strings.Contains(entry.Integration.UpdatedAt, ".") {
		t.Fatalf("updatedAt = %q, want ISO with millis and Z", entry.Integration.UpdatedAt)
	}
	if entry.Summary == nil || entry.Error != nil || entry.Summary.DNSQueriesToday != 5023 {
		t.Fatalf("entry = %+v", entry)
	}
	if h.resolver.queries[0].WidgetKind != "dnsHoleSummary" {
		t.Fatalf("widget kind = %s", h.resolver.queries[0].WidgetKind)
	}
}

// TestDNSHoleSummaryFailureShape pins the failure entry (summary null, error
// constant, no updatedAt) and the throwOnAllFailures behavior.
func TestDNSHoleSummaryFailureShape(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = -1
	})
	h.resolver.rows = []widgets.Integration{
		{ID: "int-ok", Name: "OK", Kind: "adGuardHome"},
		{ID: "int-bad", Name: "Bad", Kind: "piHole"},
	}
	h.swap(t, widgets.Options{
		NewMediaServer: func(widgets.Integration) (mediaserver.Client, error) {
			return h.ms, nil
		},
		NewNetwork: func(_ context.Context, row widgets.Integration) (network.Client, error) {
			if row.ID == "int-bad" {
				return &fakeDNSClient{summaryErr: fmt.Errorf("down")}, nil
			}
			return &fakeDNSClient{summary: dnsSummaryFixture()}, nil
		},
		MediaServerCacheTTLMS: -1,
		DNSHoleCacheTTLMS:     -1,
	})

	_, body := h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-ok","int-bad"]}`)
	data := unwrapData(t, body)
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("entries = %s", data)
	}
	want := `{"integrationId":"int-bad","integrationName":"Bad","integration":{"id":"int-bad","name":"Bad","kind":"piHole"},"summary":null,"error":"INTEGRATION_REQUEST_FAILED"}`
	if string(entries[1]) != want {
		t.Fatalf("failure entry mismatch:\n got: %s\nwant: %s", entries[1], want)
	}

	// All failures throw.
	h.swap(t, widgets.Options{
		NewNetwork: func(context.Context, widgets.Integration) (network.Client, error) {
			return &fakeDNSClient{summaryErr: fmt.Errorf("down")}, nil
		},
		MediaServerCacheTTLMS: -1,
		DNSHoleCacheTTLMS:     -1,
	})
	_, body = h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-bad"]}`)
	if code, message := unwrapError(t, body); code != "INTERNAL_SERVER_ERROR" || message != "All integration queries failed" {
		t.Fatalf("error = %s %q", code, message)
	}
}

func TestDNSHoleMutationsRequireSession(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = -1
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-p", Name: "P", Kind: "piHole"}}
	for _, path := range []string{"enable", "disable"} {
		_, body := h.call(t, "", "/api/trpc/widget.dnsHole."+path, `{"integrationId":"int-p"}`)
		if code, message := unwrapError(t, body); code != "UNAUTHORIZED" || message != "UNAUTHORIZED" {
			t.Fatalf("%s anonymous: %s %q", path, code, message)
		}
	}
	h.seedUser("ro", false)
	h.sessions["ro"].ReadOnly = true
	_, body := h.call(t, "ro", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"int-p"}`)
	if code, message := unwrapError(t, body); code != "FORBIDDEN" || message != "This credential only permits read-only operations" {
		t.Fatalf("read-only: %s %q", code, message)
	}
}

// TestDNSHoleMutations pins the ONE-integration contract: integrationId
// input, interact action with the dnsHoleControls kind and cardinality one,
// duration forwarding and the exact NOT_FOUND message of the one middleware.
func TestDNSHoleMutations(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = 30
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-p", Name: "P", Kind: "piHole"}}
	h.seedUser("alice", false)

	if _, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"int-p"}`); strings.Contains(body, `"error"`) {
		t.Fatalf("enable body = %s", body)
	}
	if h.dns.enables != 1 || h.dns.disables != 0 {
		t.Fatalf("mutations = enable %d disable %d", h.dns.enables, h.dns.disables)
	}
	req := h.resolver.queries[0]
	if req.Action != "interact" || req.WidgetKind != "dnsHoleControls" || req.Cardinality != "one" ||
		len(req.IntegrationIDs) != 1 || req.IntegrationIDs[0] != "int-p" {
		t.Fatalf("resolve request = %+v", req)
	}

	// disable forwards the optional duration (seconds).
	seconds := `{"integrationId":"int-p","duration":300}`
	if _, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.disable", seconds); strings.Contains(body, `"error"`) {
		t.Fatalf("disable body = %s", body)
	}
	if h.dns.lastDur == nil || *h.dns.lastDur != 300 {
		t.Fatalf("duration = %v, want 300", h.dns.lastDur)
	}
	// disable without duration -> nil pointer.
	if _, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.disable", `{"integrationId":"int-p"}`); strings.Contains(body, `"error"`) {
		t.Fatalf("disable body = %s", body)
	}
	if h.dns.lastDur != nil {
		t.Fatalf("duration = %v, want nil (permanent)", h.dns.lastDur)
	}
	// duration of the wrong type is a BAD_REQUEST.
	_, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.disable", `{"integrationId":"int-p","duration":"300"}`)
	if code, _ := unwrapError(t, body); code != "BAD_REQUEST" {
		t.Fatalf("duration code = %s", code)
	}
}

// TestDNSHoleOneNotFoundMessage pins the exact upstream NOT_FOUND message of
// the ONE-integration middleware (different from the many variant).
func TestDNSHoleOneNotFoundMessage(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) { o.ResolveIntegrations = nil })
	h.seedUser("alice", false)
	_, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"nope"}`)
	code, message := unwrapError(t, body)
	if code != "NOT_FOUND" {
		t.Fatalf("code = %s", code)
	}
	want := "Integration with id nope not found or not of kinds piHole,adGuardHome,technitiumDns,mock"
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}
}

// TestDNSHoleMutationFailureSurfacesExactMessage pins the wire message of a
// failed mutation (the raw client error, INTERNAL_SERVER_ERROR).
func TestDNSHoleMutationFailureSurfacesExactMessage(t *testing.T) {
	h := newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = -1
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-p", Name: "Pi-hole", Kind: "piHole"}}
	h.seedUser("alice", false)
	h.dns.enableErr = fmt.Errorf("Failed to enable PiHole for Pi-hole (int-p): Internal Server Error")
	_, body := h.call(t, "alice", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"int-p"}`)
	code, message := unwrapError(t, body)
	if code != "INTERNAL_SERVER_ERROR" ||
		message != "Failed to enable PiHole for Pi-hole (int-p): Internal Server Error" {
		t.Fatalf("error = %s %q", code, message)
	}
}

// TestDNSHoleMutationInvalidation pins the invalidation timing: it runs only
// AFTER a successful mutation (upstream invalidateCacheAsync follows the
// client call), and a mid-TTL mutation forces the next summary to rebuild.
func TestDNSHoleMutationInvalidation(t *testing.T) {
	builds := 0
	var h *msHarness
	h = newMSHarness(t, func(o *widgets.Options) {
		o.MediaServerCacheTTLMS = -1
		o.DNSHoleCacheTTLMS = 30
		o.NewNetwork = func(_ context.Context, _ widgets.Integration) (network.Client, error) {
			builds++
			return h.dns, nil
		}
	})
	h.resolver.rows = []widgets.Integration{{ID: "int-p", Name: "P", Kind: "piHole"}}
	h.seedUser("alice", false)
	h.dns.summary = dnsSummaryFixture()

	for i := 0; i < 2; i++ {
		h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-p"]}`)
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
	// A failed mutation does NOT invalidate (the throw precedes it).
	h.dns.enableErr = fmt.Errorf("boom")
	h.call(t, "alice", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"int-p"}`)
	h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-p"]}`)
	if builds != 2 { // 1 summary + 1 enable client build, summary still cached
		t.Fatalf("builds after failed mutation = %d, want 2", builds)
	}
	// A successful mutation invalidates.
	h.dns.enableErr = nil
	h.call(t, "alice", "/api/trpc/widget.dnsHole.enable", `{"integrationId":"int-p"}`)
	h.call(t, "", "/api/trpc/widget.dnsHole.summary", `{"integrationIds":["int-p"]}`)
	if builds != 4 { // + enable build + rebuilt summary
		t.Fatalf("builds after successful mutation = %d, want 4", builds)
	}
}
