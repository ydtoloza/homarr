package emby_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver/emby"
)

const authHeader = `Emby Client="Dashboard", Device="Homarr", DeviceId="homarr-emby-integration", Version="0.0.1"`

// fakeEmby answers the endpoints the client touches and records the last
// request.
type fakeEmby struct {
	t          *testing.T
	lastPath   string
	lastHeader http.Header
}

func newFakeEmby(t *testing.T, handler func(f *fakeEmby, w http.ResponseWriter, r *http.Request)) (*fakeEmby, *httptest.Server) {
	t.Helper()
	f := &fakeEmby{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastHeader = r.Header.Clone()
		handler(f, w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-1", Kind: "emby", Name: "Emby", URL: url, ExternalURL: ""}
}

func assertEmbyHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("X-Emby-Token"); got != "key-1" {
		t.Fatalf("X-Emby-Token = %q", got)
	}
	if got := header.Get("Authorization"); got != authHeader {
		t.Fatalf("Authorization = %q, want %q", got, authHeader)
	}
}

// TestCheckOnline pins testingAsync: GET /emby/System/Ping with both headers.
func TestCheckOnline(t *testing.T) {
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/System/Ping" {
			f.t.Fatalf("check path = %q, want /emby/System/Ping", r.URL.Path)
		}
		assertEmbyHeaders(t, r.Header)
		w.WriteHeader(http.StatusOK)
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("Check result = %+v", res)
	}
}

func TestCheckNon2xxIsDefinitive(t *testing.T) {
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusForbidden || res.Error != mediaserver.MsgResponse {
		t.Fatalf("Check result = %+v", res)
	}
}

// TestGetCurrentSessionsShape pins the mapping and the tri-state corners of
// the zod session schema (absent EpisodeTitle drops the member, null keeps
// null).
func TestGetCurrentSessionsShape(t *testing.T) {
	sessionsJSON := `[{"Id":"sess-1","UserId":"u-1","Client":"Emby for Kodi","DeviceName":"Kodi","UserName":"anna","DeviceId":"kodi-1","RemoteEndPoint":"172.16.3.9:40000","NowPlayingItem":{"Type":"Episode","SeriesName":"Severance","SeasonName":"Season 2","ParentIndexNumber":2,"IndexNumber":10,"EpisodeTitle":null,"RunTimeTicks":36000000000,"Width":3840,"Height":2160,"MediaStreams":[],"TranscodingInfo":null},"PlayState":{"IsPaused":true,"PositionTicks":120000000,"PlayMethod":"Transcode"},"TranscodingInfo":{"Bitrate":8000,"Framerate":60,"AudioChannels":2,"AudioCodec":"aac","VideoCodec":"h264","Container":"mpegts","IsVideoDirect":false,"IsAudioDirect":true,"Width":1920,"Height":1080}},` +
		`{"Id":"sess-2","UserId":"u-2","Client":"Homarr","DeviceName":"Homarr","DeviceId":"homarr-emby-integration"},` +
		`{"Id":"sess-3","UserId":"u-3","Client":"Web","DeviceName":"Chrome","RemoteEndPoint":"example.com","NowPlayingItem":{"Type":"Series","Name":"Dark"}}]`
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Sessions" {
			f.t.Fatalf("path = %q, want /emby/Sessions", r.URL.Path)
		}
		assertEmbyHeaders(t, r.Header)
		_, _ = w.Write([]byte(sessionsJSON))
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{APIKey: "key-1"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2 (homarr device filtered)", len(sessions))
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Pins: zod-nullish EpisodeTitle null -> episodeName null; unplayable
	// series entry -> currentlyPlaying null; percentage lan/wan via the
	// shared classifier ("example.com" is not an IP -> null); the transcoding
	// member of the schema.
	want := `[{"sessionId":"sess-1","sessionName":"Emby for Kodi (Kodi)","user":{"userId":"u-1","username":"anna","profilePictureUrl":"http://HOST/Users/u-1/Images/Primary"},"currentlyPlaying":{"type":"video","name":"Severance","seasonName":"Season 2","seasonNumber":2,"episodeName":null,"episodeNumber":10,"albumName":"","playback":{"state":"paused","positionMs":12000,"durationMs":3600000},"location":"lan","metadata":{"video":{"resolution":{"width":3840,"height":2160},"frameRate":60},"audio":{"channelCount":2,"codec":"aac"},"transcoding":{"container":"mpegts","resolution":{"width":1920,"height":1080},"target":{"audioCodec":"aac","videoCodec":"h264"},"isVideoDirect":false,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":8}}},` +
		`{"sessionId":"sess-3","sessionName":"Web (Chrome)","user":{"userId":"u-3","username":"","profilePictureUrl":"http://HOST/Users/u-3/Images/Primary"},"currentlyPlaying":{"type":"tv","name":"Dark","seasonName":"","seasonNumber":null,"episodeName":null,"episodeNumber":null,"albumName":"","playback":{"state":"playing","positionMs":null,"durationMs":null},"location":null,"metadata":{"video":{"resolution":null,"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":null}}}]`
	want = strings.ReplaceAll(want, "http://HOST", srv.URL)
	if string(raw) != want {
		t.Fatalf("sessions JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetCurrentSessionsBadStatus pins the plain upstream error message
// (unknown taxonomy, exact wording).
func TestGetCurrentSessionsBadStatus(t *testing.T) {
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{APIKey: "key-1"})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err == nil || err.Error() != "Emby server int-1 returned a non successful status code: 500" {
		t.Fatalf("error = %v", err)
	}
}

// TestGetCurrentSessionsBadJSON pins the parse message prefix.
func TestGetCurrentSessionsBadJSON(t *testing.T) {
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"not": "an array"`))
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{APIKey: "key-1"})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err == nil || !strings.HasPrefix(err.Error(), "Emby server int-1 returned an unexpected response:") {
		t.Fatalf("error = %v", err)
	}
}

// TestGetCurrentSessionsMissingSecret pins the missing apiKey error.
func TestGetCurrentSessionsMissingSecret(t *testing.T) {
	_, srv := newFakeEmby(t, func(f *fakeEmby, w http.ResponseWriter, r *http.Request) {
		f.t.Fatal("no request may happen without the apiKey secret")
	})
	client := emby.New(testInput(srv.URL), emby.Secrets{})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err == nil || err.Error() != "No secret of kind apiKey was found" {
		t.Fatalf("error = %v", err)
	}
}
