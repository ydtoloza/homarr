package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// TaskStatus is the Go port of the upstream TaskStatus
// (packages/cron-job-status/src/index.ts): the lifecycle state one job run
// publishes. Upstream sends it through the Redis channel
// "pubSub:cron-job-status:<name>" with the last published copy stored under
// "pubSub:last:cron-job-status:<name>"; the port publishes the identical JSON
// on the bus topic "events.cronjob.<name>" (internal/bus convention
// events.<domain>.<id>) and keeps the last copy in the Scheduler (single
// process, same last-data semantics).
//
// The cronJobs tRPC subscription cronJobs.subscribeToStatusUpdates
// (packages/api/src/router/cron-jobs.ts) streams exactly this shape to the
// manage/tasks page; the M4 WebSocket bridge maps it by subscribing to
// "events.cronjob.*".
type TaskStatus struct {
	// Name is the job name.
	Name string `json:"name"`
	// Status is "running" while the callback executes, "idle" otherwise.
	Status StatusState `json:"status"`
	// LastExecutionTimestamp is the ISO-8601 timestamp of the run START
	// (beforeCallbackAsync sets it; success/error keep it).
	LastExecutionTimestamp string `json:"lastExecutionTimestamp"`
	// LastExecutionStatus is "success" or "error" for the finished run, null
	// before the first completion.
	LastExecutionStatus *StatusOutcome `json:"lastExecutionStatus"`
}

// StatusState is the lifecycle phase of a job.
type StatusState string

// The two lifecycle phases published by upstream.
const (
	StatusRunning StatusState = "running"
	StatusIdle    StatusState = "idle"
)

// StatusOutcome is the result of the last completed run.
type StatusOutcome string

// The two outcomes published by upstream.
const (
	StatusSuccess StatusOutcome = "success"
	StatusError   StatusOutcome = "error"
)

// StatusTopic is the bus topic for a job's TaskStatus events
// ("events.cronjob.<name>").
func StatusTopic(name string) string { return "events.cronjob." + name }

// isoTimestamp formats a time like JavaScript's Date.prototype.toISOString
// (UTC, millisecond precision): "2026-11-06T12:34:56.789Z".
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// marshalStatus serializes a TaskStatus without HTML escaping, matching the
// JSON conventions of the rest of the port.
func marshalStatus(ts TaskStatus) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ts); err != nil {
		return nil, fmt.Errorf("jobs: marshal status: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
