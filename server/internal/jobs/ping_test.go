package jobs_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/jobs"
)

// seedApp inserts one app row.
func seedApp(t *testing.T, d *db.DB, id, name, href, pingURL string) {
	t.Helper()
	if err := d.InsertApp(t.Context(), db.App{ID: id, Name: name, IconURL: "i", HREF: href, PingURL: pingURL}); err != nil {
		t.Fatalf("seed app %s: %v", id, err)
	}
}

// setBoardSetting writes the serverSetting "board" superjson row.
func setBoardSetting(t *testing.T, d *db.DB, forceDisableStatus bool) {
	t.Helper()
	value := fmt.Sprintf(`{"json":{"forceDisableStatus":%t}}`, forceDisableStatus)
	if err := d.UpsertServerSetting(t.Context(), "board", value); err != nil {
		t.Fatalf("upsert server setting: %v", err)
	}
}

// collectEvents gathers bus messages on events.ping.* until the expected
// number arrives or the timeout passes.
func collectEvents(t *testing.T, sub *bus.Subscription, want int) []bus.Message {
	t.Helper()
	var got []bus.Message
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case msg := <-sub.C:
			got = append(got, msg)
		case <-deadline:
			t.Fatalf("collected %d/%d ping events", len(got), want)
		}
	}
	return got
}

// TestPingJobEndToEnd pins the full ping flow: the job loads the targets from
// the app table (ping_url ?? href, http(s) only), checks them through the
// ping integration, publishes one event per target on events.ping.<appId>
// with the upstream payload shape, and publishes its TaskStatus on
// events.cronjob.ping.
func TestPingJobEndToEnd(t *testing.T) {
	database := db.OpenTest(t)
	var onlineHits atomic.Int64
	online := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		onlineHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer online.Close()
	offline := httptest.NewServer(http.NotFoundHandler())
	offline.Close() // nothing listens: connection refused

	setBoardSetting(t, database, false)
	seedApp(t, database, "app-online", "Online", "", online.URL)                  // ping_url
	seedApp(t, database, "app-fallback", "Fallback", online.URL, "")              // href fallback
	seedApp(t, database, "app-offline", "Offline", "", offline.URL)               // refused
	seedApp(t, database, "app-scheme", "Custom scheme", "vscode://profile/x", "") // excluded
	seedApp(t, database, "app-empty", "Empty", "", "")                            // excluded

	b := bus.New()
	defer b.Close()
	pingSub := b.Subscribe("events.ping.*")
	defer pingSub.Unsubscribe()
	statusSub := b.Subscribe("events.cronjob.ping")
	defer statusSub.Unsubscribe()

	s := jobs.New(database, b)
	s.Register(jobs.PingJob(database, b))
	if err := s.Trigger(t.Context(), "ping"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// One event per target (three targets). Two share the online URL: both
	// receive the result, the URL is only fetched once per run (upstream
	// dedupe) — the online test server counts requests to verify.
	messages := collectEvents(t, pingSub, 3)
	byApp := map[string]map[string]any{}
	for _, msg := range messages {
		var m map[string]any
		if err := json.Unmarshal(msg.Payload, &m); err != nil {
			t.Fatalf("payload %s: %v", msg.Payload, err)
		}
		byApp[msg.Topic] = m
	}

	mOnline := byApp["events.ping.app-online"]
	if mOnline == nil {
		t.Fatalf("no event for app-online: %v", messages)
	}
	if len(mOnline) != 3 || mOnline["statusCode"] != float64(http.StatusOK) || mOnline["url"] != online.URL {
		t.Fatalf("online payload = %v", mOnline)
	}
	if _, ok := mOnline["durationMs"].(float64); !ok || mOnline["durationMs"].(float64) <= 0 {
		t.Fatalf("online payload missing durationMs: %v", mOnline)
	}

	mFallback := byApp["events.ping.app-fallback"]
	if mFallback == nil || mFallback["url"] != online.URL || mFallback["statusCode"] != float64(http.StatusOK) {
		t.Fatalf("fallback payload = %v", mFallback)
	}

	mOffline := byApp["events.ping.app-offline"]
	if mOffline == nil {
		t.Fatalf("no event for app-offline: %v", messages)
	}
	if len(mOffline) != 2 || mOffline["error"] == "" || mOffline["url"] != offline.URL {
		t.Fatalf("offline payload = %v, want exactly url+error", mOffline)
	}

	// Run state: last-data shows idle/success (the cron_job_configuration row
	// is untouched by runs — it only holds name/cron_expression/is_enabled,
	// see TestSchedulerPersistsConfigurationAndResumes).
	ts, ok := s.LastStatus("ping")
	if !ok || ts.Status != jobs.StatusIdle || ts.LastExecutionStatus == nil || *ts.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("last status = %+v, want idle/success", ts)
	}
	statusRunning := readStatus(t, statusSub)
	statusIdle := readStatus(t, statusSub)
	if statusRunning.Status != jobs.StatusRunning || statusRunning.LastExecutionStatus != nil {
		t.Fatalf("running status = %+v", statusRunning)
	}
	if statusIdle.Status != jobs.StatusIdle || *statusIdle.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("idle status = %+v", statusIdle)
	}

	// The upstream dedupe: two apps share the URL, one HTTP request.
	if requests := onlineHits.Load(); requests != 1 {
		t.Fatalf("online server got %d requests, want 1 (per-run URL dedupe)", requests)
	}
}

