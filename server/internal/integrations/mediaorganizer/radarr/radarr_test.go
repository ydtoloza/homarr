package radarr_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/radarr"
)

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-r", Kind: "radarr", Name: "Radarr", URL: url}
}

func TestCheckOnlineAndHeader(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			t.Fatalf("check path = %q, want /api", r.URL.Path)
		}
		gotKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || gotKey != "rkey" {
		t.Fatalf("result = %+v, key = %q", res, gotKey)
	}
}

func TestCheckNon2xxIsDefinitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusForbidden || res.Error != mediaorganizer.MsgResponse {
		t.Fatalf("result = %+v", res)
	}
}

// TestGetMissingShape pins radarr's own query (sortKey=title, no
// includeSeries) and the movie item JSON (radarr-integration.ts:150-181).
func TestGetMissingShape(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/wanted/missing" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		query = r.URL.Query()
		_, _ = w.Write([]byte(`{"totalRecords": 3, "records": [
			{"id": 1, "title": "Dune", "year": 2021, "titleSlug": "dune-2021",
			 "images": [{"coverType": "fanart", "remoteUrl": "https://img/fanart.jpg"},
			            {"coverType": "poster", "remoteUrl": "https://img/poster.jpg"}]},
			{"id": 2, "title": "No Images", "titleSlug": "no-images"}
		]}`))
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	res, err := client.GetMissing(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMissing: %v", err)
	}
	for k, want := range map[string]string{"page": "1", "pageSize": "10", "sortKey": "title", "sortDirection": "ascending"} {
		if got := query.Get(k); got != want {
			t.Fatalf("query %s = %q, want %q", k, got, want)
		}
	}
	if query.Has("includeSeries") {
		t.Fatalf("radarr must not send includeSeries, got %q", query.Get("includeSeries"))
	}
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"id":1,"title":"Dune","type":"movie","year":2021,"imageUrl":"https://img/poster.jpg","link":"` + srv.URL + `/movie/dune-2021"},` +
		`{"id":2,"title":"No Images","type":"movie","imageUrl":null,"link":"` + srv.URL + `/movie/no-images"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
	if res.TotalCount != 3 {
		t.Fatalf("totalCount = %d", res.TotalCount)
	}
}

// TestGetMediaQueueShape pins includeMovie=true, the movie title/year
// override and the /activity/queue fallback (radarr-integration.ts:183-221).
func TestGetMediaQueueShape(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/queue" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		query = r.URL.Query()
		_, _ = w.Write([]byte(`{"totalRecords": 2, "records": [
			{"id": 5, "title": "movie.file", "status": "downloading", "timeleft": "00:10:00",
			 "size": 1000, "sizeleft": 250,
			 "movie": {"title": "Dune", "year": 2021, "titleSlug": "dune-2021",
			           "images": [{"coverType": "poster", "remoteUrl": "https://img/p.jpg"}]}},
			{"id": 6, "title": "unknown.file", "status": "queued", "timeleft": null}
		]}`))
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	res, err := client.GetMediaQueue(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	if got := query.Get("includeMovie"); got != "true" {
		t.Fatalf("includeMovie = %q", got)
	}
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"id":5,"title":"Dune","type":"movie","status":"downloading","timeLeft":"00:10:00","percentComplete":75,"year":2021,"imageUrl":"https://img/p.jpg","link":"` + srv.URL + `/movie/dune-2021"},` +
		`{"id":6,"title":"unknown.file","type":"movie","status":"queued","timeLeft":null,"percentComplete":0,"imageUrl":null,"link":"` + srv.URL + `/activity/queue"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestPercentCompleteRounding pins Math.round semantics on the fraction.
func TestPercentCompleteRounding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"totalRecords": 1, "records": [
			{"id": 9, "title": "t", "status": "downloading", "size": 3, "sizeleft": 1}
		]}`))
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	res, err := client.GetMediaQueue(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	// (3-1)/3 = 66.666.. -> Math.round -> 67
	if res.Items[0].PercentComplete != 67 {
		t.Fatalf("percentComplete = %d, want 67", res.Items[0].PercentComplete)
	}
}

func TestMissingSecret(t *testing.T) {
	client := radarr.New(testInput("http://localhost:1"), radarr.Secrets{})
	_, err := client.GetMissing(context.Background(), 10)
	if err == nil || err.Error() != "No secret of kind apiKey was found" {
		t.Fatalf("error = %v", err)
	}
}

func TestQueueServerErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	client := radarr.New(testInput(srv.URL), radarr.Secrets{APIKey: "rkey"})
	_, err := client.GetMediaQueue(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error")
	}
	de := mediaorganizer.AsError(err)
	if de.Kind != mediaorganizer.ErrKindResponse || !strings.Contains(de.Error(), "did not indicate success") {
		t.Fatalf("error = %v (%s)", err, de.Kind)
	}
}
