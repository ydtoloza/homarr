package logging

// The slog.Handler of the pipeline: it replaces the default text handler in
// the composition root (slog.SetDefault(slog.New(logging.NewHandler(...))))
// and plays the role of winston's Console + Redis transports at once:
//
//   - console: the rendered upstream line goes to Out (os.Stdout);
//   - persistence: the entry is enqueued to a bounded channel drained by one
//     worker goroutine that saves to the Store and publishes to the bus
//     topic "events.log.<level>". Handle never blocks and never drops the
//     console line; queue-overflow entries are dropped silently like the
//     upstream fire-and-forget Redis transport (.catch(() => undefined),
//     redis-transport.ts:83).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtoloza/homarr/server/internal/bus"
)

// Options configures the pipeline. The zero value mirrors the upstream
// defaults (LOG_LEVEL=info, no persistence, no publish) except Out, which
// defaults to os.Stdout.
type Options struct {
	// Level is the minimum severity that reaches the pipeline (LOG_LEVEL,
	// logs/env.ts: entries below it are dropped before any transport).
	Level Level
	// Store persists entries (nil = no persistence).
	Store Store
	// Bus receives "events.log.<level>" publishes (nil = no publish).
	Bus *bus.Bus
	// Out receives the console lines (nil = os.Stdout).
	Out io.Writer
	// QueueSize bounds the async pipeline (default 4096). A full queue drops
	// entries instead of blocking callers.
	QueueSize int
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (o *Options) defaults() Options {
	d := *o
	if d.Level == "" {
		d.Level = LevelInfo
	}
	if d.Out == nil {
		d.Out = os.Stdout
	}
	if d.QueueSize <= 0 {
		d.QueueSize = 4096
	}
	return d
}

// now resolves the clock: the Options override or the real clock.
func (h *Handler) now() time.Time {
	if h.opts.Now != nil {
		return h.opts.Now()
	}
	return time.Now()
}

// pipeline is the state shared by a handler and all its WithAttrs/WithGroup
// children: one queue, one worker, one close switch.
type pipeline struct {
	queue  chan Entry
	done   chan struct{}
	closed sync.Once

	// Dropped counts entries discarded because the queue was full.
	Dropped atomic.Int64
	// PersistFailures counts Store.Save errors (surfaced to nothing else:
	// the upstream transport swallows them, redis-transport.ts:83).
	PersistFailures atomic.Int64
}

// Handler is an slog.Handler feeding the pipeline. It is safe for concurrent
// use; WithAttrs/WithGroup produce children sharing the same worker.
type Handler struct {
	opts   Options
	attrs  []slog.Attr
	groups []string
	pipe   *pipeline
}

// NewHandler builds the handler and starts the background worker. Call Close
// to drain the queue and stop it (after Close the handler still renders
// console lines but persists nothing).
func NewHandler(opts Options) *Handler {
	o := opts.defaults()
	p := &pipeline{queue: make(chan Entry, o.QueueSize), done: make(chan struct{})}
	h := &Handler{opts: o, pipe: p}
	go h.worker(p)
	return h
}

// worker drains the queue: persist (Store) and publish (bus) one entry at a
// time, in order. Store failures are swallowed like upstream.
func (h *Handler) worker(p *pipeline) {
	defer close(p.done)
	for e := range p.queue {
		if h.opts.Store != nil {
			if err := h.opts.Store.Save(context.Background(), e); err != nil {
				p.PersistFailures.Add(1)
			}
		}
		if h.opts.Bus != nil {
			if payload, err := MarshalMessage(e); err == nil {
				h.opts.Bus.Publish(e.Level.Topic(), payload)
			}
		}
	}
}

// Close drains the queue and stops the worker. When Store is set, every
// entry accepted before Close returns has been persisted and published.
func (h *Handler) Close() {
	h.pipe.closed.Do(func() {
		close(h.pipe.queue)
		<-h.pipe.done
	})
}

// Pipe exposes the shared pipeline counters (Dropped, PersistFailures) for
// diagnostics.
func (h *Handler) Pipe() *pipeline { return h.pipe }

// Enabled reports whether the level passes the LOG_LEVEL threshold.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return slogLevelToLevel(level).atLeast(h.opts.Level)
}

