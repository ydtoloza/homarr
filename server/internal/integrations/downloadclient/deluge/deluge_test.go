package deluge_test

import (
	"context"
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
	"github.com/ydtoloza/homarr/server/internal/integrations/downloadclient/deluge"
)

// fakeDeluge emulates the Deluge WebUI /json RPC: auth.login sets the session
// cookie, web.connected/get_hosts/connect drive the daemon connect flow, and
// web.update_ui answers the torrent dict + stats.
type fakeDeluge struct {
	t         *testing.T
	mu        sync.Mutex
	requests  []delugeCall
	payload   string // web.update_ui result payload
	connected bool   // web.connected result
}

type delugeCall struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.Number     `json:"id"`
	Cookie string          `json:"-"`
}

func newFakeDeluge(t *testing.T) (*fakeDeluge, *httptest.Server) {
	t.Helper()
	f := &fakeDeluge{t: t, connected: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			f.t.Fatalf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var call delugeCall
		if err := json.Unmarshal(raw, &call); err != nil {
			f.t.Fatalf("rpc body: %v", err)
		}
		call.Cookie = r.Header.Get("Cookie")
		f.mu.Lock()
		f.requests = append(f.requests, call)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "auth.login":
			// Deluge answers {"result": true} with the session cookie.
			w.Header().Add("Set-Cookie", "_session_id=sess-77; expires=Wed, 21 Oct 2099 07:28:00 GMT; Path=/")
			_, _ = io.WriteString(w, `{"id": 1, "result": true}`)
		case "auth.check_session":
			_, _ = io.WriteString(w, `{"id": 2, "result": true}`)
		case "web.connected":
			if f.connected {
				_, _ = io.WriteString(w, `{"id": 3, "result": true}`)
			} else {
				_, _ = io.WriteString(w, `{"id": 3, "result": false}`)
			}
		case "web.get_hosts":
			_, _ = io.WriteString(w, `{"id": 4, "result": [["host-id-1", "127.0.0.1", 58846, "2.1.1"]]}`)
		case "web.connect":
			_, _ = io.WriteString(w, `{"id": 5, "result": "host-id-1"}`)
		case "web.update_ui":
			_, _ = io.WriteString(w, `{"id": 6, "result": `+f.payload+`}`)
		default:
			_, _ = io.WriteString(w, `{"id": 7, "result": true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

const delugePayload = `{
	"torrents": {
		"tid1": {"name": "Ubuntu", "state": "Downloading", "progress": 55.5, "queue": 1,
		         "eta": 30, "time_added": 1700000000, "total_wanted": 1000, "total_uploaded": 200,
		         "download_payload_rate": 128.9, "upload_payload_rate": 12.4,
		         "completed_time": 1700001000, "label": "movies"},
		"tid2": {"name": "Seeding", "state": "Seeding", "progress": 100, "queue": -1,
		         "eta": 0, "time_added": 1700000001, "total_wanted": 500, "total_uploaded": 900,
		         "download_payload_rate": 0, "upload_payload_rate": 5.5,
		         "completed_time": 1700000100, "label": ""}
	},
	"stats": {"download_rate": 128.9, "upload_rate": 17.95}
}`

func testInput(url string) integrations.Input {
	return integrations.Input{ID: "int-d", Kind: "deluge", Name: "Deluge", URL: url}
}

func (f *fakeDeluge) calls() []delugeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]delugeCall{}, f.requests...)
}

// TestLoginCookieAndConnectFlow pins the auth + connect flow: auth.login with
// [password], the cookie on every later call, and web.connected -> update_ui
// without a connect when the daemon is attached.
func TestLoginCookieAndConnectFlow(t *testing.T) {
	f, srv := newFakeDeluge(t)
	f.payload = delugePayload
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "deluge-pass"})
	// fixed "now": completed_time of Seeding is 1700000100 -> time = -100000ms
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	jobs, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 50})
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	calls := f.calls()
	methods := callMethods(calls)
	// validateAuth: no cookie -> login (auth.check_session is skipped when
	// no cookie exists); then web.connected; then web.update_ui.
	want := []string{"auth.login", "web.connected", "web.update_ui"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	for _, c := range calls[1:] {
		if c.Cookie != "_session_id=sess-77" {
			t.Fatalf("%s cookie = %q", c.Method, c.Cookie)
		}
	}
	// The login params carry the password.
	var loginParams []any
	_ = json.Unmarshal(calls[0].Params, &loginParams)
	if len(loginParams) != 1 || loginParams[0] != "deluge-pass" {
		t.Fatalf("login params = %v", loginParams)
	}

	// Status: rates floored; paused false (Downloading present).
	if jobs.Status.Paused {
		t.Fatal("paused must be false")
	}
	if jobs.Status.Rates.Down != 128 || *jobs.Status.Rates.Up != 17 {
		t.Fatalf("rates = %+v (want floored 128/17)", jobs.Status.Rates)
	}
	// Items: id from the dict key, progress/100, label "" present.
	raw, err := json.Marshal(jobs.Items)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantItems := `[` +
		`{"id":"tid1","index":1,"name":"Ubuntu","type":"torrent","size":1000,"sent":200,"downSpeed":128.9,"upSpeed":12.4,"time":30000,"added":1700000000000,"state":"leeching","progress":0.555,"category":"movies"},` +
		`{"id":"tid2","index":-1,"name":"Seeding","type":"torrent","size":500,"sent":900,"upSpeed":5.5,"time":-100000,"added":1700000001000,"state":"seeding","progress":1,"category":""}]`
	if string(raw) != wantItems {
		t.Fatalf("items JSON mismatch:\n got: %s\nwant: %s", raw, wantItems)
	}
	if jobs.RatioItems != nil {
		t.Fatalf("no selection: ratioItems must be absent")
	}
}

// TestConnectWhenDetached pins the web.connected false -> get_hosts -> connect
// sequence (deluge.js:60-75).
func TestConnectWhenDetached(t *testing.T) {
	f, srv := newFakeDeluge(t)
	f.payload = delugePayload
	f.connected = false
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 1}); err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	methods := callMethods(f.calls())
	want := []string{"auth.login", "web.connected", "web.get_hosts", "web.connect", "web.update_ui"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	var connectParams []any
	_ = json.Unmarshal(f.calls()[3].Params, &connectParams)
	if len(connectParams) != 1 || connectParams[0] != "host-id-1" {
		t.Fatalf("connect params = %v", connectParams)
	}
}

// TestUpdateUIFields pins the fixed field list plus the extra field and the
// empty filter (deluge.js:344-371, homarr passes ["completed_time"]).
func TestUpdateUIFields(t *testing.T) {
	f, srv := newFakeDeluge(t)
	f.payload = delugePayload
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	if _, err := client.GetJobsAndStatus(context.Background(), downloadclient.JobsInput{Limit: 1}); err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	var params []any
	_ = json.Unmarshal(f.calls()[2].Params, &params)
	if len(params) != 2 {
		t.Fatalf("update_ui params = %v", params)
	}
	fieldsRaw, _ := json.Marshal(params[0])
	var fields []string
	_ = json.Unmarshal(fieldsRaw, &fields)
	for _, want := range []string{"name", "progress", "label", "completed_time", "total_wanted", "upload_payload_rate"} {
		found := false
		for _, field := range fields {
			if field == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("update_ui fields missing %q: %v", want, fields)
		}
	}
	filterRaw, _ := json.Marshal(params[1])
	if string(filterRaw) != "{}" {
		t.Fatalf("filter = %s, want {}", filterRaw)
	}
}

// TestSelectionAndRatioItems pins the local selection and ratioItems.
func TestSelectionAndRatioItems(t *testing.T) {
	f, srv := newFakeDeluge(t)
	f.payload = delugePayload
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	client.Now = func() time.Time { return time.Unix(1700000200, 0) }
	input := downloadclient.JobsInput{
		Limit: 1,
		Selection: &downloadclient.Selection{
			Sort:                   "progress",
			Descending:             true,
			CategoryFilter:         []string{"movies"},
			FilterIsWhitelist:      true,
			ShowCompletedTorrent:   true,
			ActiveTorrentThreshold: 0,
		},
	}
	jobs, err := client.GetJobsAndStatus(context.Background(), input)
	if err != nil {
		t.Fatalf("GetJobsAndStatus: %v", err)
	}
	// tid2 (complete, label "") is excluded by the whitelist; tid1 wins.
	if len(jobs.Items) != 1 || jobs.Items[0].ID != "tid1" {
		t.Fatalf("items = %+v", jobs.Items)
	}
	if len(jobs.RatioItems) != 1 || jobs.RatioItems[0].Size != 1000 {
		t.Fatalf("ratioItems = %+v", jobs.RatioItems)
	}
}

func TestMutations(t *testing.T) {
	f, srv := newFakeDeluge(t)
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	item := downloadclient.Item{ID: "tid1", Progress: 0.5}
	if err := client.PauseItem(context.Background(), item); err != nil {
		t.Fatalf("PauseItem: %v", err)
	}
	last := f.calls()[len(f.calls())-1]
	if last.Method != "core.pause_torrent" {
		t.Fatalf("method = %s", last.Method)
	}
	var params []any
	_ = json.Unmarshal(last.Params, &params)
	if inner, ok := params[0].([]any); !ok || len(inner) != 1 || inner[0] != "tid1" {
		t.Fatalf("pause params = %v (want [[tid1]])", params)
	}
	if err := client.ResumeItem(context.Background(), item); err != nil {
		t.Fatalf("ResumeItem: %v", err)
	}
	if err := client.DeleteItem(context.Background(), item, true); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	last = f.calls()[len(f.calls())-1]
	if last.Method != "core.remove_torrent" {
		t.Fatalf("delete method = %s", last.Method)
	}
	var removeParams []any
	_ = json.Unmarshal(last.Params, &removeParams)
	if len(removeParams) != 2 || removeParams[0] != "tid1" || removeParams[1] != true {
		t.Fatalf("remove params = %v", removeParams)
	}
}

func TestErrorBodySurfaces(t *testing.T) {
	// The error body only fires for the torrent method: the login and the
	// connect probes answer normally (deluge.js throws on body.error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call delugeCall
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &call)
		w.Header().Set("Content-Type", "application/json")
		switch call.Method {
		case "auth.login":
			w.Header().Add("Set-Cookie", "_session_id=s; Path=/")
			_, _ = w.Write([]byte(`{"id": 1, "result": true}`))
		case "web.connected":
			_, _ = w.Write([]byte(`{"id": 2, "result": true}`))
		default:
			_, _ = w.Write([]byte(`{"id": 3, "result": null, "error": {"message": "Unknown method", "code": 1}}`))
		}
	}))
	defer srv.Close()
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	err := client.PauseQueue(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// The plain Error of the library classifies as the pinned upstream
	// unknown message (IntegrationUnknownError).
	if err.Error() != "An unknown error occured while executing Integration method" {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckLogin(t *testing.T) {
	f, srv := newFakeDeluge(t)
	client := deluge.New(testInput(srv.URL), deluge.Secrets{Password: "p"})
	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("result = %+v", res)
	}
	if callMethods(f.calls())[0] != "auth.login" {
		t.Fatalf("check must log in first, got %v", callMethods(f.calls()))
	}
}

func callMethods(calls []delugeCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}
