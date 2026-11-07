// Default job definitions of the port. Upstream registers three jobs in the
// job group (packages/cron-jobs/src/index.ts): analytics, iconsUpdater and
// ping. Only the ping job is ported for now — analytics needs the analytics
// sender (@homarr/analytics) and iconsUpdater the icon fetcher
// (@homarr/icons), both deferred to M4; the Scheduler accepts any set of
// definitions, so they land as plain Register calls.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	// Registers the "ping" integration into the default registry (the
	// database/sql driver pattern): the factory resolution in PingJob relies
	// on it.
	_ "github.com/ydtoloza/homarr/server/internal/integrations/ping"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/ping"
)

// Default schedules, the port of packages/cron-jobs-core/src/expressions.ts
// (croner syntax: 5 fields, seconds optional 6th).
const (
	// ScheduleEvery5Seconds mirrors EVERY_5_SECONDS = "*/5 * * * * *".
	ScheduleEvery5Seconds = "*/5 * * * * *"
	// ScheduleEvery30Seconds mirrors EVERY_30_SECONDS = "*/30 * * * * *".
	ScheduleEvery30Seconds = "*/30 * * * * *"
	// ScheduleEveryMinute mirrors EVERY_MINUTE = "* * * * *".
	ScheduleEveryMinute = "* * * * *"
	// ScheduleEvery5Minutes mirrors EVERY_5_MINUTES = "*/5 * * * *".
	ScheduleEvery5Minutes = "*/5 * * * *"
	// ScheduleEvery10Minutes mirrors EVERY_10_MINUTES = "*/10 * * * *".
	ScheduleEvery10Minutes = "*/10 * * * *"
	// ScheduleEveryHour mirrors EVERY_HOUR = "0 * * * *".
	ScheduleEveryHour = "0 * * * *"
	// ScheduleEveryDay mirrors EVERY_DAY = "0 0 * * */1".
	ScheduleEveryDay = "0 0 * * */1"
	// ScheduleEveryWeek mirrors EVERY_WEEK = "0 0 * * 1".
	ScheduleEveryWeek = "0 0 * * 1"
)

// PingJobName is the registry key of the ping job. Upstream names it "ping"
// (createCronJob("ping", EVERY_MINUTE, ...) in
// packages/cron-jobs/src/jobs/ping.ts) — the name is stored in
// cron_job_configuration rows by the manage/tasks page, so it must match.
const PingJobName = "ping"

// PingOptions tunes the ping job.
type PingOptions struct {
	// Parallelism caps how many URLs are checked concurrently. Upstream uses
	// Promise.allSettled (unbounded); the port bounds it to protect a small
	// SQLite-backed process. Defaults to 10.
	Parallelism int
	// Logger; nil inherits the scheduler's (slog.Default()).
	Logger *slog.Logger
}

// PingJob returns the "ping" job definition — the port of pingJob
// (packages/cron-jobs/src/jobs/ping.ts):
//
//  1. skip entirely when the server setting board.forceDisableStatus is set
//     (ping.ts:22-26; default false, packages/server-settings/src/index.ts);
//  2. load the ping targets from the database (see loadPingTargets — upstream
//     reads the Redis list "ping-url", which nothing populates in current
//     upstream; the port reads the app rows, the same source widget.app.ping
//     uses);
//  3. check every target through the ping integration (bounded parallelism,
//     each unique URL checked once per run like upstream's Set dedupe);
//  4. publish one bus event per target on "events.ping.<appId>" with the
//     upstream payload shape (ping.Publish).
func PingJob(database *db.DB, bus *bus.Bus, opts ...PingOptions) Definition {
	var o PingOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return Definition{
		Name:     PingJobName,
		Schedule: ScheduleEveryMinute,
		Run: func(ctx context.Context) error {
			return runPing(ctx, database, bus, o, logger)
		},
	}
}

