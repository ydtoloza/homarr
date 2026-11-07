package plex_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver/plex"
)

// fakePlex answers the endpoints the client touches and records the last
// request for assertions.
type fakePlex struct {
	t          *testing.T
	lastPath   string
	lastQuery  map[string][]string
	lastHeader http.Header
}

func newFakePlex(t *testing.T, handler func(f *fakePlex, w http.ResponseWriter, r *http.Request)) (*fakePlex, *httptest.Server) {
	t.Helper()
	f := &fakePlex{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.Query()
		f.lastHeader = r.Header.Clone()
		handler(f, w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-1", Kind: "plex", Name: "Plex", URL: url, ExternalURL: ""}
}

// sessionXMLFixture covers the mapping corners: an episode with a transcode
// session and a track without one.
const sessionXMLFixture = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="2">
  <Video grandparentTitle="Breaking Bad" parentTitle="Season 5" parentIndex="5" title="Ozymandias" index="14" type="episode" viewOffset="600000" duration="2700000">
    <User id="1" title="john" thumb="/users/1.jpg"/>
    <Player product="Plex Web" title="Chrome" state="playing"/>
    <Session id="sess-1" bandwidth="4200" location="wan"/>
    <TranscodeSession videoDecision="transcode" audioDecision="copy" container="mpegts" audioCodec="aac" videoCodec="h264" width="1280" height="720"/>
    <Media container="mkv" bitrate="8000" audioCodec="dca">
      <Part container="matroska">
        <Stream streamType="2" codec="dca" channels="6"/>
        <Stream streamType="1" codec="h264" width="1920" height="1080" frameRate="23.976"/>
        <Stream streamType="3" codec="subrip"/>
      </Part>
    </Media>
  </Video>
  <Track parentTitle="Discovery" title="One More Time" index="1" type="track">
    <Player product="Plexamp" title="Phone" state="paused"/>
  </Track>
</MediaContainer>`

// TestCheckOnline pins testingAsync: GET /prefs with the token and the JSON
// accept header.
func TestCheckOnline(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefs" {
			f.t.Fatalf("check path = %q, want /prefs", r.URL.Path)
		}
		if got := r.Header.Get("X-Plex-Token"); got != "plex-token" {
			f.t.Fatalf("X-Plex-Token = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			f.t.Fatalf("Accept = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{APIKey: "plex-token"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("Check result = %+v", res)
	}
}

func TestCheckMissingSecretReportsUpstreamMessage(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		f.t.Fatal("no request may happen without the apiKey secret")
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != "No secret of kind apiKey was found" {
		t.Fatalf("error = %q", res.Error)
	}
}

func TestCheckTransportFailureIsRequestError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client := plex.New(testInput(url), plex.Secrets{APIKey: "plex-token"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check must not fail, got %v", err)
	}
	if res.Error != mediaserver.MsgRequest {
		t.Fatalf("error = %q, want the pinned request message", res.Error)
	}
}

// TestGetCurrentSessionsShape pins the query parameters, headers and the
// byte-exact session JSON of getCurrentSessionsAsync.
func TestGetCurrentSessionsShape(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status/sessions" {
			f.t.Fatalf("path = %q, want /status/sessions", r.URL.Path)
		}
		if got := r.Header.Get("X-Plex-Token"); got != "plex-token" {
			f.t.Fatalf("X-Plex-Token = %q", got)
		}
		_, _ = w.Write([]byte(sessionXMLFixture))
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{APIKey: "plex-token"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{ShowOnlyPlaying: true})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2 (no Player entries are dropped)", len(sessions))
	}
	raw, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// seasonName present on the episode, dropped on the track; episodeName
	// always present (null when not an episode); the copy audioDecision and
	// the part-vs-transcode container difference pin isStreamDirect and
	// containerChanged.
	want := `[{"sessionId":"sess-1","sessionName":"Plex Web (Chrome)","user":{"userId":"1","username":"john","profilePictureUrl":"/users/1.jpg"},"currentlyPlaying":{"type":"video","name":"Breaking Bad","seasonName":"Season 5","seasonNumber":5,"episodeName":"Ozymandias","episodeNumber":14,"albumName":null,"episodeCount":14,"playback":{"state":"playing","positionMs":600000,"durationMs":2700000},"location":"wan","metadata":{"video":{"resolution":{"width":1920,"height":1080},"frameRate":23.976},"audio":{"channelCount":6,"codec":"dca"},"transcoding":{"container":"mpegts","resolution":{"width":1280,"height":720},"target":{"audioCodec":"aac","videoCodec":"h264"},"isVideoDirect":false,"isAudioDirect":true,"containerChanged":true},"bitrateKbps":4200}}},` +
		`{"sessionId":"unknown","sessionName":"Plexamp (Phone)","user":{"userId":"Anonymous","username":"Anonymous","profilePictureUrl":null},"currentlyPlaying":{"type":"audio","name":"One More Time","seasonName":"Discovery","seasonNumber":null,"episodeName":null,"episodeNumber":null,"albumName":"Discovery","episodeCount":1,"playback":{"state":"paused","positionMs":null,"durationMs":null},"location":null,"metadata":{"video":{"resolution":null,"frameRate":null},"audio":{"channelCount":null,"codec":null},"transcoding":{"container":null,"resolution":null,"target":{"audioCodec":null,"videoCodec":null},"isVideoDirect":true,"isAudioDirect":true,"containerChanged":false},"bitrateKbps":null}}}]`
	if string(raw) != want {
		t.Fatalf("sessions JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetCurrentSessionsLiveIsTV pins the live="1" override of the type.
func TestGetCurrentSessionsLiveIsTV(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<MediaContainer><Video type="movie" live="1" title="News"><Player product="TV" title="Live"/></Video></MediaContainer>`))
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{APIKey: "k"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if sessions[0].CurrentlyPlaying.Type != mediaserver.TypeTV {
		t.Fatalf("type = %q, want tv (live override)", sessions[0].CurrentlyPlaying.Type)
	}
	if sessions[0].CurrentlyPlaying.Name != "News" {
		t.Fatalf("name = %q", sessions[0].CurrentlyPlaying.Name)
	}
}

