package transmission_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient"
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/transmission"
)

// fakeTransmission records every JSON-RPC POST and answers with canned
// payloads, replaying the 409 session handshake once.
type fakeTransmission struct {
	t          *testing.T
	mu         sync.Mutex
	requests   []rpcCall
	sessionID  string
	handshaked bool
	torrents   string
}

type rpcCall struct {
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments"`
	Header    http.Header     `json:"-"`
}

func newFakeTransmission(t *testing.T) (*fakeTransmission, *httptest.Server) {
	t.Helper()
	f := &fakeTransmission{t: t, sessionID: "sess-abc"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transmission/rpc" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Basic "+basic("admin", "secret") {
			f.t.Fatalf("authorization = %q", got)
		}
		var call rpcCall
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &call); err != nil {
			f.t.Fatalf("rpc body: %v", err)
		}
		call.Header = r.Header.Clone()
		f.mu.Lock()
		f.requests = append(f.requests, call)
		needHandshake := !f.handshaked
		f.mu.Unlock()

		if needHandshake && r.Header.Get("X-Transmission-Session-Id") == "" {
			// First contact: 409 with the fresh session id in the RESPONSE
			// header (transmission.js:327-333).
			w.Header().Set("X-Transmission-Session-Id", f.sessionID)
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.mu.Lock()
		f.handshaked = true
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "session-get":
			_, _ = io.WriteString(w, `{"result":"success","arguments":{"version":"4.0.6"}}`)
		case "torrent-get":
			torrents := f.torrents
			if torrents == "" {
				torrents = `[]`
			}
			_, _ = io.WriteString(w, `{"result":"success","arguments":{"torrents":`+torrents+`}}`)
		default:
			_, _ = io.WriteString(w, `{"result":"success","arguments":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-t", Kind: "transmission", Name: "Transmission", URL: url}
}

const fixtureTorrents = `[
	{"id": 1, "name": "Ubuntu", "hashString": "aaaa", "totalSize": 1000, "percentDone": 0.5,
	 "rateDownload": 300, "rateUpload": 40, "eta": 60, "addedDate": 1700000000, "doneDate": 0,
	 "queuePosition": 0, "uploadedEver": 100, "downloadedEver": 500, "status": 4, "labels": ["linux"]},
	{"id": 2, "name": "Seeding", "hashString": "bbbb", "totalSize": 500, "percentDone": 1,
	 "rateDownload": 0, "rateUpload": 60, "eta": 0, "addedDate": 1700000001, "doneDate": 1700000100,
	 "queuePosition": 1, "uploadedEver": 900, "downloadedEver": 500, "status": 6, "labels": []},
	{"id": 3, "name": "Paused", "hashString": "cccc", "totalSize": 10, "percentDone": 0.2,
	 "rateDownload": 0, "rateUpload": 0, "eta": 0, "addedDate": 1700000002, "doneDate": 0,
	 "queuePosition": -1, "uploadedEver": 0, "downloadedEver": 2, "status": 0}
]`

func TestSessionHandshakeAndTorrentGet(t *testing.T) {
	f, srv := newFakeTransmission(t)
	f.torrents = fixtureTorrents
	client := transmission.New(testInput(srv.URL), transmission.Secrets{Username: "admin", Password: "secret"})
	// fixed "now": doneDate of Seeding is 1700000100 -> time = -100000ms
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	// Requests: session-get bootstrap (409 handshake + retry), then
	// torrent-get — three real POSTs.
	if len(f.requests) != 3 {
		t.Fatalf("requests = %d (%s), want session-get x2 + torrent-get", len(f.requests), methodNames(f.requests))
	}
	if f.requests[0].Method != "session-get" || f.requests[1].Method != "session-get" || f.requests[2].Method != "torrent-get" {
		t.Fatalf("methods = %v", methodNames(f.requests))
	}
	if got := f.requests[2].Header.Get("X-Transmission-Session-Id"); got != "sess-abc" {
		t.Fatalf("session id header = %q", got)
	}
	// torrent-get carries the explicit field list.
	var args struct {
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(f.requests[2].Arguments, &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	for _, field := range []string{"name", "hashString", "percentDone", "rateDownload", "labels", "eta", "addedDate"} {
		found := false
		for _, f2 := range args.Fields {
			if f2 == field {
				found = true
			}
		}
		if !found {
			t.Fatalf("torrent-get fields missing %q: %v", field, args.Fields)
		}
	}

	// Status over the FULL list; items mapped.
	if jobs.Status.Paused {
		t.Fatal("paused must be false")
	}
	if jobs.Status.Rates.Down != 300 || *jobs.Status.Rates.Up != 100 {
		t.Fatalf("rates = %+v", jobs.Status.Rates)
	}
	raw, err := json.Marshal(jobs.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[` +
		`{"id":"aaaa","index":0,"name":"Ubuntu","type":"torrent","size":1000,"sent":100,"received":500,"downSpeed":300,"upSpeed":40,"time":60000,"added":1700000000000,"state":"leeching","progress":0.5,"category":["linux"]},` +
		`{"id":"bbbb","index":1,"name":"Seeding","type":"torrent","size":500,"sent":900,"received":500,"upSpeed":60,"time":-100000,"added":1700000001000,"state":"seeding","progress":1,"category":[]},` +
		`{"id":"cccc","index":-1,"name":"Paused","type":"torrent","size":10,"sent":0,"received":2,"downSpeed":0,"upSpeed":0,"time":0,"added":1700000002000,"state":"paused","progress":0.2}]`
	if string(raw) != want {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, want)
	}
	if jobs.RatioItems != nil {
		t.Fatalf("no selection: ratioItems must be absent, got %+v", jobs.RatioItems)
	}
}

// TestSelectionAndRatioItems pins the local selection and the
// torrents.slice(0, limit) ratioItems (:93-101).
func TestSelectionAndRatioItems(t *testing.T) {
	f, srv := newFakeTransmission(t)
	client := transmission.New(testInput(srv.URL), transmission.Secrets{Username: "admin", Password: "secret"})
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	f.torrents = fixtureTorrents
	input := downloadclient.JobsInput{
		Limit: 1,
		Selection: &downloadclient.Selection{
			Sort:                   "progress",
			Descending:             true,
			CategoryFilter:         []string{"linux"},
			FilterIsWhitelist:      true,
			ShowCompletedTorrent:   true,
			ActiveTorrentThreshold: 0,
		},
	}
	jobs, err := client.GetJobsAndStatus(context.Background(), input)
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	// The category whitelist excludes Seeding (labels []) and Paused before
	// the progress/sort logic; Ubuntu survives and the limit is 1.
	if len(jobs.Items) != 1 || jobs.Items[0].ID != "aaaa" {
		t.Fatalf("items = %+v", jobs.Items)
	}
	if len(jobs.RatioItems) != 1 || jobs.RatioItems[0].Size != 1000 {
		t.Fatalf("ratioItems = %+v", jobs.RatioItems)
	}
}

func TestMutations(t *testing.T) {
	f, srv := newFakeTransmission(t)
	f.torrents = fixtureTorrents
	client := transmission.New(testInput(srv.URL), transmission.Secrets{Username: "admin", Password: "secret"})
	item := downloadclient.Item{ID: "aaaa", Progress: 0.5}
	if err := client.PauseQueue(context.Background()); err != nil {
		t.Fatalf("PauseQueue: %v", err)
	}
	last := f.requests[len(f.requests)-1]
	if last.Method != "torrent-stop" {
		t.Fatalf("pause queue method = %s", last.Method)
	}
	var args map[string]any
	_ = json.Unmarshal(last.Arguments, &args)
	if ids, ok := args["ids"].([]any); !ok || len(ids) != 3 {
		t.Fatalf("pause queue ids = %v", args["ids"])
	}
	if err := client.PauseItem(context.Background(), item); err != nil {
		t.Fatalf("PauseItem: %v", err)
	}
	last = f.requests[len(f.requests)-1]
	_ = json.Unmarshal(last.Arguments, &args)
	if ids, _ := args["ids"].([]any); len(ids) != 1 || ids[0] != "aaaa" {
		t.Fatalf("pause item ids = %v", args["ids"])
	}
	if err := client.ResumeItem(context.Background(), item); err != nil {
		t.Fatalf("ResumeItem: %v", err)
	}
	if last = f.requests[len(f.requests)-1]; last.Method != "torrent-start" {
		t.Fatalf("resume item method = %s", last.Method)
	}
	if err := client.DeleteItem(context.Background(), item, true); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	last = f.requests[len(f.requests)-1]
	if last.Method != "torrent-remove" {
		t.Fatalf("delete method = %s", last.Method)
	}
	_ = json.Unmarshal(last.Arguments, &args)
	if args["delete-local-data"] != true {
		t.Fatalf("delete-local-data = %v", args["delete-local-data"])
	}
}

func TestCheckSessionGet(t *testing.T) {
	f, srv := newFakeTransmission(t)
	client := transmission.New(testInput(srv.URL), transmission.Secrets{Username: "admin", Password: "secret"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK || f.requests[0].Method != "session-get" {
		t.Fatalf("result = %+v, methods = %v", res, methodNames(f.requests))
	}
}

func TestTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client := transmission.New(testInput(url), transmission.Secrets{Username: "a", Password: "b"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !strings.Contains(res.Error, "Request to integration failed") {
		t.Fatalf("error = %q", res.Error)
	}
}

func methodNames(calls []rpcCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}
