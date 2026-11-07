package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/jobs"
)

// counterJob returns a Definition whose Run counts executions.
func counterJob(name, schedule string, counter *atomic.Int64) jobs.Definition {
	return jobs.Definition{
		Name:     name,
		Schedule: schedule,
		Run: func(context.Context) error {
			counter.Add(1)
			return nil
		},
	}
}

// waitFor polls until cond() is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// readStatus awaits one TaskStatus event on the subscription.
func readStatus(t *testing.T, sub *bus.Subscription) jobs.TaskStatus {
	t.Helper()
	select {
	case msg := <-sub.C:
		var ts jobs.TaskStatus
		if err := json.Unmarshal(msg.Payload, &ts); err != nil {
			t.Fatalf("status payload %s: %v", msg.Payload, err)
		}
		return ts
	case <-time.After(3 * time.Second):
		t.Fatal("no status event received")
		return jobs.TaskStatus{}
	}
}

// TestSchedulerRunsJobOnCronSchedule pins the scheduling loop: a job with a
// 1-second cron ("* * * * * *", the 6-field form upstream uses for
// EVERY_5_SECONDS) fires repeatedly until Stop.
func TestSchedulerRunsJobOnCronSchedule(t *testing.T) {
	var counter atomic.Int64
	s := jobs.New(nil, nil)
	s.Register(counterJob("fast", "* * * * * *", &counter))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 6*time.Second, func() bool { return counter.Load() >= 2 })
	s.Stop()

	// After Stop the schedule no longer fires.
	final := counter.Load()
	time.Sleep(1500 * time.Millisecond)
	if counter.Load() != final {
		t.Fatalf("counter advanced after Stop: %d -> %d", final, counter.Load())
	}
}