// Handle renders the console line and enqueues the entry. It never blocks:
// when the queue is full the entry is counted in Dropped and dropped.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	level := slogLevelToLevel(r.Level)
	// The clock override (Options.Now) is absolute — a test seam for
	// deterministic fixtures. Without it the record time set by slog at the
	// call site is the winston timestamp() equivalent.
	ts := r.Time
	if ts.IsZero() || h.opts.Now != nil {
		ts = h.now()
	}
	recordAttrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		recordAttrs = append(recordAttrs, a)
		return true
	})
	metadata := renderMetadata(h.groups, h.attrs, recordAttrs)
	line := formatLine(ts, level, r.Message, metadata)
	entry := Entry{
		ID:        newUUID(),
		Timestamp: ts,
		Message:   line,
		Level:     level,
	}
	// The console line must never be lost to queue backpressure: write it
	// synchronously first (the Console transport).
	if h.opts.Out != nil {
		_, _ = io.WriteString(h.opts.Out, line+"\n")
	}
	select {
	case h.pipe.queue <- entry:
	default:
		h.pipe.Dropped.Add(1)
	}
	return nil
}

// WithAttrs returns a child handler whose metadata carries attrs first
// (winston child loggers merge metadata the same way: createLogger({module})).
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	child := *h
	child.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &child
}

// WithGroup returns a child whose metadata keys are namespaced under name
// (slog group semantics: subsequent attrs render as "name.key", matching the
// flat `key="value"` shape of formatMetadata).
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	child := *h
	child.groups = append(append([]string(nil), h.groups...), name)
	return &child
}

// slogLevelToLevel maps slog levels to the four upstream keys: everything
// below debug is debug, everything above error is error.
func slogLevelToLevel(l slog.Level) Level {
	switch {
	case l < slog.LevelInfo:
		return LevelDebug
	case l < slog.LevelWarn:
		return LevelInfo
	case l < slog.LevelError:
		return LevelWarn
	default:
		return LevelError
	}
}

// renderMetadata renders the `key="value"` pairs of the handler attrs plus
// the record attrs (formatMetadata, format/metadata.ts:9-20: primitive values
// only, joined with spaces, values interpolated raw between quotes). Keys are
// namespaced with the handler's open groups (slog group semantics).
func renderMetadata(groups []string, handlerAttrs []slog.Attr, recordAttrs []slog.Attr) string {
	prefix := ""
	for _, g := range groups {
		prefix += g + "."
	}
	var out string
	for _, a := range handlerAttrs {
		out = appendMetadataPair(out, prefix, a)
	}
	for _, a := range recordAttrs {
		out = appendMetadataPair(out, prefix, a)
	}
	return out
}

// appendMetadataPair renders one attr (flattening group attrs recursively
// into the dotted namespace) and appends it space-separated to out.
func appendMetadataPair(out, prefix string, a slog.Attr) string {
	if a.Equal(slog.Attr{}) {
		return out // the slog convention: ignore the empty Attr
	}
	if a.Value.Kind() == slog.KindGroup {
		// Nested groups flatten into the dotted key (objects are otherwise
		// filtered by formatMetadata; flattening keeps the information
		// without breaking the flat shape).
		sub := prefix + a.Key + "."
		for _, ga := range a.Value.Group() {
			out = appendMetadataPair(out, sub, ga)
		}
		return out
	}
	value := formatAttrValue(a.Value)
	if value == "" {
		return out // objects/functions are filtered by formatMetadata
	}
	if out != "" {
		out += " "
	}
	return out + prefix + a.Key + `="` + value + `"`
}

// formatAttrValue renders one metadata value. Strings interpolate raw (the
// upstream template does `key="${value}"` with no JSON quoting); numbers
// render like JS String(); objects (groups, structs, maps, slices) are
// filtered out, with the pragmatic exceptions the Go codebase logs: error,
// time.Time and fmt.Stringer values.
func formatAttrValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return FormatTimestamp(v.Time())
	case slog.KindAny:
		switch value := v.Any().(type) {
		case error:
			return value.Error()
		case time.Time:
			return FormatTimestamp(value)
		case fmt.Stringer:
			return value.String()
		default:
			return "" // object: filtered by formatMetadata
		}
	default:
		return ""
	}
}