// runPing is the callback body (ping.ts withCallback).
func runPing(ctx context.Context, database *db.DB, bus *bus.Bus, opts PingOptions, logger *slog.Logger) error {
	disabled, err := forceDisableStatus(ctx, database)
	if err != nil {
		return fmt.Errorf("jobs: read board settings: %w", err)
	}
	if disabled {
		logger.Debug("jobs: simple ping is disabled by server settings")
		return nil
	}

	targets, err := loadPingTargets(ctx, database)
	if err != nil {
		return fmt.Errorf("jobs: load ping targets: %w", err)
	}
	if len(targets) == 0 {
		return nil
	}

	// Dedupe by URL (ping.ts: `[...new Set(urls)]`): each unique URL is
	// checked once per run and its result is published for every app on it.
	type target struct {
		appID string
		url   string
	}
	byURL := map[string][]string{} // url -> app ids
	var urls []string
	for _, t := range targets {
		if _, ok := byURL[t.url]; !ok {
			urls = append(urls, t.url)
		}
		byURL[t.url] = append(byURL[t.url], t.appID)
	}

	parallelism := opts.Parallelism
	if parallelism <= 0 {
		parallelism = 10
	}
	sem := make(chan struct{}, parallelism)
	type result struct {
		url string
		res integrations.CheckResult
	}
	results := make(chan result, len(urls))

	// Upstream runs Promise.allSettled: failures become error results, the
	// run itself never fails. The ping integration's Check mirrors that (it
	// only errors when it cannot run, which for ping is never).
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(targetURL string) {
			defer wg.Done()
			// Respect cancellation: skip remaining checks when ctx is done.
			if err := ctx.Err(); err != nil {
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			inst, err := integrations.New(ping.Kind, integrations.Input{
				ID:   "", // the app id is per-target; the check only needs the URL
				Kind: ping.Kind,
				URL:  targetURL,
			})
			if err != nil {
				// The ping factory cannot fail on construction; defensive.
				logger.Error("jobs: build ping integration", "err", err)
				return
			}
			res, err := inst.Check(ctx)
			if err != nil {
				// Per the Integration contract this should not happen for
				// ping; fall back to an error result instead of failing the
				// whole run.
				res = integrations.CheckResult{Error: err.Error()}
			}
			results <- result{url: targetURL, res: res}
		}(u)
	}
	wg.Wait()
	close(results)

	collected := map[string]integrations.CheckResult{}
	for r := range results {
		collected[r.url] = r.res
	}

	for _, t := range targets {
		res, ok := collected[t.url]
		if !ok {
			continue // run was cancelled before this URL was checked
		}
		if res.Error == "" {
			logger.Debug("jobs: ping executed", "url", t.url, "status_code", res.StatusCode)
		}
		if err := ping.Publish(bus, t.appID, t.url, res); err != nil {
			logger.Error("jobs: publish ping result", "app_id", t.appID, "err", err)
		}
	}
	return nil
}

// pingTarget is one app to ping.
type pingTarget struct {
	appID string
	url   string
}

// loadPingTargets returns every app with a pingable URL: app.pingUrl ?? app.href
// (the resolution of widget.app.ping, packages/api/src/router/widgets/app.ts:30;
// the onboard flow creates apps with pingUrl for its featured services). Only
// http(s) URLs are targeted: pingUrl is validated to http(s) upstream
// (packages/validation/src/app.ts:22-29), and hrefs with custom URI schemes
// (vscode://, ...) cannot be meaningfully pinged — attempting them would
// produce an error event per bookmark every minute.
func loadPingTargets(ctx context.Context, database *db.DB) ([]pingTarget, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, COALESCE(NULLIF(ping_url, ''), href) FROM "app"
		 WHERE COALESCE(NULLIF(ping_url, ''), href) IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pingTarget
	for rows.Next() {
		var (
			id  string
			url string
		)
		if err := rows.Scan(&id, &url); err != nil {
			return nil, err
		}
		if isPingableURL(url) {
			out = append(out, pingTarget{appID: id, url: url})
		}
	}
	return out, rows.Err()
}

// isPingableURL reports whether the URL is an absolute http(s) URL.
func isPingableURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// forceDisableStatus reads board.forceDisableStatus from the serverSetting
// table — the same check ping.ts does via getServerSettingByKeyAsync(db,
// "board") (ping.ts:22-26). Missing row or key falls back to the upstream
// default false (packages/server-settings/src/index.ts:146).
func forceDisableStatus(ctx context.Context, database *db.DB) (bool, error) {
	var raw sql.NullString
	err := database.QueryRowContext(ctx,
		`SELECT value FROM "serverSetting" WHERE setting_key = 'board'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !raw.Valid {
		return false, nil
	}
	var envelope struct {
		JSON struct {
			ForceDisableStatus bool `json:"forceDisableStatus"`
		} `json:"json"`
	}
	if err := json.Unmarshal([]byte(raw.String), &envelope); err != nil {
		return false, fmt.Errorf("jobs: parse board settings: %w", err)
	}
	return envelope.JSON.ForceDisableStatus, nil
}
