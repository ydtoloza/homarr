package logging_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
	"github.com/ydtoloza/homarr/server/internal/db"
	"github.com/ydtoloza/homarr/server/internal/logging"
)

// fixedNow is the reference instant of the format fixtures (a UTC instant
// whose toISOString form is 2026-11-07T12:34:56.789Z).
var fixedNow = time.Date(2026, 11, 7, 12, 34, 56, 789_000_000, time.UTC)

// memStore is a Store fake recording saves.
type memStore struct {
	mu      sync.Mutex
	entries []logging.Entry
	saveErr error
	gate    chan struct{} // non-nil: Save blocks until closed
}

func (m *memStore) Save(_ context.Context, e logging.Entry) error {
	if m.gate != nil {
		<-m.gate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.entries = append(m.entries, e)
	return nil
}

func (m *memStore) Recent(_ context.Context, levels []logging.Level, limit int) ([]logging.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, errors.New("not implemented")
}

func (m *memStore) all() []logging.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]logging.Entry(nil), m.entries...)
}

// lineFormat pins the printf template of packages/core/src/infrastructure/
// logs/format/index.ts with the npm color palette.
func TestHandlerLineFormat(t *testing.T) {
	var out bytes.Buffer
	h := logging.NewHandler(logging.Options{
		Level: logging.LevelDebug,
		Out:   &out,
		Now:   func() time.Time { return fixedNow },
	})
	defer h.Close()

	logger := slog.New(h.WithAttrs([]slog.Attr{slog.String("module", "logRouter")}))
	logger.Info("A tRPC client has connected to the logging procedure")
	logger.Error("boom", "err", errors.New("disk on fire"), "count", 3)

	got := out.String()
	want := "2026-11-07T12:34:56.789Z \x1b[32minfo\x1b[39m: A tRPC client has connected to the logging procedure module=\"logRouter\"\n" +
		"2026-11-07T12:34:56.789Z \x1b[31merror\x1b[39m: boom module=\"logRouter\" err=\"disk on fire\" count=\"3\"\n"
	if got != want {
		t.Fatalf("lines =\n%q\nwant\n%q", got, want)
	}
}

// LOG_LEVEL semantics: debug entries stay below a warn threshold; the four
// level keys map onto the four colors.
func TestHandlerLevelFilterAndColors(t *testing.T) {
	var out bytes.Buffer
	h := logging.NewHandler(logging.Options{
		Level: logging.LevelWarn,
		Out:   &out,
		Now:   func() time.Time { return fixedNow },
	})
	defer h.Close()
	logger := slog.New(h)

	logger.Debug("d") // below the threshold
	logger.Info("i")  // below the threshold
	logger.Warn("w")
	logger.Error("e")

	got := out.String()
	if strings.Contains(got, "debug") || strings.Contains(got, ": d\n") {
		t.Errorf("debug leaked below the warn threshold: %q", got)
	}
	if strings.Contains(got, "info") || strings.Contains(got, ": i\n") {
		t.Errorf("info leaked below the warn threshold: %q", got)
	}
	for _, want := range []string{
		"\x1b[33mwarn\x1b[39m: w",
		"\x1b[31merror\x1b[39m: e",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q missing from %q", want, got)
		}
	}
}