// TestPingJobForceDisableStatus pins the forceDisableStatus guard: with the
// server setting on, the job runs but does nothing (ping.ts:22-26).
func TestPingJobForceDisableStatus(t *testing.T) {
	database := db.OpenTest(t)
	setBoardSetting(t, database, true)
	seedApp(t, database, "app-1", "One", "http://127.0.0.1:1/x", "")

	b := bus.New()
	defer b.Close()
	sub := b.Subscribe("events.ping.*")
	defer sub.Unsubscribe()

	s := jobs.New(database, b)
	s.Register(jobs.PingJob(database, b))
	if err := s.Trigger(t.Context(), "ping"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	select {
	case msg := <-sub.C:
		t.Fatalf("unexpected ping event: %s", msg.Payload)
	case <-time.After(300 * time.Millisecond):
	}

	// The job still reports a successful run.
	if ts, ok := s.LastStatus("ping"); !ok || ts.LastExecutionStatus == nil || *ts.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("last status = %+v", ts)
	}
}

// TestPingJobParallelismBounded pins the bounded parallelism: with
// Parallelism 1 the checks serialize.
func TestPingJobParallelismBounded(t *testing.T) {
	database := db.OpenTest(t)

	var inflight, maxInflight int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inflight++
		if inflight > maxInflight {
			maxInflight = inflight
		}
		mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
	}))
	defer srv.Close()

	for i := 0; i < 4; i++ {
		seedApp(t, database, fmt.Sprintf("app-%d", i), fmt.Sprintf("App %d", i), srv.URL, "")
	}

	s := jobs.New(database, nil)
	s.Register(jobs.PingJob(database, nil, jobs.PingOptions{Parallelism: 1}))
	if err := s.Trigger(t.Context(), "ping"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if maxInflight != 1 {
		t.Fatalf("max concurrent checks = %d, want 1", maxInflight)
	}
}

// TestPingJobScheduleAndName pins the upstream registry entry: name "ping",
// schedule EVERY_MINUTE (createCronJob("ping", EVERY_MINUTE, ...)).
func TestPingJobScheduleAndName(t *testing.T) {
	database := db.OpenTest(t)
	def := jobs.PingJob(database, nil)
	if def.Name != "ping" {
		t.Fatalf("name = %q, want ping", def.Name)
	}
	if def.Schedule != jobs.ScheduleEveryMinute {
		t.Fatalf("schedule = %q, want * * * * *", def.Schedule)
	}

	// The default list of a scheduler with only the ping job is what the M4
	// cronJobs router getJobs will serve.
	s := jobs.New(database, nil)
	s.Register(def)
	list, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ping" || !list[0].IsEnabled || list[0].Cron != "* * * * *" {
		t.Fatalf("List = %+v", list)
	}
}
