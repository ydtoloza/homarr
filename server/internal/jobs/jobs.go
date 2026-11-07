// Package jobs is the Go port of the upstream cron job stack
// (packages/cron-jobs + packages/cron-jobs-core + packages/cron-job-status,
// started by apps/tasks): a registry of named jobs with cron schedules, a
// scheduler built on robfig/cron/v3, manual triggering, per-job configuration
// persisted in the upstream "cron_job_configuration" table, and job status
// events published on the in-process bus (internal/bus), the replacement of
// upstream's Redis channels.
//
// Upstream contract (see JOBS.md for the full pinning):
//
//   - jobs are created with createCronJob(name, cronExpression, options) and
//     grouped by createCronJobGroup (packages/cron-jobs-core/src/creator.ts,
//     group.ts); the registry keys are the job names ("analytics",
//     "iconsUpdater", "ping").
//   - apps/tasks (src/main.ts) runs initializeAsync() + startAllAsync() at
//     startup: jobs whose "cron_job_configuration" row has is_enabled=false
//     stay unscheduled; the rest run on their configured expression.
//   - every execution publishes a TaskStatus on the Redis channel
//     "cron-job-status:<name>" (packages/cron-job-status/src/publisher.ts);
//     the port publishes the same JSON on bus topic "events.cronjob.<name>".
//   - the cronJobs tRPC router (packages/api/src/router/cron-jobs.ts) exposes
//     triggerJob/startJob/stopJob/updateJobInterval/disableJob/enableJob/
//     getJobs on top of the JobManager (packages/cron-jobs/src/job-manager.ts)
//     — the Scheduler methods below are its 1:1 surface for the M4 router.
//
// Schedules are 5-field cron expressions (seconds optional 6th field, like
// upstream's croner usage: EVERY_MINUTE "* * * * *" vs EVERY_5_SECONDS
// "*/5 * * * * *", packages/cron-jobs-core/src/expressions.ts).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// defaultExpectedDuration mirrors upstream's expectedMaximumDurationInMillis
// default (2500 ms, packages/cron-jobs-core/src/creator.ts): runs taking
// longer log a warning.
const defaultExpectedDuration = 2500 * time.Millisecond

// Definition is one registered job — the port of upstream's JobCallback shape
// (packages/cron-jobs-core/src/creator.ts) plus its createCronJob options.
type Definition struct {
	// Name is the registry key and the "cron_job_configuration"."name"
	// primary key (upstream: "analytics", "iconsUpdater", "ping"). It is also
	// the id of the bus topic "events.cronjob.<name>", so it must not contain
	// dots or whitespace.
	Name string
	// Schedule is the default cron expression (5 fields, seconds optional —
	// e.g. "* * * * *"). A "cron_job_configuration" row overrides it unless
	// PreventCustomInterval is set.
	Schedule string
	// Run is the job callback. Errors mark the run as failed in the status
	// events but never crash the scheduler (upstream catches and logs).
	Run func(ctx context.Context) error

	// RunOnStart executes once when the job is started (upstream runOnStart:
	// analytics and iconsUpdater set it).
	RunOnStart bool
	// PreventManualExecution rejects Trigger (upstream preventManualExecution:
	// analytics). The error message mirrors runManuallyAsync.
	PreventManualExecution bool
	// PreventCustomInterval ignores the configured schedule override (upstream
	// preventCustomInterval: analytics).
	PreventCustomInterval bool
	// Hidden excludes the job from List (upstream hidden: analytics, so the
	// tasks page only shows operational jobs).
	Hidden bool
	// ExpectedDuration logs a warning when a run exceeds it. Zero uses the
	// upstream default of 2500 ms.
	ExpectedDuration time.Duration
}

// Status is the read model of List — the response shape of the cronJobs
// getJobs procedure (JobManager.getAllAsync,
// packages/cron-jobs/src/job-manager.ts).
type Status struct {
	Name                   string `json:"name"`
	Cron                   string `json:"cron"`
	PreventManualExecution bool   `json:"preventManualExecution"`
	PreventCustomInterval  bool   `json:"preventCustomInterval"`
	IsEnabled              bool   `json:"isEnabled"`
}

// Options configures the Scheduler.
type Options struct {
	// Logger receives lifecycle/run logs; nil uses slog.Default().
	Logger *slog.Logger
}