// The pipeline persists and publishes asynchronously; Close drains.
func TestHandlerPipelinePersistAndPublish(t *testing.T) {
	eventBus := bus.New()
	defer eventBus.Close()
	store := &memStore{}
	h := logging.NewHandler(logging.Options{
		Level: logging.LevelDebug,
		Store: store,
		Bus:   eventBus,
		Now:   func() time.Time { return fixedNow },
	})
	defer h.Close()
	logger := slog.New(h)

	sub := eventBus.Subscribe("events.log.*")
	logger.Warn("hello pipeline", "module", "test")
	h.Close() // drain: everything accepted before Close is persisted+published

	entries := store.all()
	if len(entries) != 1 {
		t.Fatalf("store has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Level != logging.LevelWarn || e.Message == "" || e.ID == "" {
		t.Errorf("entry = %+v", e)
	}
	if want := "2026-11-07T12:34:56.789Z \x1b[33mwarn\x1b[39m: hello pipeline module=\"test\""; e.Message != want {
		t.Errorf("message = %q, want %q", e.Message, want)
	}
	if !e.Timestamp.Equal(fixedNow) {
		t.Errorf("timestamp = %v, want %v", e.Timestamp, fixedNow)
	}

	select {
	case msg := <-sub.C:
		// The payload is the superjson LoggerMessage envelope with the Date
		// annotation (stringify(message) of redis-transport.ts).
		want := `{"json":{"id":"` + e.ID + `","timestamp":"2026-11-07T12:34:56.789Z",` +
			`"message":"2026-11-07T12:34:56.789Z \u001b[33mwarn\u001b[39m: hello pipeline module=\"test\"",` +
			`"level":"warn"},"meta":{"values":{"timestamp":"Date"},"v":1}}`
		if string(msg.Payload) != want {
			t.Errorf("payload = %s\nwant      %s", msg.Payload, want)
		}
		if msg.Topic != "events.log.warn" {
			t.Errorf("topic = %q, want events.log.warn", msg.Topic)
		}
	default:
		t.Fatal("no bus message published")
	}
}

// A full queue drops entries instead of blocking the caller.
func TestHandlerQueueOverflowDrops(t *testing.T) {
	store := &memStore{gate: make(chan struct{})}
	h := logging.NewHandler(logging.Options{
		Level:     logging.LevelInfo,
		Store:     store,
		Out:       io.Discard,
		QueueSize: 2,
		Now:       func() time.Time { return fixedNow },
	})
	defer h.Close()
	logger := slog.New(h)

	start := time.Now()
	for i := 0; i < 50; i++ {
		logger.Info("flood", "i", i)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Handle blocked on the full queue (%v)", elapsed)
	}
	if dropped := h.Pipe().Dropped.Load(); dropped == 0 {
		t.Error("no drops counted for a blocked queue")
	}
	// Release the worker and make sure Close drains without hanging.
	close(store.gate)
	h.Close()
}

// Store errors are counted, not fatal (the upstream transport swallows them).
func TestHandlerPersistFailuresCounted(t *testing.T) {
	store := &memStore{saveErr: errors.New("disk on fire")}
	h := logging.NewHandler(logging.Options{
		Level: logging.LevelInfo,
		Out:   io.Discard,
		Store: store,
		Now:   func() time.Time { return fixedNow },
	})
	logger := slog.New(h)
	logger.Error("one")
	h.Close()
	if h.Pipe().PersistFailures.Load() != 1 {
		t.Errorf("PersistFailures = %d, want 1", h.Pipe().PersistFailures.Load())
	}
}

// WithGroup namespaces metadata keys (slog semantics over the flat upstream
// shape).
func TestHandlerGroups(t *testing.T) {
	var out bytes.Buffer
	h := logging.NewHandler(logging.Options{
		Level: logging.LevelInfo,
		Out:   &out,
		Now:   func() time.Time { return fixedNow },
	})
	defer h.Close()
	logger := slog.New(h.WithGroup("http"))
	logger.Info("request", "path", "/api/trpc", slog.Group("user", slog.String("id", "u1")))

	got := out.String()
	want := "2026-11-07T12:34:56.789Z \x1b[32minfo\x1b[39m: request http.path=\"/api/trpc\" http.user.id=\"u1\"\n"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// The SQLite store: create, save, filter and order like the upstream
// getRecentAsync (newest-limited, reversed to oldest-first).
func TestSQLiteStoreHistory(t *testing.T) {
	d := db.OpenTest(t)
	ctx := context.Background()
	store, err := logging.NewSQLiteStore(ctx, d)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	total := 30
	for i := 0; i < total; i++ {
		e := logging.Entry{
			ID:        fmt.Sprintf("id-%04d", i),
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Message:   fmt.Sprintf("line %d", i),
			Level:     logging.LevelInfo,
		}
		if i%10 == 0 {
			e.Level = logging.LevelError
		}
		if err := store.Save(ctx, e); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	all, err := store.Recent(ctx, nil, 1000)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(all) != total {
		t.Fatalf("row count = %d, want %d", len(all), total)
	}
	if all[0].ID != "id-0000" || all[len(all)-1].ID != fmt.Sprintf("id-%04d", total-1) {
		t.Errorf("order wrong: first=%s last=%s", all[0].ID, all[len(all)-1].ID)
	}

	// Level filter (the IN clause).
	errs, err := store.Recent(ctx, []logging.Level{logging.LevelError}, 1000)
	if err != nil {
		t.Fatalf("Recent(errs): %v", err)
	}
	if len(errs) != 3 {
		t.Errorf("error count = %d, want 3", len(errs))
	}
	for _, e := range errs {
		if e.Level != logging.LevelError {
			t.Errorf("filter leaked %s entry", e.Level)
		}
	}

	// The limit keeps the NEWEST rows (lrange 0..limit-1 semantics), oldest
	// first after the reverse.
	newest, err := store.Recent(ctx, nil, 5)
	if err != nil {
		t.Fatalf("Recent(5): %v", err)
	}
	if len(newest) != 5 || newest[0].ID != fmt.Sprintf("id-%04d", total-5) {
		t.Errorf("limit window wrong: %+v", newest)
	}

	// The timestamp round-trips through the ISO string with millisecond
	// precision.
	if got := newest[len(newest)-1].Timestamp.Format("2006-01-02T15:04:05.000Z"); got != "2026-01-01T00:00:00.029Z" {
		t.Errorf("timestamp = %s", got)
	}
}

// The bus payload round-trips through UnmarshalMessage with the Date
// annotation semantics (parseLoggerMessage of redis/index.ts drops invalid
// payloads).
func TestMessageEnvelopeRoundTrip(t *testing.T) {
	e := logging.Entry{
		ID:        "abc",
		Timestamp: fixedNow,
		Message:   "line with \"quotes\" and éè",
		Level:     logging.LevelError,
	}
	payload, err := logging.MarshalMessage(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// superjson envelope shape.
	if !bytes.Contains(payload, []byte(`,"meta":{"values":{"timestamp":"Date"},"v":1}}`)) {
		t.Fatalf("payload missing Date annotation: %s", payload)
	}
	got, ok := logging.UnmarshalMessage(payload)
	if !ok {
		t.Fatal("payload rejected")
	}
	if got != e {
		t.Errorf("round trip = %+v, want %+v", got, e)
	}
	if _, ok := logging.UnmarshalMessage([]byte(`{"json":{"id":"","timestamp":"nope","message":"m","level":"info"}}`)); ok {
		t.Error("invalid payload accepted")
	}
}
