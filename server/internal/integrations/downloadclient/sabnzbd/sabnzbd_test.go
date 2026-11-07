package sabnzbd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/sabnzbd"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// fakeSab records the /api calls and answers per-mode payloads.
type fakeSab struct {
	t        *testing.T
	requests []sabCall
	payloads map[string]string // mode -> body ("" -> default empty JSON)
	status   int
}

type sabCall struct {
	query  url.Values
	header http.Header
}

func newFakeSab(t *testing.T) (*fakeSab, *httptest.Server) {
	t.Helper()
	f := &fakeSab{t: t, status: http.StatusOK, payloads: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		f.requests = append(f.requests, sabCall{query: r.URL.Query(), header: r.Header.Clone()})
		mode := r.URL.Query().Get("mode")
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte("down"))
			return
		}
		if body, ok := f.payloads[mode]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-s", Kind: "sabNzbd", Name: "SABnzbd", URL: url}
}

const sabQueuePayload = `{"queue": {"paused": false, "kbpersec": "10.5", "slots": [
	{"status": "Downloading", "index": 0, "mb": "10.5", "filename": "one.nzb", "cat": "movies",
	 "timeleft": "0:00:10:30", "percentage": "50", "nzo_id": "nzo1"},
	{"status": "Queued", "index": 1, "mb": "2", "filename": "two.nzb", "cat": "tv",
	 "timeleft": "1:02:00:00", "percentage": "0", "nzo_id": "nzo2"},
	{"status": "Paused", "index": 2, "mb": "1", "filename": "three.nzb", "cat": "tv",
	 "timeleft": "0:00:00:05", "percentage": "10", "nzo_id": "nzo3"}
]}}`

const sabHistoryPayload = `{"history": {"slots": [
	{"category": "movies", "download_time": 60, "status": "Completed", "completed": 1700000100,
	 "nzo_id": "nzo1", "postproc_time": 30, "name": "one.nzb", "bytes": 11010048},
	{"category": "tv", "download_time": 10, "status": "Failed", "completed": 1700000200,
	 "nzo_id": "nzo9", "postproc_time": 5, "name": "failed.nzb", "bytes": 100}
]}}`

