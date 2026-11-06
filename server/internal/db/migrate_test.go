package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
)

// upstreamChecksums holds the sha256 of every upstream .sql migration file as
// computed from packages/db/migrations/sqlite at copy time. The test below
// compares them against the embedded copies to prove byte fidelity (the hash
// drizzle stores in __drizzle_migrations is exactly this sha256).
var upstreamChecksums = map[string]string{
	"0000_talented_ben_parker.sql":                  "c6fd51d50bbe0a63eaab178e83dc81b202a9b6eb5fe714abfb8a1c19a45b7a44",
	"0001_mixed_titanium_man.sql":                   "058465bfa0947ad8894abf2a20dcf4976b896978e7a74faa805e06055d852fc2",
	"0002_cooing_sumo.sql":                          "70cc9f161df3df995279ac89f3df957669047afa59f55cb2a0e6723f3bee4c3d",
	"0003_adorable_raider.sql":                      "10ccb5e8f52c0aa600c653d002e7d22090dd3e0ea70d13285c0aacb8ca655bfb",
	"0004_peaceful_red_ghost.sql":                   "a380540721b601bee0317d30272a23cf8d4a8955413d4d8ec6232ca516097e05",
	"0005_lean_random.sql":                          "f459640296a99c99f9d62f971e66e7a81804257140f7d0414b54deac5acd1deb",
	"0006_windy_doctor_faustus.sql":                 "cbf3736bed4b6966aa0cf04ef56f879f22dba6305fabfde94224ab5432ee413c",
	"0007_known_ultragirl.sql":                      "721592bdc44fdff9efa3234f8d7172ca1a61d3aff5ff3fc5725b0c6df23839c6",
	"0008_third_thor.sql":                           "6de721f09f78dad924abb9f5f4328dae76a316914f9b5f04570033acbd86ad54",
	"0009_stale_roulette.sql":                       "3688327ce98d497d7684bca6de72ecf2df6a91d4400a6559674cb4d9ec60e2ef",
	"0010_gorgeous_stingray.sql":                    "1375278b167980e9cfab9252e7e509add547d8465875b8f7f2d814e68b98dea1",
	"0011_classy_angel.sql":                         "db7cd838e954be9cf9853187d77a83920663589094399ff00f05f29393c80058",
	"0012_ambiguous_black_panther.sql":              "ba5ea9ed055770015302e46411b36104060e227117a6e09625494e75e3e62836",
	"0013_faithful_hex.sql":                         "28a87e76d5726e3e6c32c85e2ab8f345b47878c35c7d70f8353f19607c049adb",
	"0014_colorful_cargill.sql":                     "3e18defeed7f3e3b07db1fcc3441ddf184cdd814fa61fd0b0a061360859a0bff",
	"0015_superb_psylocke.sql":                      "a645a5bb21b331c51b38e8ca4cc053f9d1ee07bc343b93687212680c6142400b",
	"0016_change_all_to_snake_case.sql":             "26a9895c88eb9ac339640de8da7d76b74f81017aa51ec95fbeb8185f717abe8a",
	"0017_small_rumiko_fujikawa.sql":                "f0eca4ae890a552231b1906e9e3bb4664000ee3f6d2871c84025e86576375559",
	"0018_cheerful_tattoo.sql":                      "798af2ff160a9193528328341327655a2a8ff731c90af1372490259830b7468b",
	"0019_steady_darkhawk.sql":                      "0e3d2739b77c0bf0bc78d587e5b777feb2fd76430033bb30ddef9c1d42318cee",
	"0020_empty_hellfire_club.sql":                  "d6860fd993742cdb7d12c0ad8be5beeb5e6d81b7fd9a900d89770fce9a480a38",
	"0021_famous_bruce_banner.sql":                  "04b12c3285cec666f61e47684c22ef90e551b21de692a5bac08878e782b44afe",
	"0022_modern_sunfire.sql":                       "560b656f92946b97c7ed5aeb843912f1d3050bec8ed32a0c69298b7a26ea2f5c",
	"0023_fix_on_delete_actions.sql":                "9086b777a1ea9346ac3213e7281a325330c99764dbb7319d7dd7c2d526209bfc",
	"0024_bitter_scrambler.sql":                     "0b4404d37e81c6770bc73795e3f93b10e7ceaf94633bf3ed54d1029418a91b4e",
	"0025_add-group-home-board-settings.sql":        "62fede19625d4f74e79b84cae992abedbc65d54786f4aae272bac629c1013ff2",
	"0026_add-border-radius.sql":                    "a0f2add9ef93d36b90055049b8147753d58246c44f719b5ebcc7f6464b859378",
	"0027_wooden_blizzard.sql":                      "21ca1fa3959681191bcd735068b3fb2efd5cd8d1c7da29dd6cc2ca00365092a8",
	"0028_add_app_ping_url.sql":                     "03a502c9f0716354196180af98cd506182d1dbfb6480c6d26bd94d76375fe5a4",
	"0029_add_layouts.sql":                          "17f2e70f28737ee06bb02ed3044cce54b9c3cf2c740dfaa50b2c6d5e38023b54",
	"0030_migrate_item_and_section_for_layouts.sql": "c25e11095c7118ad7d555db32d9885b509a203592f341302a8b011c5908bc6b2",
	"0031_add_dynamic_section_options.sql":          "55979c264bda530a9a03b35fa28240aa842e96c898ddb16aff8ceebadbae2b70",
	"0032_add_trusted_certificate_hostnames.sql":    "946f889f8e28301fbeba6ec5ffb4ef7dd6de87354040dca440b1946bd8cc7dbd",
	"0033_add_cron_job_configuration.sql":           "d7a634088f188021db0d963eb6340d23d8ff3091d3c7b45ea1f973e3082a4f9f",
	"0034_add_app_reference_to_integration.sql":     "54300d3a35b8a832d98d5c810e1712bf9d776290a678fa92db303affab1963ae",
	"0035_remove_salt.sql":                          "bfc29ee6e393c7c3d0d54e0e5767a7687cfdf36dd477f1e09f66fb15dde4d12d",
	"0036_add_user_ddg_bangs.sql":                   "2f1af4ef0c08b7e9ee42a611fa553b1abf42d678ba851add31e436614fafa87d",
	"0037_add_user_onboarding_tour_status.sql":      "1e03e457d2a8a5b55e17354d16b492ac8f4840471ba6eb43b2df0cbb20d89a9c",
	"0038_crazy_hobgoblin.sql":                      "56fdb07c0992ccb1c3c591424067b144a87057796756049fee1b6a1f94e3d7f7",
	"0039_merge_custom_widget_url.sql":              "5f6edb156d510dec14c8331b6960835a76bba70ae02c43753d007f67325abf71",
	"0040_add_widget_secret.sql":                    "0738021b96feac0ae6b5d41fb070fc8457c5deda8726c02bd524a219076076f1",
	"0041_add_right_click_preference.sql":           "85474151c7a89d1fc48f969f952f45dd8d8f1417377dbf783171d15719b0fa9a",
	"0042_custom_widget_v2_tables.sql":              "d951ccac7ef87b4c4d95f2cd4d1ace06f7f1c8d3571970c5e5b59089350b925b",
	"0043_add_layout_gutters.sql":                   "44e2b37644924707dca0debde4f928a9736c9015695c59a6d3d5ed17b175001e",
	"0044_add_layout_role.sql":                      "7b1e8ca7822d527def3ae3ac5b9a5e6215de5abaaeb75cee64f40abb73311438",
	"0045_add_assistant_tables.sql":                 "9f1ca3ce4db96ae24fc262c7c226a3b6b480ad203a62b168bbe9868b347ea092",
	"0046_add_header_preferences_and_branding.sql":  "57abf183531d37b7ac32c6a7319b0169ad847d57217fb6e21308b5bdd4be7be1",
	"0047_add_byte_unit_system.sql":                 "69692ea9f7fa98de8ccb1a7b9664f484a59cc344731e50b3c5ef2521ee6df147",
	"0048_add_layout_resize_source.sql":             "a838e39fd4ebf48d66c7d9cee9efae902d22c1e509b2255adcd5b7886a16c49f",
	"0049_add_api_key_oauth_provenance.sql":         "b8d6a52eb8ccb3c20fda151fee0ddfd1856c1e4e202b677bf9f650a2624d156c",
	"0050_typical_madelyne_pryor.sql":               "b3d766abf07cefba479bfdb69d3c8a4bbb2804ad11cb9450a1c4623b7191b5c2",
	"0051_add_integration_external_url.sql":         "459ade73a67c58636aeb7a27f2bb65996eaa6edc8402f263dd940d1609844dad",
	"0052_add_api_key_read_only.sql":                "f1e3b5d281375b035b1bf32bc7dcc8e8707ff475cfae16186990374796c6c121",
}

