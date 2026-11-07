package bindery_test

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
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer/bindery"
)

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-b", Kind: "bindery", Name: "Bindery", URL: url}
}

// TestCheckUsesSystemStatus pins the check endpoint difference: Bindery has
// no bare /api, testingAsync hits /api/v1/system/status (:101-110).
func TestCheckUsesSystemStatus(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := bindery.New(testInput(srv.URL), bindery.Secrets{APIKey: "bkey"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || path != "/api/v1/system/status" {
		t.Fatalf("result = %+v, path = %q", res, path)
	}
}

// TestGetMissingBareArrayAndPaging pins the client-side paging of the bare
// array (:22-50).
func TestGetMissingBareArrayAndPaging(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/wanted/missing" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		query = r.URL.Query()
		_, _ = w.Write([]byte(`[
			{"id": 1, "title": "The Will of the Many", "releaseDate": "2023-06-13", "imageUrl": "https://assets.hardcover.app/cover.jpg", "author": {"authorName": "James Islington"}},
			{"id": 2, "title": "No Date Book", "releaseDate": null, "imageUrl": "/api/v1/book/2/image", "author": {"authorName": "An Author"}},
			{"id": 3, "title": "Third Book", "releaseDate": "2024-01T00:00:00Z"}
		]`))
	}))
	defer srv.Close()
	client := bindery.New(testInput(srv.URL), bindery.Secrets{APIKey: "bkey"})
	res, err := client.GetMissing(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetMissing: %v", err)
	}
	if query.Get("pageSize") != "" {
		t.Fatalf("no pagination params expected, got %v", query)
	}
	// totalCount is the FULL array length, not the page size (:33).
	if res.TotalCount != 3 {
		t.Fatalf("totalCount = %d, want 3", res.TotalCount)
	}
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"id":1,"title":"The Will of the Many","type":"book","year":2023,"seriesTitle":"James Islington","imageUrl":"https://assets.hardcover.app/cover.jpg","link":"` + srv.URL + `/book/1"},` +
		`{"id":2,"title":"No Date Book","type":"book","seriesTitle":"An Author","imageUrl":null,"link":"` + srv.URL + `/book/2"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetMediaQueueUnionAndFiltering pins the array|{items} union, the
// "imported" exclusion, the stale timeLeft drop at 100% (:52-93).
func TestGetMediaQueueUnionAndFiltering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/queue" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items": [
			{"id": 10, "title": "q.imported", "status": "imported", "percentage": "100.0", "book": {"id": 5, "title": "Imported Book", "authorName": "A"}},
			{"id": 11, "title": "q.active", "status": "downloading", "percentage": "37.4", "timeLeft": "0:02:11", "book": {"id": 6, "title": "Active Book", "authorName": "B"}},
			{"id": 12, "title": "q.blocked", "status": "queued", "percentage": "100.0", "timeLeft": "721h 20m"},
			{"id": 13, "title": "q.nopercent", "status": "paused"}
		]}`))
	}))
	defer srv.Close()
	client := bindery.New(testInput(srv.URL), bindery.Secrets{APIKey: "bkey"})
	res, err := client.GetMediaQueue(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	// imported filtered BEFORE totalCount (:66-69).
	if res.TotalCount != 3 {
		t.Fatalf("totalCount = %d, want 3", res.TotalCount)
	}
	raw, err := json.Marshal(res.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"id":11,"title":"Active Book","type":"book","status":"downloading","timeLeft":"0:02:11","percentComplete":37,"seriesTitle":"B","imageUrl":null,"link":"` + srv.URL + `/book/6"},` +
		`{"id":12,"title":"q.blocked","type":"book","status":"queued","timeLeft":null,"percentComplete":100,"imageUrl":null,"link":"` + srv.URL + `/queue"},` +
		`{"id":13,"title":"q.nopercent","type":"book","status":"paused","timeLeft":null,"percentComplete":0,"imageUrl":null,"link":"` + srv.URL + `/queue"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestGetMediaQueueBareArray pins the union's array arm.
func TestGetMediaQueueBareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "title": "t", "status": "downloading", "percentage": "5"}]`))
	}))
	defer srv.Close()
	client := bindery.New(testInput(srv.URL), bindery.Secrets{APIKey: "bkey"})
	res, err := client.GetMediaQueue(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetMediaQueue: %v", err)
	}
	if res.TotalCount != 1 || res.Items[0].ID != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestMissingSecret(t *testing.T) {
	client := bindery.New(testInput("http://localhost:1"), bindery.Secrets{})
	_, err := client.GetMissing(context.Background(), 10)
	if err == nil || err.Error() != "No secret of kind apiKey was found" {
		t.Fatalf("error = %v", err)
	}
}

func TestServerErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down"))
	}))
	defer srv.Close()
	client := bindery.New(testInput(srv.URL), bindery.Secrets{APIKey: "bkey"})
	_, err := client.GetMediaQueue(context.Background(), 10)
	if err == nil {
		t.Fatal("expected error")
	}
	if mediaorganizer.AsError(err).Kind != mediaorganizer.ErrKindResponse {
		t.Fatalf("kind = %s", mediaorganizer.AsError(err).Kind)
	}
	_ = strings.TrimSpace
}