// TestGetCurrentSessionsEmpty pins the "no sessions" case (empty array, not
// null).
func TestGetCurrentSessionsEmpty(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{APIKey: "k"})
	sessions, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err != nil {
		t.Fatalf("GetCurrentSessions: %v", err)
	}
	if sessions == nil || len(sessions) != 0 {
		t.Fatalf("sessions = %#v, want empty non-nil", sessions)
	}
}

// TestGetCurrentSessionsBadXML pins the parse error classification (the
// upstream ParseError message "Invalid xml format" is documented to stay
// local — the query replaces every failure with the public constant).
func TestGetCurrentSessionsBadXML(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<MediaContainer><Video`))
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{APIKey: "k"})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err == nil {
		t.Fatal("expected error")
	}
	de := mediaserver.AsError(err)
	if de.Kind != mediaserver.ErrKindParse {
		t.Fatalf("kind = %s, want parse", de.Kind)
	}
}

// TestGetCurrentSessionsMissingSecret pins the getSecretValue error (no
// request may happen without the token).
func TestGetCurrentSessionsMissingSecret(t *testing.T) {
	_, srv := newFakePlex(t, func(f *fakePlex, w http.ResponseWriter, r *http.Request) {
		f.t.Fatal("no request may happen without the apiKey secret")
	})
	client := plex.New(testInput(srv.URL), plex.Secrets{})
	_, err := client.GetCurrentSessions(context.Background(), mediaserver.CurrentSessionsInput{})
	if err == nil || err.Error() != "No secret of kind apiKey was found" {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("error = %v", err)
	}
}