// Scheduler runs registered jobs on their cron schedules. It is safe for
// concurrent use: Start/Stop/Register/Trigger and the per-job management
// methods may be called from any goroutine.
type Scheduler struct {
	database *db.DB
	bus      *bus.Bus
	logger   *slog.Logger
	parser   cron.Parser

	// mu guards the registry, the schedule entries and the cron loop state.
	mu      sync.Mutex
	defs    map[string]Definition
	entries map[string]cron.EntryID
	cron    *cron.Cron
	loopUp  bool

	// runCtx cancels in-flight job runs on Stop. It is created lazily from
	// the first Start's ctx (or Background for per-job starts before Start).
	runCtx    context.Context
	runCancel context.CancelFunc

	// statusMu guards the last published status per job (the equivalent of
	// upstream's Redis "pubSub:last:cron-job-status:<name>" last-data store).
	statusMu sync.Mutex
	lastData map[string]TaskStatus
}

// New creates an empty Scheduler. database is used to load/save the
// "cron_job_configuration" rows; bus (optional, nil allowed) receives the job
// status and job result events.
func New(database *db.DB, bus *bus.Bus, opts ...Options) *Scheduler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		database: database,
		bus:      bus,
		logger:   logger,
		parser: cron.NewParser(
			cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
		),
		defs:     make(map[string]Definition),
		entries:  make(map[string]cron.EntryID),
		lastData: make(map[string]TaskStatus),
	}
}

// Register adds jobs to the registry (upstream jobRegistry.set in
// createJobGroupCreator, packages/cron-jobs-core/src/group.ts). It panics on
// a duplicate name, an invalid name or schedule, or a nil Run — the same
// fail-fast a TS object literal with duplicate keys gives at compile time.
// Register must be called before Start (re-registering after Start is not
// supported).
func (s *Scheduler) Register(defs ...Definition) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, def := range defs {
		if err := validName(def.Name); err != nil {
			panic(fmt.Sprintf("jobs: Register(%q): %v", def.Name, err))
		}
		if _, err := s.parser.Parse(def.Schedule); err != nil {
			panic(fmt.Sprintf("jobs: Register(%q): invalid schedule %q: %v", def.Name, def.Schedule, err))
		}
		if def.Run == nil {
			panic(fmt.Sprintf("jobs: Register(%q): nil Run", def.Name))
		}
		if _, ok := s.defs[def.Name]; ok {
			panic(fmt.Sprintf("jobs: job %q registered twice", def.Name))
		}
		s.defs[def.Name] = def
	}
}

// Start initializes the scheduler from the database and schedules every
// enabled job — the port of apps/tasks main.ts startup
// (jobGroup.initializeAsync() + jobGroup.startAllAsync()). Jobs whose
// "cron_job_configuration" row has is_enabled=false stay unscheduled; the
// others run on their configured expression (row override) or the definition
// default. Jobs with RunOnStart execute once, synchronously, after
// scheduling. Start is idempotent while running; ctx scopes the
// initialization work AND the lifetime of scheduled runs (in-flight runs are
// cancelled by Stop, not by ctx expiry).
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.loopUp {
		s.mu.Unlock()
		return nil
	}
	if err := s.ensureLoopLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.ensureRunContextLocked(ctx)

	configs, err := s.loadConfigurations(ctx)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("jobs: load cron_job_configuration: %w", err)
	}

	var onStart []Definition
	scheduled := 0
	for _, def := range sortedDefs(s.defs) {
		var cfg *jobConfiguration
		if c, ok := configs[def.Name]; ok {
			cfg = &c
		}
		if cfg != nil && !cfg.IsEnabled {
			// upstream initializeAsync: disabled jobs get no task.
			continue
		}
		if _, err := s.scheduleLocked(def, cfg); err != nil {
			// A hand-edited cron_expression must not take down startup;
			// upstream would throw inside initializeAsync (croner throws on an
			// invalid pattern). Log and skip instead.
			s.logger.Error("jobs: scheduling job failed", "job", def.Name, "err", err)
			continue
		}
		scheduled++
		if def.RunOnStart {
			onStart = append(onStart, def)
		}
	}
	s.mu.Unlock()

	s.logger.Info("jobs: scheduler started", "jobs", scheduled)
	// Upstream startAsync runs onStartAsync (which executes runOnStart
	// callbacks) before resuming the schedule; do the same, outside the lock.
	for _, def := range onStart {
		if err := s.execute(ctx, def); err != nil {
			s.logger.Error("jobs: run-on-start failed", "job", def.Name, "err", err)
		}
	}
	return nil
}