// TestGetJobsAndStatusShape pins the whole mapping: apikey/output/mode query
// params, KiB/s -> B/s floor, MiB -> ceil bytes, timeleft parse, states,
// history concat and the limit slice.
func TestGetJobsAndStatusShape(t *testing.T) {
	f, srv := newFakeSab(t)
	f.payloads["queue"] = sabQueuePayload
	f.payloads["history"] = sabHistoryPayload
	client := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "sab-key"})
	// fixed now: completed 1700000200 -> last history time = 0ms
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 2})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	// Two calls: queue + history, both with the shared query params.
	if len(f.requests) != 2 {
		t.Fatalf("calls = %d, want queue + history", len(f.requests))
	}
	for _, call := range f.requests {
		if got := call.query.Get("apikey"); got != "sab-key" {
			t.Fatalf("apikey param = %q", got)
		}
		if got := call.query.Get("output"); got != "json" {
			t.Fatalf("output param = %q", got)
		}
		if got := call.query.Get("limit"); got != "2" {
			t.Fatalf("limit param = %q", got)
		}
	}
	if f.requests[0].query.Get("mode") != "queue" || f.requests[1].query.Get("mode") != "history" {
		t.Fatalf("modes = %q, %q", f.requests[0].query.Get("mode"), f.requests[1].query.Get("mode"))
	}

	// status: kbpersec 10.5 KiB/s -> floor(10752) = 10752; rates.up absent.
	if jobs.Status.Paused {
		t.Fatal("paused must be false")
	}
	if jobs.Status.Rates.Down != 10752 {
		t.Fatalf("down rate = %v, want 10752", jobs.Status.Rates.Down)
	}
	if jobs.Status.Rates.Up != nil {
		t.Fatalf("up rate must be absent for SABnzbd, got %v", *jobs.Status.Rates.Up)
	}

	// limit 2 slices AFTER the concat (2 queue slots survive).
	raw, err := json.Marshal(jobs.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[` +
		// queue slot 0: index 0 -> downSpeed = status rate; size ceil(10.5*1024^2)=11010048;
		// timeleft 0:00:10:30 -> 10h30m = 37800000ms; progress .5
		`{"id":"nzo1","index":0,"name":"one.nzb","type":"usenet","size":11010048,"downSpeed":10752,"time":630000,"state":"downloading","progress":0.5,"category":"movies"},` +
		// queue slot 1: index 1 -> downSpeed 0; timeleft 1:02:00:00 -> 93600000000ms
		`{"id":"nzo2","index":1,"name":"two.nzb","type":"usenet","size":2097152,"downSpeed":0,"time":93600000,"state":"queued","progress":0,"category":"tv"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestFullQueueAndHistoryShapes pins the mapping beyond the limit slice.
func TestFullQueueAndHistoryShapes(t *testing.T) {
	f, srv := newFakeSab(t)
	f.payloads["queue"] = sabQueuePayload
	f.payloads["history"] = sabHistoryPayload
	client := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "k"})
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	raw, err := json.Marshal(jobs.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[` +
		`{"id":"nzo1","index":0,"name":"one.nzb","type":"usenet","size":11010048,"downSpeed":10752,"time":630000,"state":"downloading","progress":0.5,"category":"movies"},` +
		`{"id":"nzo2","index":1,"name":"two.nzb","type":"usenet","size":2097152,"downSpeed":0,"time":93600000,"state":"queued","progress":0,"category":"tv"},` +
		`{"id":"nzo3","index":2,"name":"three.nzb","type":"usenet","size":1048576,"downSpeed":0,"time":5000,"state":"paused","progress":0.1,"category":"tv"},` +
		// history: index restarts at 0, progress 1, time = completed*1000 - now,
		// added = (completed - download_time - postproc_time)*1000
		`{"id":"nzo1","index":0,"name":"one.nzb","type":"usenet","size":11010048,"time":-100000,"added":1700000010000,"state":"completed","progress":1,"category":"movies"},` +
		`{"id":"nzo9","index":1,"name":"failed.nzb","type":"usenet","size":100,"time":0,"added":1700000185000,"state":"failed","progress":1,"category":"tv"}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
}

// TestArchivedHistory pins mode=history&archive=1 paging and the merge rules
// of sabnzbd-history.ts: processing slots first, then the window sorted desc,
// deduped.
func TestArchivedHistory(t *testing.T) {
	f, srv := newFakeSab(t)
	f.payloads["queue"] = `{"queue": {"paused": true, "kbpersec": "0", "slots": []}}`
	// Active history: one processing slot (still not Completed/Failed) and
	// one completed inside the window.
	f.payloads["history"] = `{"history": {"slots": [
		{"category": "tv", "download_time": 1, "status": "Repairing", "completed": 1700000100,
		 "nzo_id": "proc", "postproc_time": 0, "name": "processing.nzb", "bytes": 1},
		{"category": "tv", "download_time": 1, "status": "Completed", "completed": 1700000000,
		 "nzo_id": "done", "postproc_time": 0, "name": "done.nzb", "bytes": 2}
	]}}`
	// Archived pages: page 0 (full 100 rows of old entries) then page 1
	// (one entry inside the window and one duplicated done).
	var archived []string
	for i := 0; i < 99; i++ {
		// Old filler rows: outside the 7-day window (cutoff = 1700000200 -
		// 604800 = 1699395400).
		archived = append(archived, `{"category": "old", "download_time": 1, "status": "Completed", "completed": 1000000000, "nzo_id": "old-`+strconv.Itoa(i)+`", "postproc_time": 0, "name": "old.nzb", "bytes": 1}`)
	}
	// One row inside the window keeps the loop going to page 1 (a duplicate
	// of "done", which the dedupe drops).
	archived = append(archived, `{"category": "tv", "download_time": 1, "status": "Completed", "completed": 1700000000, "nzo_id": "done", "postproc_time": 0, "name": "done.nzb", "bytes": 2}`)
	page0 := `{"history": {"slots": [` + strings.Join(archived, ",") + `]}}`
	page1 := `{"history": {"slots": [
		{"category": "tv", "download_time": 1, "status": "Completed", "completed": 1700000050,
		 "nzo_id": "arch", "postproc_time": 0, "name": "arch.nzb", "bytes": 3},
		{"category": "tv", "download_time": 1, "status": "Completed", "completed": 1700000000,
		 "nzo_id": "done", "postproc_time": 0, "name": "done.nzb", "bytes": 2}
	]}}`
	client := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "k"})
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }

	// The fake must answer page 0 then page 1 for archive calls.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		f.requests = append(f.requests, sabCall{query: r.URL.Query(), header: r.Header.Clone()})
		mode := r.URL.Query().Get("mode")
		switch {
		case mode == "queue":
			_, _ = w.Write([]byte(f.payloads["queue"]))
		case mode == "history" && r.URL.Query().Get("archive") == "":
			_, _ = w.Write([]byte(f.payloads["history"]))
		case mode == "history" && r.URL.Query().Get("start") == "0":
			_, _ = w.Write([]byte(page0))
		case mode == "history" && r.URL.Query().Get("start") == "100":
			_, _ = w.Write([]byte(page1))
		default:
			t.Fatalf("unexpected archive call %v", r.URL.Query())
		}
	})

	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{
		Limit:                  50,
		IncludeArchivedHistory: true,
		HistoryWindowDays:      7,
	})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	ids := make([]string, 0, len(jobs.Items))
	for _, item := range jobs.Items {
		ids = append(ids, item.ID)
	}
	// Processing first, then the window rows sorted by completed desc:
	// done(1700000000) comes after arch(1700000050).
	wantIDs := []string{"proc", "arch", "done"}
	if strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("ids = %v, want %v", ids, wantIDs)
	}
	// The dedupe dropped the duplicate "done"; the page-0 filler rows are
	// inside the window but are archived-only, while the page-1 "old" rows
	// (completed 1000000000) fall outside the 7-day window.
	if len(jobs.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(jobs.Items))
	}
}

