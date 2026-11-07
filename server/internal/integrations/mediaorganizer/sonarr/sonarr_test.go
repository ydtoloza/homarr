package sonarr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/sonarr"
)

// fakeSonarr answers the three endpoints the client touches and records the
// last request for assertions.
type fakeSonarr struct {
	t          *testing.T
	status     int // response status of the API calls, default 200
	lastPath   string
	lastQuery  map[string][]string
	lastHeader http.Header
}

func newFakeSonarr(t *testing.T, handler func(f *fakeSonarr, w http.ResponseWriter, r *http.Request)) (*fakeSonarr, *httptest.Server) {
	t.Helper()
	f := &fakeSonarr{t: t}
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
	return integrations.Input{ID: "int-1", Kind: "sonarr", Name: "Sonarr", URL: url, ExternalURL: ""}
}

func TestCheckOnline(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			f.t.Fatalf("check path = %q, want /api", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.Error != "" {
		t.Fatalf("Check result = %+v", res)
	}
	if got := res.DurationMs; got < 0 {
		t.Fatalf("durationMs = %v", got)
	}
}

func TestCheckUnauthorizedStatusIsDefinitive(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statusCode = %d", res.StatusCode)
	}
	if res.Error != mediaorganizer.MsgResponse {
		t.Fatalf("error = %q, want the pinned upstream response message", res.Error)
	}
}

func TestCheckMissingSecretReportsUpstreamMessage(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		f.t.Fatal("no request may happen without the apiKey secret")
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != "No secret of kind apiKey was found" {
		t.Fatalf("error = %q", res.Error)
	}
}

func TestCheckConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens anymore
	client := sonarr.New(testInput(url), sonarr.Secrets{APIKey: "key-1"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check must not fail, got %v", err)
	}
	if res.Error != mediaorganizer.MsgRequest {
		t.Fatalf("error = %q, want the pinned upstream request message", res.Error)
	}
}