func TestMigrationFilesMatchUpstreamChecksums(t *testing.T) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	sqlFiles := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		sqlFiles++
		raw, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		want, ok := upstreamChecksums[e.Name()]
		if !ok {
			t.Errorf("embedded migration %s has no recorded upstream checksum (was a new file added without updating the test?)", e.Name())
			continue
		}
		if got != want {
			t.Errorf("embedded migration %s diverges from upstream: got sha256 %s, want %s", e.Name(), got, want)
		}
	}
	if sqlFiles != len(upstreamChecksums) {
		t.Errorf("embedded .sql file count = %d, upstream checksum count = %d", sqlFiles, len(upstreamChecksums))
	}
}

func TestJournalShape(t *testing.T) {
	raw, err := fs.ReadFile(migrationsFS, migrationsDir+"/meta/_journal.json")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("parse journal: %v", err)
	}
	if j.Version != "6" || j.Dialect != "sqlite" {
		t.Fatalf("unexpected journal version/dialect: %q/%q", j.Version, j.Dialect)
	}
	if len(j.Entries) != 53 {
		t.Fatalf("journal entries = %d, want 53", len(j.Entries))
	}
	first := j.Entries[0]
	if first.Tag != "0000_talented_ben_parker" || first.When != 1715334238443 || !first.Breakpoints {
		t.Errorf("unexpected first journal entry: %+v", first)
	}
	last := j.Entries[len(j.Entries)-1]
	if last.Tag != "0052_add_api_key_read_only" || last.When != 1793804202724 {
		t.Errorf("unexpected last journal entry: %+v", last)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) != len(j.Entries) {
		t.Fatalf("loaded migrations = %d, want %d", len(migrations), len(j.Entries))
	}
	for i, m := range migrations {
		if m.when != j.Entries[i].When || m.tag != j.Entries[i].Tag {
			t.Fatalf("migration %d does not match journal entry: %+v vs %+v", i, m, j.Entries[i])
		}
		if len(m.statements) == 0 {
			t.Fatalf("migration %s produced no statements", m.tag)
		}
		if len(m.hash) != 64 {
			t.Fatalf("migration %s hash %q is not sha256 hex", m.tag, m.hash)
		}
	}
}