// Stop stops the scheduler, cancels in-flight runs and clears the schedule
// (the registered definitions are kept; Start can be called again and
// resumes from the persisted configuration). Safe to call when not running.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.loopUp {
		s.mu.Unlock()
		return
	}
	if s.runCancel != nil {
		s.runCancel()
		s.runCancel = nil
	}
	// robfig Stop waits for running jobs; runCtx cancellation above makes
	// ctx-aware jobs return promptly. The wait happens OUTSIDE the lock so a
	// finishing job can still take the scheduler lock (status publishing).
	stopped := s.cron.Stop()
	s.cron = nil
	s.loopUp = false
	for name := range s.entries {
		delete(s.entries, name)
	}
	s.mu.Unlock()
	<-stopped.Done()
	s.logger.Info("jobs: scheduler stopped")
}

// Trigger runs the job immediately and synchronously — the port of
// runManuallyAsync behind the cronJobs.triggerJob mutation
// (packages/api/src/router/cron-jobs.ts). Deviation from upstream (which
// fire-and-forgets croner's task.trigger()): the run is awaited, which gives
// the tRPC mutation natural completion semantics and deterministic tests.
// It rejects unknown jobs and jobs with PreventManualExecution (same message
// as upstream).
func (s *Scheduler) Trigger(ctx context.Context, name string) error {
	def, err := s.definition(name)
	if err != nil {
		return err
	}
	if def.PreventManualExecution {
		return fmt.Errorf("The job %q can not be executed manually.", def.Name)
	}
	return s.execute(ctx, def)
}

// StartJob schedules a single job (cronJobs.startJob → JobManager.startAsync
// → jobGroup.startAsync): it is a no-op when already scheduled, runs the
// RunOnStart callback like upstream's onStartAsync, and works regardless of
// the is_enabled flag (upstream startAsync does not check it either — that
// gate only applies to the bulk startup in initializeAsync).
func (s *Scheduler) StartJob(ctx context.Context, name string) error {
	s.mu.Lock()
	if !s.loopUp {
		if err := s.ensureLoopLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.ensureRunContextLocked(nil)
	}
	def, ok := s.defs[name]
	if !ok {
		s.mu.Unlock()
		return s.unknownJobError(name)
	}
	if _, scheduled := s.entries[name]; scheduled {
		s.mu.Unlock()
		return nil
	}
	var cfg *jobConfiguration
	if s.database != nil {
		c, err := s.loadConfiguration(ctx, name)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if c != nil {
			cfg = c
		}
	}
	if _, err := s.scheduleLocked(def, cfg); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	if def.RunOnStart {
		if err := s.execute(ctx, def); err != nil {
			s.logger.Error("jobs: run-on-start failed", "job", def.Name, "err", err)
		}
	}
	s.logger.Info("jobs: job schedule started", "job", name)
	return nil
}

// StopJob pauses a single job (cronJobs.stopJob → JobManager.stop): the
// definition and its configuration row stay intact; StartJob resumes it.
func (s *Scheduler) StopJob(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.defs[name]; !ok {
		return s.unknownJobError(name)
	}
	id, scheduled := s.entries[name]
	if !scheduled {
		return nil
	}
	s.cron.Remove(id)
	delete(s.entries, name)
	s.logger.Info("jobs: job schedule stopped", "job", name)
	return nil
}

// Enable persists is_enabled=true and schedules the job
// (cronJobs.enableJob → JobManager.enableAsync).
func (s *Scheduler) Enable(ctx context.Context, name string) error {
	if _, err := s.definition(name); err != nil {
		return err
	}
	if _, err := s.saveConfiguration(ctx, name, nil, boolPtr(true)); err != nil {
		return err
	}
	return s.StartJob(ctx, name)
}

// Disable persists is_enabled=false and pauses the job
// (cronJobs.disableJob → JobManager.disableAsync). Like upstream, disabling
// a job without a configuration row inserts one (with the default schedule).
func (s *Scheduler) Disable(ctx context.Context, name string) error {
	if _, err := s.definition(name); err != nil {
		return err
	}
	if _, err := s.saveConfiguration(ctx, name, nil, boolPtr(false)); err != nil {
		return err
	}
	return s.StopJob(name)
}

