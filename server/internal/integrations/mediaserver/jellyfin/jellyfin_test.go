package jellyfin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver/jellyfin"
)

const authPrefix = `MediaBrowser Client="Homarr", Device="Homarr", DeviceId="homarr", Version="0.0.1"`

// fakeJellyfin answers the endpoints the client touches and records every
// request (method + path + headers + body) in order.
type fakeJellyfin struct {
	t        *testing.T
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

func newFakeJellyfin(t *testing.T, handler func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request)) (*fakeJellyfin, *httptest.Server) {
	t.Helper()
	f := &fakeJellyfin{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.Body != nil {
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			body = string(raw[:n])
		}
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
		})
		handler(f, w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-1", Kind: "jellyfin", Name: "Jellyfin", URL: url, ExternalURL: ""}
}

// TestCheckWithAPIKey pins the apiKey flow: GET /System/Ping with the SDK
// token header, no login.
func TestCheckWithAPIKey(t *testing.T) {
	_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Ping" {
			f.t.Fatalf("path = %q, want /System/Ping", r.URL.Path)
		}
		want := authPrefix + `, Token="key-1"`
		if got := r.Header.Get("Authorization"); got != want {
			f.t.Fatalf("Authorization = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte("Jellyfin Server"))
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("Check result = %+v", res)
	}
	if len(srv.URL) == 0 {
		t.Fatal("unreachable")
	}
}

// TestCheckWithCredentials authenticates first: POST /Users/AuthenticateByName
// (fixed prefix, JSON body) then the ping with the returned token.
func TestCheckWithCredentials(t *testing.T) {
	f, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			want := authPrefix
			if got := r.Header.Get("Authorization"); got != want {
				f.t.Fatalf("login Authorization = %q, want %q", got, want)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				f.t.Fatalf("login Content-Type = %q", got)
			}
			if f.requests[len(f.requests)-1].Body != `{"Username":"john","Pw":"secret"}` {
				f.t.Fatalf("login body = %q", f.requests[len(f.requests)-1].Body)
			}
			_, _ = w.Write([]byte(`{"AccessToken":"tok-1"}`))
		case "/System/Ping":
			want := authPrefix + `, Token="tok-1"`
			if got := r.Header.Get("Authorization"); got != want {
				f.t.Fatalf("ping Authorization = %q, want %q", got, want)
			}
			_, _ = w.Write([]byte("Jellyfin Server"))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{Username: "john", Password: "secret"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Check result = %+v", res)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want login + ping", len(f.requests))
	}
}