func TestCheckSendsAPIKeyHeader(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "key-1" {
			f.t.Fatalf("X-Api-Key = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	if _, err := client.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// TestGetMissingShape pins the query parameters and the byte-exact item JSON
// of getMissingAsync (sonarr-integration.ts:192-229).
func TestGetMissingShapeAndQuery(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/wanted/missing":
			want := map[string]string{
				"page": "1", "pageSize": "10", "sortKey": "airDateUtc",
				"sortDirection": "ascending", "includeSeries": "true",
			}
			for k, v := range want {
				if got := r.URL.Query().Get(k); got != v {
					f.t.Fatalf("query %s = %q, want %q", k, got, v)
				}
			}
			if got := r.Header.Get("X-Api-Key"); got != "key-1" {
				f.t.Fatalf("X-Api-Key = %q", got)
			}
			_, _ = w.Write([]byte(`{"totalRecords": 42, "records": [
				{"id": 7, "title": "S02E05 - The Last of Us", "seasonNumber": 2, "episodeNumber": 5,
				 "series": {"id": 3, "title": "The Last of Us", "year": 2023, "titleSlug": "the-last-of-us",
				            "images": [{"coverType": "poster", "remoteUrl": "https://img/poster.jpg"},
				                       {"coverType": "banner", "remoteUrl": "https://img/banner.jpg"}]}},
				{"id": 8, "title": "Orphan episode", "seasonNumber": 1, "episodeNumber": 2}
			]}`))
		default:
			f.t.Fatalf("unexpected path %q", r.URL.Path)
		}
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	res, err := client.GetMissing(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMissing: %v", err)
	}
	if res.TotalCount != 42 {
		t.Fatalf("totalCount = %d", res.TotalCount)
	}
	// The second record has no series: poster null, link fallback, no
	// year/seriesTitle keys. externalUrl falls back to url (no externalUrl
	// configured).
	want := strings.ReplaceAll(
		`[{"id":7,"title":"S02E05 - The Last of Us","type":"episode","year":2023,"seasonNumber":2,"episodeNumber":5,"seriesTitle":"The Last of Us","imageUrl":"https://img/poster.jpg","link":"http://HOST/series/the-last-of-us"},`+
			`{"id":8,"title":"Orphan episode","type":"episode","seasonNumber":1,"episodeNumber":2,"imageUrl":null,"link":"http://HOST/wanted/missing"}]`,
		"http://HOST", srv.URL)
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetMediaQueueShape pins the queue mapping: percent from
// size/sizeleft, episode title override, nullish timeleft, fallback link.
func TestGetMediaQueueShape(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/queue" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		for k, want := range map[string]string{"page": "1", "pageSize": "5", "includeSeries": "true", "includeEpisode": "true"} {
			if got := r.URL.Query().Get(k); got != want {
				f.t.Fatalf("query %s = %q, want %q", k, got, want)
			}
		}
		_, _ = w.Write([]byte(`{"totalRecords": 1, "records": [
			{"id": 11, "title": "queued.item", "status": "downloading", "timeleft": "01:02:03",
			 "size": 2000, "sizeleft": 500,
			 "series": {"title": "Show", "titleSlug": "show", "images": [{"coverType": "poster", "remoteUrl": "https://img/p.jpg"}]},
			 "episode": {"title": "Pilot", "seasonNumber": 1, "episodeNumber": 1}},
			{"id": 12, "title": "no.episode", "status": "paused", "timeleft": null}
		]}`))
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	res, err := client.GetMediaQueue(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := strings.ReplaceAll(
		`[{"id":11,"title":"Pilot","type":"episode","status":"downloading","timeLeft":"01:02:03","percentComplete":75,"seasonNumber":1,"episodeNumber":1,"seriesTitle":"Show","imageUrl":"https://img/p.jpg","link":"http://HOST/series/show"},`+
			`{"id":12,"title":"no.episode","type":"episode","status":"paused","timeLeft":null,"percentComplete":0,"imageUrl":null,"link":"http://HOST/activity/queue"}]`,
		"http://HOST", srv.URL)
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetMediaQueueServerError pins the error classification: non-2xx is a
// response error with the pinned message.
func TestGetMediaQueueServerError(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	_, err := client.GetMediaQueue(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error")
	}
	de := mediaorganizer.AsError(err)
	if de.Kind != mediaorganizer.ErrKindResponse || de.Error() != mediaorganizer.MsgResponse {
		t.Fatalf("error = (%s, %v)", de.Kind, de.Error())
	}
}

// TestGetMissingBadJSON pins the parse error classification.
func TestGetMissingBadJSON(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not-json`))
	})
	client := sonarr.New(testInput(srv.URL), sonarr.Secrets{APIKey: "key-1"})
	_, err := client.GetMissing(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error")
	}
	if mediaorganizer.AsError(err).Error() != mediaorganizer.MsgParse {
		t.Fatalf("error = %v", err)
	}
}

// TestExternalURLOverride pins the externalUrl(path) link semantics
// (base/integration.ts:93-98).
func TestExternalURLOverride(t *testing.T) {
	_, srv := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"totalRecords": 0, "records": []}`))
	})
	input := testInput(srv.URL)
	input.ExternalURL = "https://tv.example.com/sonarr/"
	client := sonarr.New(input, sonarr.Secrets{APIKey: "key-1"})
	res, err := client.GetMissing(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetMissing: %v", err)
	}
	if res.TotalCount != 0 || len(res.Items) != 0 {
		t.Fatalf("unexpected items %+v", res.Items)
	}
	// The links fall back to the same host when no records exist; assert via
	// the queue fallback path instead.
	_, srv2 := newFakeSonarr(t, func(f *fakeSonarr, w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"totalRecords": 1, "records": [{"id": 1, "title": "t", "status": "s"}]}`))
	})
	input2 := testInput(srv2.URL)
	input2.ExternalURL = "https://tv.example.com/sonarr"
	client2 := sonarr.New(input2, sonarr.Secrets{APIKey: "key-1"})
	res2, err := client2.GetMediaQueue(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	if res2.Items[0].Link != "https://tv.example.com/sonarr/activity/queue" {
		t.Fatalf("link = %q", res2.Items[0].Link)
	}
}

// TestRegistryRegistration pins the self-registration: kind "sonarr" resolves
// through the default registry (factory without secrets reports the
// missing-secret error on Check).
func TestRegistryRegistration(t *testing.T) {
	def, err := integrations.DefinitionOf("sonarr")
	if err != nil {
		t.Fatalf("DefinitionOf: %v", err)
	}
	if def.Name != "Sonarr" {
		t.Fatalf("name = %q", def.Name)
	}
	inst, err := integrations.New("sonarr", testInput("http://localhost:1"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := inst.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != "No secret of kind apiKey was found" {
		t.Fatalf("error = %q", res.Error)
	}
}