// UpdateInterval persists a new cron expression and reschedules the job if it
// is currently scheduled (cronJobs.updateJobInterval →
// JobManager.updateIntervalAsync). Jobs with PreventCustomInterval reject
// overrides; the expression must parse.
func (s *Scheduler) UpdateInterval(ctx context.Context, name, expression string) error {
	def, err := s.definition(name)
	if err != nil {
		return err
	}
	if def.PreventCustomInterval {
		return fmt.Errorf("Custom cron expressions are not allowed for job %s", def.Name)
	}
	if _, err := s.parser.Parse(expression); err != nil {
		return fmt.Errorf("jobs: invalid cron expression %q: %w", expression, err)
	}
	saved, err := s.saveConfiguration(ctx, name, &expression, nil)
	if err != nil {
		return err
	}

	// Upstream replaces the task with a new Cron(..., {paused:
	// !updatedConfig.isEnabled}); the port updates the row and, when the job
	// is currently scheduled, swaps the entry in place (enabled → new
	// expression; disabled → stays stopped, like paused). A job that is not
	// scheduled picks the new expression at its next Start/Enable.
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, scheduled := s.entries[name]; scheduled {
		s.cron.Remove(id)
		delete(s.entries, name)
		if saved.IsEnabled {
			if _, err := s.scheduleLocked(def, &saved); err != nil {
				return err
			}
		}
	}
	s.logger.Info("jobs: job interval updated", "job", name, "expression", expression)
	return nil
}

// List returns every non-hidden job with its effective schedule — the
// JobManager.getAllAsync shape consumed by the cronJobs getJobs procedure
// (hidden jobs are filtered, missing configuration rows default to enabled
// with the definition's schedule).
func (s *Scheduler) List(ctx context.Context) ([]Status, error) {
	configs, err := s.loadConfigurations(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: load cron_job_configuration: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Status
	for _, def := range sortedDefs(s.defs) {
		if def.Hidden {
			continue
		}
		st := Status{
			Name:                   def.Name,
			Cron:                   def.Schedule,
			PreventManualExecution: def.PreventManualExecution,
			PreventCustomInterval:  def.PreventCustomInterval,
			IsEnabled:              true,
		}
		if cfg, ok := configs[def.Name]; ok {
			if cfg.CronExpression != "" && !def.PreventCustomInterval {
				st.Cron = cfg.CronExpression
			}
			st.IsEnabled = cfg.IsEnabled
		}
		out = append(out, st)
	}
	return out, nil
}

// LastStatus returns the last published TaskStatus of a job, or false when
// the job never ran in this process (the equivalent of the Redis
// "pubSub:last:cron-job-status:<name>" last-data read).
func (s *Scheduler) LastStatus(name string) (TaskStatus, bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	ts, ok := s.lastData[name]
	return ts, ok
}

// --- internals --------------------------------------------------------------

// definition returns a registered definition (lock-free copy).
func (s *Scheduler) definition(name string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.defs[name]
	if !ok {
		return Definition{}, s.unknownJobError(name)
	}
	return def, nil
}

func (s *Scheduler) unknownJobError(name string) error {
	// Mirrors JobManager: `throw new Error(`Job ${name} not found`)`.
	return fmt.Errorf("Job %s not found", name)
}

// ensureLoopLocked lazily creates and starts the robfig cron loop. The loop
// outlives Stop — Stop only clears the entries and cancels runCtx — so
// per-job StartJob/StopJob work even without a global Start.
func (s *Scheduler) ensureLoopLocked() error {
	if s.loopUp {
		return nil
	}
	if s.cron == nil {
		s.cron = cron.New(cron.WithParser(s.parser))
	}
	s.cron.Start()
	s.loopUp = true
	return nil
}

// ensureRunContextLocked creates the context scheduled runs inherit. Start
// derives it from the caller's ctx so graceful shutdown propagates; per-job
// starts before the first Start fall back to Background.
func (s *Scheduler) ensureRunContextLocked(ctx context.Context) {
	if s.runCancel != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.runCtx, s.runCancel = context.WithCancel(ctx)
}

// scheduleLocked adds the cron entry for def, honoring the persisted
// configuration override (upstream createTask: configuration?.cronExpression
// ?? default, unless preventCustomInterval). Callers must hold s.mu.
func (s *Scheduler) scheduleLocked(def Definition, cfg *jobConfiguration) (cron.EntryID, error) {
	expression := def.Schedule
	if cfg != nil && !def.PreventCustomInterval && cfg.CronExpression != "" {
		expression = cfg.CronExpression
	}
	// Capture the values: the closure fires on a robfig goroutine without
	// holding s.mu, and a Stop/Start cycle replaces s.runCtx while old
	// entries may still be finishing.
	defCopy := def
	runCtx := s.runCtx
	id, err := s.cron.AddFunc(expression, func() {
		s.execute(runCtx, defCopy)
	})
	if err != nil {
		return 0, fmt.Errorf("jobs: add schedule %q for job %s: %w", expression, def.Name, err)
	}
	s.entries[def.Name] = id
	return id, nil
}

// execute runs one job execution with the upstream lifecycle: publish the
// "running" status, run the callback, publish the final status
// (packages/cron-job-status/src/publisher.ts + the catchingCallbackAsync
// wrapper of packages/cron-jobs-core/src/creator.ts). Transport-level errors
// of the callback are returned but never panic the scheduler.
func (s *Scheduler) execute(ctx context.Context, def Definition) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	started := time.Now()
	// beforeCallbackAsync: timestamp = now, lastExecutionStatus carried over
	// from the previous run, status "running".
	s.publishStatus(TaskStatus{
		Name:                   def.Name,
		Status:                 StatusRunning,
		LastExecutionTimestamp: isoTimestamp(started),
		LastExecutionStatus:    s.previousStatus(def.Name),
	})

	expected := def.ExpectedDuration
	if expected <= 0 {
		expected = defaultExpectedDuration
	}

	runErr := def.Run(ctx)
	if elapsed := time.Since(started); elapsed > expected {
		s.logger.Warn("jobs: run took longer than expected",
			"job", def.Name, "duration_ms", elapsed.Milliseconds(), "expected_ms", expected.Milliseconds())
	}

	final := StatusSuccess
	if runErr != nil {
		final = StatusError
		s.logger.Error("jobs: run failed", "job", def.Name, "err", runErr)
	}
	// onCallbackSuccess/onCallbackError: status "idle", the timestamp stays
	// the run-start one (previous?.lastExecutionTimestamp ?? now).
	s.publishStatus(TaskStatus{
		Name:                   def.Name,
		Status:                 StatusIdle,
		LastExecutionTimestamp: isoTimestamp(started),
		LastExecutionStatus:    &final,
	})
	return runErr
}

