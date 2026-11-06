# SQLite migrations (upstream verbatim copies)

These files are **verbatim byte-for-byte copies** of the upstream Homarr SQLite
migration set. They must not be hand-edited: the sha256 of each `.sql` file is
the `hash` value that drizzle stores in the `__drizzle_migrations` bookkeeping
table, and byte fidelity is verified by `TestMigrationFilesMatchUpstreamChecksums`.

- Origin path: `packages/db/migrations/sqlite/` (including `meta/_journal.json`)
- Upstream repository: https://github.com/homarr-labs/homarr (branch `dev`)
- Upstream license: Apache-2.0 (see the repository root `LICENSE`)

The `_journal.json` is drizzle-kit's migration journal (format `version: "6"`,
`dialect: "sqlite"`). Each entry records `idx`, `version`, `when` (epoch
milliseconds), `tag` (file stem) and `breakpoints`. The Go runner in
`../migrate.go` parses this journal with `encoding/json` and replays the
entries exactly like drizzle-orm 0.45.2 does for better-sqlite3 (see
`SCHEMA.md` for the exact semantics).

Statements inside each `.sql` file are separated by the literal
`--> statement-breakpoint` marker, exactly as drizzle-kit generates them.

New upstream migrations are brought over by copying the new `NNNN_*.sql` file(s)
plus the updated `_journal.json` from `packages/db/migrations/sqlite/` — nothing
else changes. The `meta/NNNN_snapshot.json` files are only needed by drizzle-kit
and are intentionally NOT copied here (they are ~2 MB of JSON the Go binary
would otherwise embed).
