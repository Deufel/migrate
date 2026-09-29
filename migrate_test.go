package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db")+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func file(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestLoadOrdersAndRefuses(t *testing.T) {
	fsys := fstest.MapFS{
		"0002-two.sql":   file("SELECT 2;"),
		"0001-one.sql":   file("SELECT 1;"),
		"README.md":      file("not a migration"),
		"0010-ten-x.sql": file("SELECT 10;"),
	}
	steps, err := Load(fsys)
	if err != nil || len(steps) != 3 || steps[0].Version != 1 || steps[2].Version != 10 || steps[2].Name != "ten-x" {
		t.Fatalf("load: %+v %v", steps, err)
	}
	for name, bad := range map[string]fstest.MapFS{
		"pattern":   {"1-one.sql": file("SELECT 1;")},
		"duplicate": {"0001-a.sql": file("SELECT 1;"), "0001-b.sql": file("SELECT 1;")},
		"empty":     {"0001-a.sql": file("  \n")},
		"case":      {"0001-Bad.sql": file("SELECT 1;")},
	} {
		if _, err := Load(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunAppliesOnceInOrder(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	fsys := fstest.MapFS{
		"0001-base.sql": file(`CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
			INSERT INTO a (name) VALUES ('x'), ('y');`),
		"0002-b.sql": file(`CREATE TABLE b (id INTEGER PRIMARY KEY, a INTEGER NOT NULL REFERENCES a(id));
			INSERT INTO b (a) SELECT id FROM a;`),
	}
	o := Options{FS: fsys}
	done, err := Run(ctx, db, o)
	if err != nil || len(done) != 2 || done[0].Version != 1 || done[1].Version != 2 {
		t.Fatalf("run: %+v %v", done, err)
	}
	if count(t, db, `SELECT COUNT(*) FROM b`) != 2 {
		t.Fatal("the files ran")
	}
	if count(t, db, `SELECT COUNT(*) FROM sys_migration`) != 2 {
		t.Fatal("the record")
	}
	again, err := Run(ctx, db, o)
	if err != nil || len(again) != 0 {
		t.Fatalf("a second run is a no-op: %+v %v", again, err)
	}
	applied, pending, err := Status(ctx, db, o)
	if err != nil || len(applied) != 2 || len(pending) != 0 || applied[1].File != "0002-b.sql" || applied[1].AppliedAt == "" {
		t.Fatalf("status: %+v %+v %v", applied, pending, err)
	}
	// a third file lands later — only it is pending
	fsys["0003-c.sql"] = file(`ALTER TABLE a ADD COLUMN note TEXT NOT NULL DEFAULT '';`)
	_, pending, _ = Status(ctx, db, o)
	if len(pending) != 1 || pending[0].Version != 3 {
		t.Fatalf("pending: %+v", pending)
	}
}

func TestChangedOrMissingFileFailsLoudly(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	fsys := fstest.MapFS{"0001-base.sql": file(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)}
	o := Options{FS: fsys}
	if _, err := Run(ctx, db, o); err != nil {
		t.Fatal(err)
	}
	fsys["0001-base.sql"] = file(`CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);`)
	if _, _, err := Status(ctx, db, o); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("a changed file: %v", err)
	}
	delete(fsys, "0001-base.sql")
	if _, _, err := Status(ctx, db, o); err == nil || !strings.Contains(err.Error(), "file is gone") {
		t.Fatalf("a missing file: %v", err)
	}
	// a file slipped in below the newest applied version
	fsys["0001-base.sql"] = file(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)
	fsys["0002-late.sql"] = file(`CREATE TABLE late (id INTEGER PRIMARY KEY);`)
	if _, err := Run(ctx, db, o); err != nil {
		t.Fatal(err)
	}
	fsys["0000-slip.sql"] = file(`SELECT 1;`)
	if _, _, err := Status(ctx, db, o); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("out of order: %v", err)
	}
}