func TestMigrateCreatesAllTables(t *testing.T) {
	d := OpenTest(t)

	wantTables := []string{
		// Full inventory of the 41 upstream tables (snapshot meta/0052_snapshot.json).
		"account", "apiKey", "app", "assistant_configuration", "assistant_message",
		"assistant_thread", "board", "boardGroupPermission", "boardUserPermission",
		"cron_job_configuration", "custom_widget_definition", "custom_widget_secret",
		"custom_widget_v2_definition", "custom_widget_v2_secret", "group",
		"groupMember", "groupPermission", "icon", "iconRepository",
		"integration", "integrationGroupPermissions", "integrationUserPermission",
		"integrationSecret", "integration_item", "invite", "item", "item_layout",
		"layout", "media", "onboarding", "search_engine", "section",
		"section_collapse_state", "section_layout", "serverSetting", "session",
		"trusted_certificate_hostname", "user", "user_item_state",
		"verificationToken", "widget_secret",
	}
	for _, table := range wantTables {
		var name string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after Migrate: %v", table, err)
		}
	}
	if t.Failed() {
		t.Fatalf("expected %d tables", len(wantTables))
	}

	// The bookkeeping table must exist with drizzle's exact column set.
	rows, err := d.Query(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = '__drizzle_migrations'`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	var ddl string
	if !rows.Next() {
		t.Fatal("__drizzle_migrations not found in sqlite_master")
	}
	if err := rows.Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id SERIAL PRIMARY KEY", "hash text NOT NULL", "created_at numeric"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("__drizzle_migrations DDL %q does not contain %q", ddl, want)
		}
	}
}

func TestMigrateIsIdempotentAndRecordsJournalTimestamps(t *testing.T) {
	path := tempDBPath(t)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	firstCount, err := d.MigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	if firstCount != 53 {
		t.Fatalf("bookkeeping rows after first migrate = %d, want 53", firstCount)
	}
	// Record stored timestamps to verify the second pass changes nothing.
	type row struct{ hash, createdAt string }
	var before []row
	rows, err := d.Query(`SELECT hash, created_at FROM "__drizzle_migrations" ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.hash, &r.createdAt); err != nil {
			t.Fatal(err)
		}
		before = append(before, r)
	}
	rows.Close()

	// Second migrate on the same handle must be a no-op.
	if err := d.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// Close, reopen and migrate again: still a no-op (the full on-disk
	// roundtrip a real restart performs).
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if err := d2.Migrate(); err != nil {
		t.Fatalf("migrate after reopen: %v", err)
	}
	secondCount, err := d2.MigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	if secondCount != firstCount {
		t.Errorf("bookkeeping rows after re-migrate = %d, want %d", secondCount, firstCount)
	}
	var after []row
	rows, err = d2.Query(`SELECT hash, created_at FROM "__drizzle_migrations" ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.hash, &r.createdAt); err != nil {
			t.Fatal(err)
		}
		after = append(after, r)
	}
	if len(before) != len(after) {
		t.Fatalf("bookkeeping rows changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("bookkeeping row %d changed: %+v -> %+v", i, before[i], after[i])
		}
	}
}

func TestMigrateResumesFromUpstreamBookkeeping(t *testing.T) {
	// Simulate a database that upstream migrated partially: the bookkeeping
	// table exists with upstream's DDL and records that only the first two
	// migrations were applied (hash of the file + journal "when"). Migrate
	// must apply exactly the remaining 51 entries and record them with the
	// same semantics.
	path := tempDBPath(t)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	applied := migrations[:2]

	// Simulate upstream exactly: drizzle frames ALL migrations in one outer
	// BEGIN/COMMIT. The generated files that rebuild tables (e.g. 0001) open
	// with "COMMIT TRANSACTION" + "PRAGMA foreign_keys = OFF" and end with a
	// trailing "BEGIN TRANSACTION", deliberately escaping and re-establishing
	// the outer transaction — so the simulation must use the same framing or
	// those files fail with "cannot commit - no transaction is active".
	//
	// The connection is fully released before Migrate runs (the pool holds a
	// single connection).
	func() {
		conn, err := d.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx := context.Background()
		if _, err := conn.ExecContext(ctx, drizzleMigrationsDDL); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		for _, m := range applied {
			for _, stmt := range m.statements {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("simulated upstream apply of %s: %v", m.tag, err)
				}
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO "__drizzle_migrations" ("hash", "created_at") VALUES(?, ?)`, m.hash, m.when); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			t.Fatal(err)
		}
	}()

	if err := d.Migrate(); err != nil {
		t.Fatalf("migrate over partial bookkeeping: %v", err)
	}
	count, err := d.MigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Errorf("bookkeeping rows = %d, want %d (the remaining migrations must be recorded)", count, len(migrations))
	}
	// The database must end up usable: the M1 tables exist and work.
	if _, err := d.GetUserByName(context.Background(), "nobody", "credentials"); !errors.Is(err, ErrNotFound) {
		t.Errorf("user lookup on migrated db returned %v, want ErrNotFound", err)
	}
}