// TestGetCurrentSessionsShapeAndFilters pins the query, the filters and the
// byte-exact session JSON (one episode with transcode metadata, the homarr
// device filtered, a UserId-less entry filtered).
func TestGetCurrentSessionsShapeAndFilters(t *testing.T) {
	sessionsJSON := `[{"Id":"sess-1","UserId":"u-1","Client":"Jellyfin Web","DeviceName":"Firefox","UserName":"john","DeviceId":"firefox-1","RemoteEndPoint":"192.168.1.50:51234","NowPlayingItem":{"Type":"Episode","SeriesName":"Breaking Bad","Name":"IgnoredName","SeasonName":"Season 5","ParentIndexNumber":5,"IndexNumber":14,"RunTimeTicks":27000000000,"Album":"ignored","Width":1920,"Height":1080,"MediaStreams":[{"BitRate":4200000},{"BitRate":180000}],"MediaSources":[{"Bitrate":0}]},"PlayState":{"IsPaused":false,"PositionTicks":600000000,"PlayMethod":"DirectStream"},"TranscodingInfo":null},` +
		`{"Id":"sess-2","UserId":"u-2","Client":"Homarr","DeviceName":"Homarr","DeviceId":"homarr"},` +
		`{"Id":"sess-3","Client":"NoUser","DeviceName":"X"},` +
		`{"Id":"sess-4","UserId":"u-3","Client":"Infuse","DeviceName":"iPad","DeviceId":"pad-1","RemoteEndPoint":"203.0.113.9","NowPlayingItem":{"Type":"Movie","Name":"Interstellar","RunTimeTicks":9000000000,"MediaSources":[{"Bitrate":25000}]}}]`
	_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Sessions" {
			f.t.Fatalf("path = %q, want /Sessions", r.URL.Path)
		}
		want := authPrefix + `, Token="key-1"`
		if got := r.Header.Get("Authorization"); got != want {
			f.t.Fatalf("Authorization = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(sessionsJSON))
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{APIKey: "key-1"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2 (homarr device and UserId-less filtered)", len(sessions))
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Pin the mapping corners: ticks -> ms (Math.round), bitrateKbps (the 0
	// MediaSources bitrate is kept by ??, NOT replaced by the stream sum),
	// lan classification, containerChanged via DirectStream, season/album
	// coalescing with "" and the dropped episodeCount of the movie.
	want := `[{"sessionId":"sess-1","sessionName":"Jellyfin Web (Firefox)","user":{"userId":"u-1","username":"john","profilePictureUrl":"http://HOST/Users/u-1/Images/Primary"},"currentlyPlaying":{"type":"video","name":"Breaking Bad","seasonName":"Season 5","seasonNumber":5,"episodeNumber":14,"albumName":"ignored","playback":{"state":"playing","positionMs":60000,"durationMs":2700000},"location":"lan","metadata":{"video":{"resolution":{"width":1920,"height":1080},"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":true},"bitrateKbps":0}}},` +
		`{"sessionId":"sess-4","sessionName":"Infuse (iPad)","user":{"userId":"u-3","username":"","profilePictureUrl":"http://HOST/Users/u-3/Images/Primary"},"currentlyPlaying":{"type":"movie","name":"Interstellar","seasonName":"","seasonNumber":null,"episodeName":null,"episodeNumber":null,"albumName":"","playback":{"state":"playing","positionMs":null,"durationMs":900000},"location":"wan","metadata":{"video":{"resolution":null,"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":25}}}]`
	want = strings.ReplaceAll(want, "http://HOST", srv.URL)
	if string(raw) != want {
		t.Fatalf("sessions JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetCurrentSessionsShowOnlyPlaying pins the filter behavior.
func TestGetCurrentSessionsShowOnlyPlaying(t *testing.T) {
	sessionsJSON := `[{"Id":"a","UserId":"u-1","NowPlayingItem":{"Type":"Movie","Name":"X"}},{"Id":"b","UserId":"u-2"}]`
	_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sessionsJSON))
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{APIKey: "k"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{ShowOnlyPlaying: true})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "a" {
		t.Fatalf("sessions = %+v, want only the playing one", sessions)
	}
}

// TestGetCurrentSessionsPaused pins the paused state and the tri-state
// episodeName (absent EpisodeTitle drops the member; null emits null).
func TestGetCurrentSessionsPausedAndTriState(t *testing.T) {
	sessionsJSON := `[{"Id":"a","UserId":"u-1","PlayState":{"IsPaused":true},"NowPlayingItem":{"Type":"Episode","Name":"E","EpisodeTitle":null}},{"Id":"b","UserId":"u-2","NowPlayingItem":{"Type":"Episode","Name":"E2"}}]`
	_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sessionsJSON))
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{APIKey: "k"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// a: episodeName null (explicit); b: episodeName dropped (undefined).
	want := `[{"sessionId":"a","sessionName":" ()","user":{"userId":"u-1","username":"","profilePictureUrl":"http://HOST/Users/u-1/Images/Primary"},"currentlyPlaying":{"type":"video","name":"E","seasonName":"","seasonNumber":null,"episodeName":null,"episodeNumber":null,"albumName":"","playback":{"state":"paused","positionMs":null,"durationMs":null},"location":null,"metadata":{"video":{"resolution":null,"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":null}}},` +
		`{"sessionId":"b","sessionName":" ()","user":{"userId":"u-2","username":"","profilePictureUrl":"http://HOST/Users/u-2/Images/Primary"},"currentlyPlaying":{"type":"video","name":"E2","seasonName":"","seasonNumber":null,"episodeNumber":null,"albumName":"","playback":{"state":"playing","positionMs":null,"durationMs":null},"location":null,"metadata":{"video":{"resolution":null,"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":null}}}]`
	want = strings.ReplaceAll(want, "http://HOST", srv.URL)
	if string(raw) != want {
		t.Fatalf("sessions JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestSessionsTransportErrorClassified pins the axios taxonomy of /Sessions.
func TestSessionsTransportErrorClassified(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_ = srv
	client := jellyfin.New(testInput(url), jellyfin.Secrets{APIKey: "k"})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	de := mediaserver.AsError(err)
	if de.Kind != mediaserver.ErrKindRequest {
		t.Fatalf("kind = %s, want request", de.Kind)
	}
}

// TestSessionsBadStatus pins the response classification.
func TestSessionsBadStatus(t *testing.T) {
	_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{APIKey: "k"})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	de := mediaserver.AsError(err)
	if de.Kind != mediaserver.ErrKindResponse {
		t.Fatalf("kind = %s, want response", de.Kind)
	}
}

// TestLoginFailuresCollapseToPinnedMessage pins the query-path auth wrapper:
// every login failure is the plain "Jellyfin authentication failed" error.
func TestLoginFailuresCollapseToPinnedMessage(t *testing.T) {
	t.Run("bad credentials", func(t *testing.T) {
		_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{Username: "john", Password: "wrong"})
		_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
		de := mediaserver.AsError(err)
		if de.Kind != mediaserver.ErrKindUnknown || de.Error() != "An unknown error occured while executing Integration method" {
			t.Fatalf("error = (%s, %s)", de.Kind, de.Error())
		}
	})
	t.Run("missing accessToken", func(t *testing.T) {
		_, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{Username: "john", Password: "x"})
		_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
		if mediaserver.AsError(err).Kind != mediaserver.ErrKindUnknown {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing secret", func(t *testing.T) {
		_, _ = newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
			f.t.Fatal("no request may happen without secrets")
		})
		if _, err := jellyfin.SecretsFromMap(map[string]string{}); err == nil || err.Error() != "No secret of kind username was found" {
			t.Fatalf("SecretsFromMap error = %v", err)
		}
	})
}

// TestSecretsFromMapPicksEitherSet pins the alternative secret sets
// ([["username","password"],["apiKey"]]).
func TestSecretsFromMapPicksEitherSet(t *testing.T) {
	api, err := jellyfin.SecretsFromMap(map[string]string{"apiKey": "k"})
	if err != nil || api.APIKey != "k" {
		t.Fatalf("apiKey set: %+v %v", api, err)
	}
	user, err := jellyfin.SecretsFromMap(map[string]string{"username": "u", "password": "p"})
	if err != nil || user.Username != "u" || user.Password != "p" {
		t.Fatalf("user set: %+v %v", user, err)
	}
}

// TestConcurrentLoginsShareOneRequest pins the in-flight dedupe of
// getAccessTokenAsync (pendingAuthentications): parallel sessions log in
// ONCE.
func TestConcurrentLoginsShareOneRequest(t *testing.T) {
	f, srv := newFakeJellyfin(t, func(f *fakeJellyfin, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"tok-1"}`))
		case "/Sessions":
			_, _ = w.Write([]byte(`[]`))
		}
	})
	client := jellyfin.New(testInput(srv.URL), jellyfin.Secrets{Username: "john", Password: "secret"})
	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("GetCurrentSessions: %v", err)
		}
	}
	logins := 0
	for _, req := range f.requests {
		if req.Path == "/Users/AuthenticateByName" {
			logins++
		}
	}
	if logins != 1 {
		t.Fatalf("logins = %d, want 1 (in-flight dedupe)", logins)
	}
}

// TestParseLocation mirrors the upstream spec cases
// (jellyfin-integration.spec.ts:23-66).
func TestParseLocation(t *testing.T) {
	cases := []struct {
		in   string
		want *string
	}{
		{"", nil},
		{"example.com", nil},
		{"192.168.1.100:54321", lanPtr()},
		{"8.8.8.8", wanPtr()},
		{"[::1]:8096", lanPtr()},
		{"::1", lanPtr()},
		{"fe80::5", lanPtr()},
		{"2001:db8::1", wanPtr()},
		{"::ffff:192.168.1.5", lanPtr()},
		{"::ffff:8.8.8.8", wanPtr()},
	}
	for _, c := range cases {
		if got := jellyfin.ParseLocation(c.in); !sameStringPtr(got, c.want) {
			t.Fatalf("ParseLocation(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func lanPtr() *string { return strPtr("lan") }
func wanPtr() *string { return strPtr("wan") }

func strPtr(s string) *string { return &s }

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