// TestSchedulerTriggerRunsImmediately pins the manual trigger semantics
// (cronJobs.triggerJob → runManuallyAsync): the run executes synchronously
// and its TaskStatus is recorded.
func TestSchedulerTriggerRunsImmediately(t *testing.T) {
	var counter atomic.Int64
	s := jobs.New(nil, nil)
	s.Register(counterJob("manual", "* * * * *", &counter))

	if err := s.Trigger(t.Context(), "manual"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if counter.Load() != 1 {
		t.Fatalf("counter = %d, want 1 (synchronous run)", counter.Load())
	}
	if err := s.Trigger(t.Context(), "manual"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if counter.Load() != 2 {
		t.Fatalf("counter = %d, want 2", counter.Load())
	}

	ts, ok := s.LastStatus("manual")
	if !ok {
		t.Fatal("LastStatus: no status recorded")
	}
	if ts.Status != jobs.StatusIdle || ts.LastExecutionStatus == nil || *ts.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("status = %+v, want idle/success", ts)
	}
}

// TestSchedulerTriggerGuards pins the error surface of Trigger: unknown jobs
// (JobManager throws `Job ${name} not found`) and preventManualExecution
// (runManuallyAsync throws `The job "${name}" can not be executed manually.`).
func TestSchedulerTriggerGuards(t *testing.T) {
	s := jobs.New(nil, nil)
	s.Register(jobs.Definition{
		Name:                   "locked",
		Schedule:               "* * * * *",
		Run:                    func(context.Context) error { return nil },
		PreventManualExecution: true,
	})

	if err := s.Trigger(t.Context(), "missing"); err == nil || err.Error() != "Job missing not found" {
		t.Fatalf("err = %v, want `Job missing not found`", err)
	}
	if err := s.Trigger(t.Context(), "locked"); err == nil ||
		err.Error() != `The job "locked" can not be executed manually.` {
		t.Fatalf("err = %v, want the upstream preventManualExecution message", err)
	}
}

// TestSchedulerPersistsConfigurationAndResumes pins the
// cron_job_configuration persistence: the row uses the upstream columns
// (name/cron_expression/is_enabled, migration 0033), is written with the
// JobManager UPDATE/INSERT semantics, and a fresh scheduler over the same
// database resumes it (disabled jobs stay unscheduled, custom expressions are
// honored).
func TestSchedulerPersistsConfigurationAndResumes(t *testing.T) {
	database := db.OpenTest(t)
	var counter atomic.Int64

	// Scheduler 1: customize the interval, run once, disable.
	s1 := jobs.New(database, nil)
	s1.Register(counterJob("resumable", jobs.ScheduleEveryMinute, &counter))
	if err := s1.UpdateInterval(t.Context(), "resumable", "* * * * * *"); err != nil {
		t.Fatalf("UpdateInterval: %v", err)
	}
	if err := s1.Trigger(t.Context(), "resumable"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if err := s1.Disable(t.Context(), "resumable"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// The row must match the upstream columns and the JobManager semantics
	// (updateIntervalAsync inserted {cron: custom, enabled: true},
	// disableAsync updated is_enabled to 0).
	var cronExpr string
	var enabled int64
	if err := database.QueryRow(`SELECT cron_expression, is_enabled FROM "cron_job_configuration" WHERE name = 'resumable'`).
		Scan(&cronExpr, &enabled); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cronExpr != "* * * * * *" || enabled != 0 {
		t.Fatalf("row = (%q, %d), want (* * * * * *, 0)", cronExpr, enabled)
	}

	// Scheduler 2 (fresh instance, same DB = "restart"): the disabled job
	// must NOT be scheduled and the stored custom expression wins over the
	// definition default.
	s2 := jobs.New(database, nil)
	s2.Register(counterJob("resumable", jobs.ScheduleEveryMinute, &counter))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s2.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if counter.Load() != 1 {
		t.Fatalf("counter = %d, want 1 (disabled job must not run after restart)", counter.Load())
	}

	// Enable persists is_enabled=1 and schedules with the stored expression
	// (upstream enableAsync → updateConfiguration + startAsync).
	if err := s2.Enable(t.Context(), "resumable"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitFor(t, 6*time.Second, func() bool { return counter.Load() >= 3 })

	list, err := s2.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "resumable" || list[0].IsEnabled != true || list[0].Cron != "* * * * * *" {
		t.Fatalf("List = %+v, want resumable enabled with the stored expression", list)
	}
	s2.Stop()
}

// TestSchedulerUpdateIntervalRejected pins preventCustomInterval and invalid
// expressions (JobManager.updateIntervalAsync throws both).
func TestSchedulerUpdateIntervalRejected(t *testing.T) {
	s := jobs.New(nil, nil)
	s.Register(jobs.Definition{
		Name:                  "fixed",
		Schedule:              jobs.ScheduleEveryHour,
		Run:                   func(context.Context) error { return nil },
		PreventCustomInterval: true,
	})
	if err := s.UpdateInterval(t.Context(), "fixed", "* * * * * *"); err == nil ||
		err.Error() != "Custom cron expressions are not allowed for job fixed" {
		t.Fatalf("err = %v, want the upstream preventCustomInterval message", err)
	}
	if err := s.UpdateInterval(t.Context(), "fixed", "not a cron"); err == nil {
		t.Fatal("invalid expression accepted")
	}
}

// TestSchedulerPublishesTaskStatusEvents pins the status event contract: the
// JSON of packages/cron-job-status published on the bus topic
// events.cronjob.<name>, with running (lastExecutionStatus carried over /
// null) then idle (success|error) and the run-start timestamp kept.
func TestSchedulerPublishesTaskStatusEvents(t *testing.T) {
	b := bus.New()
	defer b.Close()
	sub := b.Subscribe("events.cronjob.*")
	defer sub.Unsubscribe()

	s := jobs.New(nil, b)
	fail := false
	s.Register(jobs.Definition{
		Name:     "ev",
		Schedule: "* * * * *",
		Run: func(context.Context) error {
			if fail {
				return fmt.Errorf("boom")
			}
			return nil
		},
	})

	if err := s.Trigger(t.Context(), "ev"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	// First run: running(null) → idle(success).
	running := readStatus(t, sub)
	if running.Name != "ev" || running.Status != jobs.StatusRunning || running.LastExecutionStatus != nil {
		t.Fatalf("running status = %+v, want running with null lastExecutionStatus", running)
	}
	startedAt := running.LastExecutionTimestamp
	if len(startedAt) != 24 || startedAt[10] != 'T' || startedAt[23] != 'Z' {
		t.Fatalf("timestamp = %q, want ISO-8601 with milliseconds like Date.toISOString", startedAt)
	}
	idle := readStatus(t, sub)
	if idle.Status != jobs.StatusIdle || idle.LastExecutionStatus == nil || *idle.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("idle status = %+v, want idle/success", idle)
	}
	if idle.LastExecutionTimestamp != startedAt {
		t.Fatalf("timestamp changed on completion: %q -> %q (upstream keeps the run-start one)", startedAt, idle.LastExecutionTimestamp)
	}

	// Failing run: running(success carried over) → idle(error).
	fail = true
	if err := s.Trigger(t.Context(), "ev"); err == nil {
		t.Fatal("Trigger must return the run error")
	}
	running2 := readStatus(t, sub)
	if running2.Status != jobs.StatusRunning || running2.LastExecutionStatus == nil || *running2.LastExecutionStatus != jobs.StatusSuccess {
		t.Fatalf("running status = %+v, want previous success carried over", running2)
	}
	idle2 := readStatus(t, sub)
	if idle2.LastExecutionStatus == nil || *idle2.LastExecutionStatus != jobs.StatusError {
		t.Fatalf("idle status = %+v, want idle/error", idle2)
	}

	// Raw payload shape check (exact upstream JSON members, including the
	// null lastExecutionStatus of the first run — re-marshaled here).
	raw, err := json.Marshal(running)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	for _, key := range []string{"name", "status", "lastExecutionTimestamp", "lastExecutionStatus"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("payload %s missing key %q", raw, key)
		}
	}
}

// TestSchedulerStatusTopicWildcard pins the topic format
// events.cronjob.<name>.
func TestSchedulerStatusTopicWildcard(t *testing.T) {
	if got := jobs.StatusTopic("ping"); got != "events.cronjob.ping" {
		t.Fatalf("StatusTopic = %q", got)
	}
}

// TestSchedulerLastStatusSurvivesRestart pins the in-process last-data
// retention (the equivalent of upstream's Redis "pubSub:last:cron-job-status"
// keys): Stop/Start keeps the last run status; the persisted part across
// process restarts is the configuration row only (see JOBS.md).
func TestSchedulerLastStatusSurvivesRestart(t *testing.T) {
	var counter atomic.Int64
	s := jobs.New(nil, nil)
	s.Register(counterJob("sticky", "* * * * *", &counter))

	if err := s.Trigger(t.Context(), "sticky"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	before, ok := s.LastStatus("sticky")
	if !ok {
		t.Fatal("no status before restart")
	}

	s.Stop()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	after, ok := s.LastStatus("sticky")
	if !ok {
		t.Fatal("last run status lost across Stop/Start")
	}
	if after != before {
		t.Fatalf("status changed across restart: %+v -> %+v", before, after)
	}
	s.Stop()
}

// TestSchedulerRunOnStart pins runOnStart (analytics/iconsUpdater set it
// upstream): the callback runs once synchronously during Start.
func TestSchedulerRunOnStart(t *testing.T) {
	var counter atomic.Int64
	s := jobs.New(nil, nil)
	def := counterJob("boot", "* * * * *", &counter)
	def.RunOnStart = true
	s.Register(def)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if counter.Load() != 1 {
		t.Fatalf("counter = %d, want 1 (run-on-start is synchronous)", counter.Load())
	}
	s.Stop()
}

// TestSchedulerListFiltersHidden pins the getJobs read model: hidden jobs are
// excluded (analytics upstream), flags and configuration defaults surface.
func TestSchedulerListFiltersHidden(t *testing.T) {
	s := jobs.New(nil, nil)
	s.Register(
		jobs.Definition{
			Name:                   "analytics",
			Schedule:               jobs.ScheduleEveryWeek,
			Run:                    func(context.Context) error { return nil },
			RunOnStart:             true,
			PreventManualExecution: true,
			PreventCustomInterval:  true,
			Hidden:                 true,
		},
		jobs.Definition{Name: "ping", Schedule: jobs.ScheduleEveryMinute, Run: func(context.Context) error { return nil }},
	)
	list, err := s.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %+v, want only the visible job", list)
	}
	st := list[0]
	if st.Name != "ping" || st.Cron != jobs.ScheduleEveryMinute || !st.IsEnabled ||
		st.PreventManualExecution || st.PreventCustomInterval {
		t.Fatalf("status = %+v", st)
	}
}

// TestSchedulerConcurrentStartStop pins the concurrency safety of the public
// API: Start/Stop/StartJob/StopJob/Trigger/List race cleanly (run with -race
// in CI).
func TestSchedulerConcurrentStartStop(t *testing.T) {
	database := db.OpenTest(t)
	var counter atomic.Int64
	s := jobs.New(database, nil)
	s.Register(counterJob("racer", "* * * * *", &counter))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(700 * time.Millisecond)
			for time.Now().Before(deadline) {
				_ = s.Start(ctx)
				_ = s.Trigger(ctx, "racer")
				_ = s.StopJob("racer")
				_ = s.StartJob(ctx, "racer")
				_, _ = s.List(ctx)
				s.Stop()
			}
		}()
	}
	wg.Wait()

	// Leave the scheduler stopped and verify the API still works.
	if err := s.Trigger(ctx, "racer"); err != nil {
		t.Fatalf("Trigger after churn: %v", err)
	}
	s.Stop()
}

// TestSchedulerRegisterValidation pins the fail-fast registry: duplicate
// names, invalid schedules, nil runs and topic-unsafe names panic.
func TestSchedulerRegisterValidation(t *testing.T) {
	ok := func(context.Context) error { return nil }
	cases := []struct {
		name string
		def  jobs.Definition
	}{
		{"duplicate", jobs.Definition{Name: "dup", Schedule: "* * * * *", Run: ok}},
		{"invalid schedule", jobs.Definition{Name: "x", Schedule: "not a cron", Run: ok}},
		{"nil run", jobs.Definition{Name: "x", Schedule: "* * * * *"}},
		{"name with dot", jobs.Definition{Name: "a.b", Schedule: "* * * * *", Run: ok}},
		{"name with space", jobs.Definition{Name: "a b", Schedule: "* * * * *", Run: ok}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := jobs.New(nil, nil)
			if tc.name == "duplicate" {
				s.Register(jobs.Definition{Name: "dup", Schedule: "* * * * *", Run: ok})
			}
			defer func() {
				if rec := recover(); rec == nil {
					t.Fatal("expected panic")
				}
			}()
			s.Register(tc.def)
		})
	}
}