func TestFailedFileRollsBackWhole(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	fsys := fstest.MapFS{
		"0001-base.sql": file(`CREATE TABLE a (id INTEGER PRIMARY KEY);`),
		"0002-bad.sql": file(`CREATE TABLE b (id INTEGER PRIMARY KEY);
			INSERT INTO b (id) VALUES (1);
			INSERT INTO nowhere (id) VALUES (1);`),
		"0003-after.sql": file(`CREATE TABLE c (id INTEGER PRIMARY KEY);`),
	}
	done, err := Run(ctx, db, Options{FS: fsys})
	if err == nil || len(done) != 1 {
		t.Fatalf("the run stops at the bad file: %+v %v", done, err)
	}
	if count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('b','c')`) != 0 {
		t.Fatal("the failed file left a table behind, or the run went on")
	}
	if count(t, db, `SELECT COUNT(*) FROM sys_migration`) != 1 {
		t.Fatal("only the first is recorded")
	}
	// the connection is back to FK enforcement after the failure
	var fk int
	_ = db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatalf("foreign keys left off: %d", fk)
	}
}

func TestRebuildKeepsDataAndChecksKeys(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	fsys := fstest.MapFS{
		"0001-base.sql": file(`
			CREATE TABLE parent (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('a','b')));
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent INTEGER NOT NULL REFERENCES parent(id));
			INSERT INTO parent (id, kind) VALUES (1,'a'), (2,'b');
			INSERT INTO child (parent) VALUES (1), (2);`),
		// THE REBUILD: the CHECK grows and a column goes, data kept, children intact
		"0002-rebuild.sql": file(`
			CREATE TABLE parent_new (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('a','b','c')), note TEXT NOT NULL DEFAULT '');
			INSERT INTO parent_new (id, kind) SELECT id, kind FROM parent;
			DROP TABLE parent;
			ALTER TABLE parent_new RENAME TO parent;`),
	}
	if _, err := Run(ctx, db, Options{FS: fsys}); err != nil {
		t.Fatal(err)
	}
	if count(t, db, `SELECT COUNT(*) FROM child c JOIN parent p ON p.id = c.parent`) != 2 {
		t.Fatal("the rebuild kept the rows and the children still point at them")
	}
	if _, err := db.Exec(`INSERT INTO parent (kind) VALUES ('c')`); err != nil {
		t.Fatal("the grown CHECK admits the new word:", err)
	}
	// a rebuild that strands a child is refused whole
	fsys["0003-strand.sql"] = file(`
		CREATE TABLE parent_new (id INTEGER PRIMARY KEY, kind TEXT NOT NULL);
		INSERT INTO parent_new (id, kind) SELECT id, kind FROM parent WHERE id = 1;
		DROP TABLE parent;
		ALTER TABLE parent_new RENAME TO parent;`)
	if _, err := Run(ctx, db, Options{FS: fsys}); err == nil || !strings.Contains(err.Error(), "foreign-key violations") {
		t.Fatalf("a stranding rebuild: %v", err)
	}
	if count(t, db, `SELECT COUNT(*) FROM parent`) != 3 {
		t.Fatal("the refused rebuild rolled back")
	}
}

func TestBaselineAndBackup(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE a (id INTEGER PRIMARY KEY); INSERT INTO a VALUES (7);`); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"0001-base.sql": file(`CREATE TABLE a (id INTEGER PRIMARY KEY);`), // already there
		"0002-more.sql": file(`CREATE TABLE b (id INTEGER PRIMARY KEY);`),
	}
	o := Options{FS: fsys}
	if err := Baseline(ctx, db, o, 1); err != nil {
		t.Fatal(err)
	}
	if err := Baseline(ctx, db, o, 1); err == nil {
		t.Fatal("a second baseline is refused")
	}
	backup := filepath.Join(t.TempDir(), "before-0002.db")
	o.Backup = backup
	done, err := Run(ctx, db, o)
	if err != nil || len(done) != 1 || done[0].Version != 2 {
		t.Fatalf("after the baseline only 0002 runs: %+v %v", done, err)
	}
	if count(t, db, `SELECT COUNT(*) FROM a`) != 1 {
		t.Fatal("the baseline never ran the file — the row survived")
	}
	if st, err := os.Stat(backup); err != nil || st.Size() == 0 {
		t.Fatalf("the backup file: %v", err)
	}
	bk, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer bk.Close()
	if count(t, bk, `SELECT COUNT(*) FROM sqlite_master WHERE name='b'`) != 0 {
		t.Fatal("the backup predates the step")
	}
	// no pending steps → no backup attempted (the path may exist)
	o.Backup = backup
	if _, err := Run(ctx, db, o); err != nil {
		t.Fatal("a no-op run must not try the backup:", err)
	}
	if name, err := Next(fsys, "third-thing"); err != nil || name != "0003-third-thing.sql" {
		t.Fatalf("next: %s %v", name, err)
	}
	if _, err := Next(fsys, "Bad Name"); err == nil {
		t.Fatal("a bad description is refused")
	}
}