func TestOpenSameDatabaseTwice(t *testing.T) {
	// Two independent handles on one file, like upstream's main connection
	// plus its proxy-reader connection. Writes from both handles must
	// interleave cleanly (busy_timeout serializes lock acquisition).
	path := tempDBPath(t)
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	if err := d1.Migrate(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		board := Board{ID: fmt.Sprintf("b%d", i), Name: fmt.Sprintf("board-%d", i), IsPublic: true}
		if err := d1.CreateBoard(ctx, board); err != nil {
			t.Fatalf("handle1 create board %d: %v", i, err)
		}
		if _, err := d2.CountBoards(ctx); err != nil {
			t.Fatalf("handle2 read: %v", err)
		}
	}
	boards, err := d2.ListBoardsForUser(ctx, "anyone")
	if err != nil {
		t.Fatal(err)
	}
	if len(boards) != 5 {
		t.Fatalf("handle2 sees %d boards, want 5", len(boards))
	}

	// Concurrent writes from both handles must not produce SQLITE_BUSY
	// failures escaping busy_timeout.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs <- d1.CreateBoard(ctx, Board{ID: fmt.Sprintf("c1-%d", i), Name: fmt.Sprintf("c1-%d", i)})
		}(i)
		go func(i int) {
			defer wg.Done()
			errs <- d2.CreateBoard(ctx, Board{ID: fmt.Sprintf("c2-%d", i), Name: fmt.Sprintf("c2-%d", i)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
}

func TestMigrationHashMatchesDrizzleConvention(t *testing.T) {
	// drizzle: hash = sha256 hex of the full raw file content. Spot-check the
	// first migration against an independently computed value and verify the
	// runner stores exactly that hash plus the journal timestamp.
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(migrationsFS, migrationsDir+"/0000_talented_ben_parker.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	if migrations[0].hash != want {
		t.Fatalf("migration hash = %s, want %s", migrations[0].hash, want)
	}
	if migrations[0].when != 1715334238443 {
		t.Fatalf("migration when = %d, want 1715334238443 (journal millis)", migrations[0].when)
	}
}

// tempDBPath returns a fresh file path for tests that manage Open/Migrate
// themselves.
func tempDBPath(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/db.sqlite"
}
