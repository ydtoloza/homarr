// Package db is the Go port of the upstream Homarr database layer
// (packages/db): it opens the same db.sqlite file, applies the same drizzle
// migrations with a compatible bookkeeping format, and exposes minimal
// repositories for the M1 milestone (login + board list).
//
// Compatibility contract (see SCHEMA.md for details):
//   - A db.sqlite created by upstream Homarr (better-sqlite3 + drizzle) must be
//     recognized and upgraded by Migrate, and vice versa: a database migrated
//     by this package keeps its state in the same "__drizzle_migrations" table
//     with the same columns and semantics.
//   - Upstream sets no PRAGMA statements at runtime (verified in
//     packages/core/src/infrastructure/db/drivers/sqlite.ts and better-sqlite3
//     v12.11.1 src/objects/database.cpp): the only connection-level behavior
//     better-sqlite3 adds is a 5000 ms busy timeout. Journal mode therefore
//     stays at the file default (DELETE unless the file is already WAL) and
//     foreign key enforcement stays OFF (SQLite default), which this package
//     reproduces.
package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

// driverName is the database/sql driver registered by modernc.org/sqlite.
const driverName = "sqlite"

// defaultBusyTimeoutMs mirrors better-sqlite3's default timeout option
// (5000 ms), which is the only statement better-sqlite3 executes on open.
const defaultBusyTimeoutMs = 5000

// DB wraps a *sql.DB pool. Upstream uses a single better-sqlite3 connection;
// the pool is therefore capped to one connection by default so runtime
// behavior (serialized writes, no spurious SQLITE_BUSY between our own
// connections) matches upstream.
type DB struct {
	*sql.DB
	path string
}

// Options control how the database file is opened.
type Options struct {
	// BusyTimeoutMs is the SQLite busy_timeout in milliseconds. Defaults to
	// 5000, matching better-sqlite3's built-in default.
	BusyTimeoutMs int
	// MaxOpenConns caps the database/sql pool. Defaults to 1 (upstream runs on
	// a single better-sqlite3 connection).
	MaxOpenConns int
}

func (o *Options) defaults() {
	if o.BusyTimeoutMs <= 0 {
		o.BusyTimeoutMs = defaultBusyTimeoutMs
	}
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 1
	}
}

// dsn builds the modernc.org/sqlite DSN. Pragmas are passed as DSN
// parameters (not one-off Exec calls) so every connection taken from the pool
// gets them; busy_timeout is per-connection state in SQLite.
func dsn(path string, opts Options) string {
	// Windows paths are converted to forward slashes; SQLite accepts them and
	// they avoid URI confusion inside the driver.
	p := filepath.ToSlash(path)
	q := url.Values{}
	q.Set("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeoutMs))
	// The query is appended manually (instead of being embedded in a file: URI)
	// so the path itself never needs percent-encoding.
	return "file:" + p + "?" + q.Encode()
}

// Open opens (creating if needed) the SQLite database at path with upstream
// parity pragmas. The parent directory is created when missing, mirroring the
// upstream entrypoint (`mkdir -p /appdata/db` in scripts/run.sh). Apply
// Migrate afterwards to bring the schema up to date.
func Open(path string, opts ...Options) (*DB, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o.defaults()

	// better-sqlite3 refuses to open a database whose directory does not
	// exist; upstream pre-creates it in the entrypoint. Do the same here.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("db: create directory for %s: %w", path, err)
		}
	}

	sqlDB, err := sql.Open(driverName, dsn(path, o))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(o.MaxOpenConns)
	// One long-lived connection mirrors upstream; avoid needless reconnects.
	sqlDB.SetMaxIdleConns(o.MaxOpenConns)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	return &DB{DB: sqlDB, path: path}, nil
}

// Path returns the file path the database was opened with.
func (d *DB) Path() string { return d.path }

// OpenTest opens a migrated database in a fresh temporary directory and wires
// cleanup to t. Intended for tests of this package and of packages that build
// on it (auth, api, ...).
func OpenTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("db: OpenTest: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("db: OpenTest migrate: %v", err)
	}
	return d
}
