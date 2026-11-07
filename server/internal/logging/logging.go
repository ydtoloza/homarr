// Package logging is the Go port of the upstream log pipeline
// (packages/core/src/infrastructure/logs): one slog.Handler that renders the
// upstream production line format, persists every entry to the log table of
// the shared SQLite database and publishes it to the event bus topic
// "events.log.<level>", where the tRPC log router (internal/api/router/log)
// serves history and live tail to the frontend.
//
// Upstream shape (all verified against this repo's sources):
//
//   - winston logger with format.combine(colorize, timestamp, errors, printf)
//     (format/index.ts): the rendered line is
//     `<timestamp> <colored level>: <message>[ <metadata>]`, where metadata is
//     `key="value"` pairs of the primitive metadata members (format/metadata.ts
//     filters objects and functions). The colored level uses @colors/colors
//     npm palette (winston config npm: error=red, warn=yellow, info=green,
//     debug=blue), so the stored/published message carries ANSI codes — the
//     SPA renders the message raw in an xterm terminal
//     (apps/nextjs .../manage/tools/logs/terminal.tsx).
//   - RedisTransport (transports/redis-transport.ts): per entry a
//     LoggerMessage {id: uuid, timestamp: Date, message: <the rendered line>,
//     level} is serialized with superjson (Date annotated in meta.values),
//     pushed to a bounded history list (LOG_HISTORY_MAX_ENTRIES = 5000,
//     constants.ts:17) and published to the logging channel. homarr-go
//     replaces the Redis list with the "log" SQLite table (additive, owned by
//     this package — the upstream drizzle schema has no log table) and the
//     Redis publish with bus.Publish.
//   - LOG_LEVEL env semantics (logs/env.ts): one of error|warn|info|debug,
//     default info; entries below the level never reach the transports.
//
// The pipeline is async with a bounded queue: Handle never blocks the caller
// and drops entries when the queue is full (dashboard logs are ephemeral,
// mirroring the drop-on-slow behavior of internal/bus).
package logging

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Level is one of the four upstream log levels (logLevels,
// packages/core/src/infrastructure/logs/constants.ts:3).
type Level string

const (
	LevelError Level = "error"
	LevelWarn  Level = "warn"
	LevelInfo  Level = "info"
	LevelDebug Level = "debug"
)

// levels ordered least to most severe (winston npm levels: higher number =
// lower severity; LOG_LEVEL=info passes error/warn/info but not debug).
var levelSeverity = map[Level]int{
	LevelDebug: 0,
	LevelInfo:  1,
	LevelWarn:  2,
	LevelError: 3,
}

// ParseLevel validates a LOG_LEVEL value (zodEnumFromArray(logLevels),
// logs/env.ts).
func ParseLevel(s string) (Level, bool) {
	l := Level(s)
	_, ok := levelSeverity[l]
	return l, ok
}

// atLeast reports whether l is at least as severe as min.
func (l Level) atLeast(min Level) bool {
	return levelSeverity[l] >= levelSeverity[min]
}

// Entry is one log record: the upstream LoggerMessage
// (loggerMessageSchema, constants.ts:9-14).
type Entry struct {
	ID        string
	Timestamp time.Time
	// Message is the fully rendered console line (timestamp, colored level,
	// text and metadata), exactly what winston puts on info[messageSymbol].
	Message string
	Level   Level
}

// Topic is the bus topic of one entry (the events.<domain>.<id> convention of
// internal/bus; WEBSOCKET.md section 6 maps log.subscribe to events.log.*).
func (l Level) Topic() string {
	return "events.log." + string(l)
}

// ansi color codes of the npm palette used by winston's colorize format
// (@colors/colors: red=31, yellow=33, green=32, blue=34; reset-fg=39).
var levelColor = map[Level]string{
	LevelError: "\x1b[31m",
	LevelWarn:  "\x1b[33m",
	LevelInfo:  "\x1b[32m",
	LevelDebug: "\x1b[34m",
}

const ansiResetFg = "\x1b[39m"

// colorizeLevel renders `<color><level><reset>` — format.colorize() applied
// to info.level before the printf template embeds it.
func colorizeLevel(l Level) string {
	return levelColor[l] + string(l) + ansiResetFg
}

// FormatTimestamp renders the winston timestamp() default: ISO 8601 UTC with
// millisecond precision and the Z suffix (new Date().toISOString()). It is
// also the string form the log router emits inside LoggerMessage payloads.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// formatLine renders the printf template of format/index.ts:
// `<timestamp> <colored level>: <message>[ <metadata>]`. metadata is the
// pre-rendered `key="value"` pairs joined with spaces (formatMetadata).
func formatLine(timestamp time.Time, level Level, message string, metadata string) string {
	line := FormatTimestamp(timestamp) + " " + colorizeLevel(level) + ": " + message
	if metadata != "" {
		line += " " + metadata
	}
	return line
}

// newUUID is a random RFC 4122 version 4 UUID (randomUUID of
// redis-transport.ts). The crypto/rand error path is unreachable on the
// supported platforms; a zeroed fallback id keeps the pipeline alive.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
