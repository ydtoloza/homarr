package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Persistence of job configuration in the upstream "cron_job_configuration"
// table. The table (created by migration
// packages/db/migrations/sqlite/0033_add_cron_job_configuration.sql) holds
// EXACTLY three columns — the run status (last run time / last outcome) is
// NOT persisted there upstream: it lives in the Redis last-data key
// "pubSub:last:cron-job-status:<name>" (see status.go). Keeping the same
// columns is what makes an upstream database keep working:
//
//	CREATE TABLE `cron_job_configuration` (
//	  `name` text PRIMARY KEY NOT NULL,
//	  `cron_expression` text NOT NULL,
//	  `is_enabled` integer DEFAULT true NOT NULL
//	);
//
// The write semantics mirror JobManager.updateConfigurationAsync
// (packages/cron-jobs/src/job-manager.ts): UPDATE the row when it exists
// (only the provided fields), otherwise INSERT one with the definition's
// default schedule and is_enabled=true (or the explicit override, so
// disableJob on a job without a row inserts {name, default cron, false}).

// jobConfiguration is one "cron_job_configuration" row.
type jobConfiguration struct {
	Name           string
	CronExpression string
	IsEnabled      bool
}

// loadConfigurations reads every row (initializeAsync:
// db.query.cronJobConfigurations.findMany()).
func (s *Scheduler) loadConfigurations(ctx context.Context) (map[string]jobConfiguration, error) {
	if s.database == nil {
		return map[string]jobConfiguration{}, nil
	}
	rows, err := s.database.QueryContext(ctx,
		`SELECT name, cron_expression, is_enabled FROM "cron_job_configuration"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]jobConfiguration{}
	for rows.Next() {
		var (
			c       jobConfiguration
			enabled int64
		)
		if err := rows.Scan(&c.Name, &c.CronExpression, &enabled); err != nil {
			return nil, err
		}
		c.IsEnabled = enabled != 0
		out[c.Name] = c
	}
	return out, rows.Err()
}

// loadConfiguration reads one row; nil when absent (findFirst semantics).
func (s *Scheduler) loadConfiguration(ctx context.Context, name string) (*jobConfiguration, error) {
	if s.database == nil {
		return nil, nil
	}
	var (
		c       jobConfiguration
		enabled int64
	)
	err := s.database.QueryRowContext(ctx,
		`SELECT name, cron_expression, is_enabled FROM "cron_job_configuration" WHERE name = ?`,
		name).Scan(&c.Name, &c.CronExpression, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.IsEnabled = enabled != 0
	return &c, nil
}

// saveConfiguration applies the patch with the upstream upsert semantics and
// returns the effective configuration after the write.
func (s *Scheduler) saveConfiguration(ctx context.Context, name string, cronExpression *string, enabled *bool) (jobConfiguration, error) {
	if s.database == nil {
		return jobConfiguration{Name: name}, errors.New("jobs: no database configured for cron_job_configuration")
	}
	existing, err := s.loadConfiguration(ctx, name)
	if err != nil {
		return jobConfiguration{}, fmt.Errorf("jobs: load configuration of job %s: %w", name, err)
	}

	if existing != nil {
		// UPDATE only the provided fields (upstream .set({...configuration,
		// name: undefined}) — the primary key is never rewritten).
		if cronExpression != nil {
			if _, err := s.database.ExecContext(ctx,
				`UPDATE "cron_job_configuration" SET cron_expression = ? WHERE name = ?`,
				*cronExpression, name); err != nil {
				return jobConfiguration{}, fmt.Errorf("jobs: update configuration of job %s: %w", name, err)
			}
		}
		if enabled != nil {
			if _, err := s.database.ExecContext(ctx,
				`UPDATE "cron_job_configuration" SET is_enabled = ? WHERE name = ?`,
				boolToInt(*enabled), name); err != nil {
				return jobConfiguration{}, fmt.Errorf("jobs: update configuration of job %s: %w", name, err)
			}
		}
		out := *existing
		if cronExpression != nil {
			out.CronExpression = *cronExpression
		}
		if enabled != nil {
			out.IsEnabled = *enabled
		}
		return out, nil
	}

	// INSERT with defaults for the fields not provided: the definition's
	// default schedule and is_enabled=true (JobManager.updateConfigurationAsync).
	def, ok := s.defsLocked(name)
	if !ok {
		return jobConfiguration{}, s.unknownJobError(name)
	}
	cronExpr := def.Schedule
	if cronExpression != nil {
		cronExpr = *cronExpression
	}
	isEnabled := true
	if enabled != nil {
		isEnabled = *enabled
	}
	if _, err := s.database.ExecContext(ctx,
		`INSERT INTO "cron_job_configuration" (name, cron_expression, is_enabled) VALUES (?, ?, ?)`,
		name, cronExpr, boolToInt(isEnabled)); err != nil {
		return jobConfiguration{}, fmt.Errorf("jobs: insert configuration of job %s: %w", name, err)
	}
	return jobConfiguration{Name: name, CronExpression: cronExpr, IsEnabled: isEnabled}, nil
}

func (s *Scheduler) defsLocked(name string) (Definition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.defs[name]
	return def, ok
}

// boolToInt encodes the drizzle boolean mode ("integer 0/1") of the column.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
