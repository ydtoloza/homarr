package logging

// SQLite persistence of the log pipeline.
//
// Upstream keeps the history in a bounded Redis list (LPUSH + LTRIM to
// LOG_HISTORY_MAX_ENTRIES = 5000 entries, redis-transport.ts:59-64) and has
// NO database table (the drizzle schema of packages/db carries none). The
// homarr-go history therefore lives in an additive "log" table this package
// owns and creates itself — the drizzle migration set under internal/db must
// stay verbatim upstream, and a table the upstream schema does not know
// cannot live there. The table is invisible to a real upstream instance
// (drizzle ignores unknown tables) and survives restarts, which the Redis
// list of a stock compose deployment does not.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// HistoryMaxEntries is LOG_HISTORY_MAX_ENTRIES (constants.ts:17): the table
// retains the newest 5000 entries, like the upstream LTRIM, and the log
// router serves the same window as history. A var only so the internal
// retention test can shrink it.
var HistoryMaxEntries = 5000

// retentionInterval bounds the DELETE frequency: the trim runs once per this
// many inserts instead of on every row (the upstream LTRIM is O(1) in Redis,
// the SQLite equivalent scans; 250 keeps the table within ~5250 rows). A var
// only so the internal retention test can shrink it.
var retentionInterval = 250

// logTableDDL creates the additive log table. timestamp is the ISO 8601
// string the payload carries (lexicographic order == chronological), level is
// one of the four upstream keys, message is the fully rendered console line.
const logTableDDL = `CREATE TABLE IF NOT EXISTS "log" (
	id text NOT NULL PRIMARY KEY,
	timestamp text NOT NULL,
	message text NOT NULL,
	level text NOT NULL
);
CREATE INDEX IF NOT EXISTS "log_timestamp_idx" ON "log" ("timestamp");`

// Store persists and reads log entries. The slog pipeline depends only on
// this interface; *SQLiteStore is the production implementation and tests
// inject fakes.
type Store interface {
	// Save persists one entry (fire-and-forget from the pipeline's worker
	// goroutine — never called on the logging caller's goroutine).
	Save(ctx context.Context, e Entry) error
	// Recent returns up to limit entries of the given levels ordered OLDEST
	// FIRST (the upstream getRecentAsync: lrange newest-first, then
	// toReversed — redis/index.ts:53-59). Empty levels = no filter.
	Recent(ctx context.Context, levels []Level, limit int) ([]Entry, error)
}

// SQLiteStore is the production Store over the shared database.
type SQLiteStore struct {
	db *db.DB
	// inserts counts Save calls to schedule the retention trim.
	inserts int64
}

// NewSQLiteStore creates the log table if missing. Call once at startup,
// after db.Migrate.
func NewSQLiteStore(ctx context.Context, database *db.DB) (*SQLiteStore, error) {
	if _, err := database.ExecContext(ctx, logTableDDL); err != nil {
		return nil, fmt.Errorf("logging: create log table: %w", err)
	}
	return &SQLiteStore{db: database}, nil
}

// Save inserts one entry and trims the table to the newest
// HistoryMaxEntries rows once per retentionInterval inserts (the
// LPUSH+LTRIM pair of redis-transport.ts:61-63).
func (s *SQLiteStore) Save(ctx context.Context, e Entry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO "log" (id, timestamp, message, level) VALUES (?, ?, ?, ?)`,
		e.ID, FormatTimestamp(e.Timestamp), e.Message, string(e.Level))
	if err != nil {
		return err
	}
	s.inserts++
	if s.inserts%int64(retentionInterval) == 0 {
		// rowid order == insertion order; keep the newest N.
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM "log" WHERE rowid NOT IN (SELECT rowid FROM "log" ORDER BY rowid DESC LIMIT ?)`,
			HistoryMaxEntries); err != nil {
			return err
		}
	}
	return nil
}

// Recent reads the history oldest-first (getRecentAsync,
// redis/index.ts:50-59): the newest `limit` rows in insertion order,
// reversed. Levels filter with an IN clause (the upstream filter runs on the
// parsed list; the SQL version is equivalent for the four fixed keys).
func (s *SQLiteStore) Recent(ctx context.Context, levels []Level, limit int) ([]Entry, error) {
	query := `SELECT id, timestamp, message, level FROM "log"`
	var args []any
	if len(levels) > 0 {
		placeholders := make([]string, len(levels))
		for i, l := range levels {
			placeholders[i] = "?"
			args = append(args, string(l))
		}
		query += ` WHERE level IN (` + strings.Join(placeholders, ", ") + `)`
	}
	args = append(args, limit)
	query += ` ORDER BY rowid DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("logging: read history: %w", err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var (
			e  Entry
			ts string
		)
		if err := rows.Scan(&e.ID, &ts, &e.Message, &e.Level); err != nil {
			return nil, fmt.Errorf("logging: scan history: %w", err)
		}
		parsed, perr := parseTimestamp(ts)
		if perr != nil {
			return nil, perr
		}
		e.Timestamp = parsed
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("logging: iterate history: %w", err)
	}
	// Oldest first: upstream lrange returns newest-first and the router
	// reverses (toReversed).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// parseTimestamp reads the stored ISO 8601 string back (JS
// Date.parse semantics via Go RFC 3339; the writer always emits the
// millisecond-UTC form of formatTimestamp).
func parseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", s)
	if err != nil {
		// Tolerate other RFC 3339 variants (e.g. rows written by tests).
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2, nil
		}
		return time.Time{}, fmt.Errorf("logging: parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// marshalMessage renders the superjson envelope of one LoggerMessage — the
// exact bytes upstream PUBLISHes to the logging channel
// (stringify(message), redis-transport.ts:57): {"json": {...}, "meta":
// {"values": {"timestamp": "Date"}, "v": 1}}. The Date annotation is
// load-bearing: the SPA log terminal calls message.timestamp.getTime()
// (terminal.tsx:96), so the payload must reconstruct real Dates.
func MarshalMessage(e Entry) ([]byte, error) {
	payload, err := json.Marshal(struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Message   string `json:"message"`
		Level     Level  `json:"level"`
	}{
		ID:        e.ID,
		Timestamp: FormatTimestamp(e.Timestamp),
		Message:   e.Message,
		Level:     e.Level,
	})
	if err != nil {
		return nil, err
	}
	return []byte(`{"json":` + string(payload) + `,"meta":{"values":{"timestamp":"Date"},"v":1}}`), nil
}

// unmarshalMessage parses the superjson envelope of the bus payload (the
// inverse of marshalMessage; the logging channel upstream parses with
// superjson parse and validates the shape, redis/index.ts:20-30). Returns
// ok=false for malformed payloads, which upstream drops silently.
func UnmarshalMessage(payload []byte) (Entry, bool) {
	var envelope struct {
		JSON struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
			Message   string `json:"message"`
			Level     Level  `json:"level"`
		} `json:"json"`
		Meta struct {
			Values map[string]string `json:"values"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Entry{}, false
	}
	if envelope.JSON.ID == "" || envelope.JSON.Message == "" {
		return Entry{}, false
	}
	if _, ok := levelSeverity[envelope.JSON.Level]; !ok {
		return Entry{}, false
	}
	ts, err := parseTimestamp(envelope.JSON.Timestamp)
	if err != nil {
		return Entry{}, false
	}
	return Entry{
		ID:        envelope.JSON.ID,
		Timestamp: ts,
		Message:   envelope.JSON.Message,
		Level:     envelope.JSON.Level,
	}, true
}