// previousStatus reads the lastExecutionStatus of the previous run, nil when
// the job never ran (beforeCallbackAsync: previous?.lastExecutionStatus ??
// null).
func (s *Scheduler) previousStatus(name string) *StatusOutcome {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if prev, ok := s.lastData[name]; ok && prev.LastExecutionStatus != nil {
		p := *prev.LastExecutionStatus
		return &p
	}
	return nil
}

// publishStatus stores the status as last-data and publishes it on the bus
// topic "events.cronjob.<name>". A nil bus skips only the publish.
func (s *Scheduler) publishStatus(ts TaskStatus) {
	s.statusMu.Lock()
	s.lastData[ts.Name] = ts
	s.statusMu.Unlock()
	if s.bus == nil {
		return
	}
	payload, err := marshalStatus(ts)
	if err != nil {
		s.logger.Error("jobs: marshal task status", "job", ts.Name, "err", err)
		return
	}
	s.bus.Publish(StatusTopic(ts.Name), payload)
}

func boolPtr(b bool) *bool { return &b }

// sortedDefs returns the definitions ordered by name so registry iteration is
// deterministic (upstream iterates object keys, which are insertion-ordered).
func sortedDefs(defs map[string]Definition) []Definition {
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Definition, 0, len(names))
	for _, name := range names {
		out = append(out, defs[name])
	}
	return out
}

// validName enforces the bus topic invariants of a job name (it becomes the
// id segment of "events.cronjob.<name>").
func validName(name string) error {
	if name == "" {
		return errors.New("empty Name")
	}
	for _, r := range name {
		if r == '.' || r == ' ' || r == '\t' || r == '\n' {
			return errors.New("name must not contain dots or whitespace (bus topic separator)")
		}
	}
	return nil
}