func TestMutations(t *testing.T) {
	f, srv := newFakeSab(t)
	client := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "k"})
	item := downloadclient.Item{ID: "nzo1", Progress: 0.5}
	done := downloadclient.Item{ID: "nzo9", Progress: 1}

	if err := client.PauseQueue(context.Background()); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	if got := f.requests[len(f.requests)-1]; got.query.Get("mode") != "pause" {
		t.Fatalf("pause mode = %q", got.query.Get("mode"))
	}
	if err := client.ResumeQueue(context.Background()); err != nil {
		t.Fatalf("ResumeQueue: %v", err)
	}
	if got := f.requests[len(f.requests)-1]; got.query.Get("mode") != "resume" {
		t.Fatalf("resume mode = %q", got.query.Get("mode"))
	}
	if err := client.PauseItem(context.Background(), item); err != nil {
		t.Fatalf("PauseItem: %v", err)
	}
	got := f.requests[len(f.requests)-1]
	if got.query.Get("mode") != "queue" || got.query.Get("name") != "pause" || got.query.Get("value") != "nzo1" {
		t.Fatalf("pause item = %v", got.query)
	}
	if err := client.ResumeItem(context.Background(), item); err != nil {
		t.Fatalf("ResumeItem: %v", err)
	}
	// Active item deletion uses mode=queue; fromDisk keeps the archive and
	// removes the files.
	if err := client.DeleteItem(context.Background(), item, true); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	got = f.requests[len(f.requests)-1]
	if got.query.Get("mode") != "queue" || got.query.Get("name") != "delete" ||
		got.query.Get("archive") != "0" || got.query.Get("del_files") != "1" || got.query.Get("value") != "nzo1" {
		t.Fatalf("delete active = %v", got.query)
	}
	// Completed item deletion uses mode=history; !fromDisk archives it.
	if err := client.DeleteItem(context.Background(), done, false); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	got = f.requests[len(f.requests)-1]
	if got.query.Get("mode") != "history" || got.query.Get("archive") != "1" || got.query.Get("del_files") != "0" {
		t.Fatalf("delete done = %v", got.query)
	}
}

func TestMissingSecretAndErrorPath(t *testing.T) {
	client := sabnzbd.New(testInput("http://localhost:1"), sabnzbd.Secrets{})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Error != "No secret of kind apiKey was found" {
		t.Fatalf("error = %q", res.Error)
	}

	f, srv := newFakeSab(t)
	f.status = http.StatusInternalServerError
	client2 := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "k"})
	_, err = client2.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 1})
	if err == nil {
		t.Fatal("expected error")
	}
	if mediaorganizer.AsError(err).Kind != mediaorganizer.ErrKindResponse {
		t.Fatalf("kind = %s", mediaorganizer.AsError(err).Kind)
	}
}

func TestCheckUsesTranslatePing(t *testing.T) {
	f, srv := newFakeSab(t)
	client := sabnzbd.New(testInput(srv.URL), sabnzbd.Secrets{APIKey: "k"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("result = %+v", res)
	}
	if got := f.requests[0]; got.query.Get("mode") != "translate" || got.query.Get("value") != "ping" {
		t.Fatalf("check query = %v", got.query)
	}
}
