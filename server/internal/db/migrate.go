package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// migrationsFS embeds the verbatim upstream drizzle migration set (see
// migrations/README.md for provenance and licensing).
//
//go:embed migrations/*.sql migrations/meta/_journal.json
var migrationsFS embed.FS

// migrationsDir is the embedded folder the migration files live in.
const migrationsDir = "migrations"

// statementBreakpoint is the literal marker drizzle-kit uses to separate
// statements inside a migration file. The upstream runner (drizzle-orm
// readMigrationFiles) splits the raw file content on exactly this substring,
// without any whitespace normalization, so the split here must match it.
const statementBreakpoint = "--> statement-breakpoint"

// drizzleMigrationsTable is the bookkeeping table name used by drizzle-orm's
// SQLite migrator (SQLiteSyncDialect.migrate, drizzle-orm 0.45.2).
const drizzleMigrationsTable = "__drizzle_migrations"

// drizzleMigrationsDDL is verbatim the CREATE TABLE that drizzle-orm 0.45.2
// executes for the SQLite sync dialect (better-sqlite3), including the exact
// leading whitespace of the rendered sql template. It is intentionally
// unusual — "id SERIAL" is not an INTEGER PRIMARY KEY rowid alias in SQLite,
// so drizzle's inserts leave id NULL — but it is reproduced byte-for-byte so
// databases migrated by upstream and by this package are indistinguishable
// (including the SQL stored in sqlite_master).
const drizzleMigrationsDDL = `
			CREATE TABLE IF NOT EXISTS "__drizzle_migrations" (
				id SERIAL PRIMARY KEY,
				hash text NOT NULL,
				created_at numeric
			)`

// journalEntry mirrors one entry of meta/_journal.json (drizzle-kit journal
// format version 6, dialect sqlite).
type journalEntry struct {
	Idx         int    `json:"idx"`
	Version     string `json:"version"`
	When        int64  `json:"when"`
	Tag         string `json:"tag"`
	Breakpoints bool   `json:"breakpoints"`
}

type journal struct {
	Version string         `json:"version"`
	Dialect string         `json:"dialect"`
	Entries []journalEntry `json:"entries"`
}

// migration is a journal entry resolved to its SQL statements and hash, the
// equivalent of drizzle's readMigrationFiles output.
type migration struct {
	// statements are the file content split on statementBreakpoint.
	statements []string
	// hash is the sha256 hex digest of the full raw file content, exactly as
	// drizzle computes it (crypto.createHash("sha256").update(query)).
	hash string
	// when is the journal "when" epoch milliseconds (drizzle: folderMillis).
	when int64
	tag  string
}

// loadMigrations reads the embedded journal and files. Files are hashed from
// their exact embedded bytes, which is why the copies under migrations/ must
// never be re-saved with different line endings or an added header.
func loadMigrations() ([]migration, error) {
	journalRaw, err := fs.ReadFile(migrationsFS, migrationsDir+"/meta/_journal.json")
	if err != nil {
		return nil, fmt.Errorf("db: read journal: %w", err)
	}
	var j journal
	if err := json.Unmarshal(journalRaw, &j); err != nil {
		return nil, fmt.Errorf("db: parse journal: %w", err)
	}
	if j.Dialect != "sqlite" {
		return nil, fmt.Errorf("db: unexpected journal dialect %q", j.Dialect)
	}

	migrations := make([]migration, 0, len(j.Entries))
	for _, e := range j.Entries {
		raw, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+e.Tag+".sql")
		if err != nil {
			return nil, fmt.Errorf("db: read migration %s: %w", e.Tag, err)
		}
		query := string(raw)
		// Split exactly like drizzle's readMigrationFiles; chunks are not
		// trimmed (whitespace is harmless to SQLite, but bytes are not).
		parts := strings.Split(query, statementBreakpoint)
		statements := make([]string, 0, len(parts))
		for _, p := range parts {
			// drizzle would fail on an empty statement (better-sqlite3
			// prepare rejects it); no upstream file produces one, but skip
			// defensively so a trailing breakpoint never breaks Migrate.
			if strings.TrimSpace(p) == "" {
				continue
			}
			statements = append(statements, p)
		}
		sum := sha256.Sum256(raw)
		migrations = append(migrations, migration{
			statements: statements,
			hash:       hex.EncodeToString(sum[:]),
			when:       e.When,
			tag:        e.Tag,
		})
	}
	return migrations, nil
}

// Migrate applies all pending embedded migrations. It reproduces the exact
// semantics of drizzle-orm 0.45.2 SQLiteSyncDialect.migrate (the migrator
// upstream runs at container startup via packages/db/migrations/sqlite/
// migrate.ts, bundled as migrate.cjs and invoked from scripts/run.sh):
//
//  1. CREATE TABLE IF NOT EXISTS "__drizzle_migrations" (verbatim DDL above).
//  2. Read the last applied migration: SELECT id, hash, created_at FROM
//     "__drizzle_migrations" ORDER BY created_at DESC LIMIT 1.
//  3. In one transaction, replay every journal entry whose "when" timestamp
//     is strictly greater than the stored created_at (or all entries when the
//     table is empty), running the file's statements in order and inserting
//     (hash = sha256 of the raw file, created_at = journal "when").
//
// The hash column is stored but never consulted by drizzle's decision logic —
// ordering is purely by the "when" millisecond timestamps — and that behavior
// is preserved here. This makes the bookkeeping format interoperable in both
// directions with real upstream databases.
func (d *DB) Migrate() error {
	return d.MigrateContext(context.Background())
}

// MigrateContext is the context-aware variant of Migrate.
func (d *DB) MigrateContext(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// Run the whole sequence on a single connection, like better-sqlite3 does
	// (one connection for everything). This also makes the explicit BEGIN /
	// COMMIT below reliable even when the pool would hand out several
	// connections.
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire connection for migrate: %w", err)
	}
	defer conn.Close()

	// Step 1: bookkeeping table (outside the transaction, like drizzle).
	if _, err := conn.ExecContext(ctx, drizzleMigrationsDDL); err != nil {
		return fmt.Errorf("db: create %s: %w", drizzleMigrationsTable, err)
	}

	// Step 2: last applied migration.
	lastCreatedAt, err := lastAppliedMigration(ctx, conn)
	if err != nil {
		return err
	}

	// Step 3: apply pending migrations in journal order.
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return fmt.Errorf("db: begin migration transaction: %w", err)
	}
	if err := applyMigrations(ctx, conn, migrations, lastCreatedAt); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("db: commit migrations: %w", err)
	}

	// Parity: upstream applies migrations in a separate process, so runtime
	// connections never see foreign_keys=ON. Some migration files enable it
	// explicitly and it persists per-connection; this connection comes from
	// the same pool, so restore better-sqlite3's runtime default (OFF)
	// before handing it back. Upstream therefore tolerates non-cascading
	// deletes that would otherwise fail here (see db package doc).
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("db: restore foreign_keys pragma after migrations: %w", err)
	}
	return nil
}

// lastAppliedMigration reads the newest bookkeeping row with the same query
// drizzle uses. It returns 0 when the table is empty (equivalent to drizzle's
// undefined lastDbMigration, since every journal timestamp is a positive
// epoch-millisecond value). created_at is "numeric" and stores epoch millis.
func lastAppliedMigration(ctx context.Context, conn *sql.Conn) (int64, error) {
	const query = `SELECT id, hash, created_at FROM "__drizzle_migrations" ORDER BY created_at DESC LIMIT 1`
	var (
		id        sql.NullInt64
		hash      string
		createdAt int64
	)
	err := conn.QueryRowContext(ctx, query).Scan(&id, &hash, &createdAt)
	switch {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("db: read %s: %w", drizzleMigrationsTable, err)
	default:
		// Mirror drizzle's Number(lastDbMigration[2]) coercion.
		return createdAt, nil
	}
}

func applyMigrations(ctx context.Context, conn *sql.Conn, migrations []migration, lastCreatedAt int64) error {
	for _, m := range migrations {
		// Strictly-less comparison, exactly like drizzle:
		//   if (!lastDbMigration || Number(last.created_at) < migration.folderMillis)
		if lastCreatedAt >= m.when {
			continue
		}
		for _, stmt := range m.statements {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("db: migration %s: %w", m.tag, err)
			}
		}
		// Same insert shape as drizzle: id is omitted (stays NULL, as SQLite
		// does not treat SERIAL as INTEGER PRIMARY KEY) and the columns are
		// quoted with double quotes exactly like the upstream SQL.
		const insert = `INSERT INTO "__drizzle_migrations" ("hash", "created_at") VALUES(?, ?)`
		if _, err := conn.ExecContext(ctx, insert, m.hash, m.when); err != nil {
			return fmt.Errorf("db: record migration %s: %w", m.tag, err)
		}
	}
	return nil
}

// MigrationCount returns the number of recorded migrations in the bookkeeping
// table. Mainly for tests and diagnostics.
func (d *DB) MigrationCount() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM "__drizzle_migrations"`).Scan(&n)
	return n, err
}
