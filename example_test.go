package migrate_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deufel/migrate"
	_ "modernc.org/sqlite"
)

// dir writes a migrations directory for an example and answers it.
func dir(files map[string]string) string {
	d, _ := os.MkdirTemp("", "migrate-example")
	for name, body := range files {
		_ = os.WriteFile(filepath.Join(d, name), []byte(body), 0o644)
	}
	return d
}

func open() *sql.DB {
	d, _ := os.MkdirTemp("", "migrate-example")
	db, _ := sql.Open("sqlite", filepath.Join(d, "app.db"))
	return db
}

func ExampleLoad() {
	steps, _ := migrate.Load(os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
		"0002-add-name.sql": "ALTER TABLE thing ADD COLUMN name TEXT NOT NULL DEFAULT '';",
	})))
	for _, s := range steps {
		fmt.Println(s.Version, s.Name)
	}
	// Output:
	// 1 baseline
	// 2 add-name
}

func ExampleRun() {
	db := open()
	defer db.Close()
	o := migrate.Options{FS: os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
		"0002-add-name.sql": "ALTER TABLE thing ADD COLUMN name TEXT NOT NULL DEFAULT '';",
	}))}
	applied, _ := migrate.Run(context.Background(), db, o)
	fmt.Println(len(applied), "applied")
	again, _ := migrate.Run(context.Background(), db, o)
	fmt.Println(len(again), "applied the second time")
	// Output:
	// 2 applied
	// 0 applied the second time
}

func ExampleStatus() {
	db := open()
	defer db.Close()
	o := migrate.Options{FS: os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
		"0002-add-name.sql": "ALTER TABLE thing ADD COLUMN name TEXT NOT NULL DEFAULT '';",
	}))}
	_, _ = migrate.Run(context.Background(), db, migrate.Options{FS: os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
	}))})
	applied, pending, _ := migrate.Status(context.Background(), db, o)
	fmt.Println(len(applied), "applied,", len(pending), "pending:", pending[0].File)
	// Output: 1 applied, 1 pending: 0002-add-name.sql
}

func ExampleBaseline() {
	db := open()
	defer db.Close()
	// a store that already has the table: the files up to 1 are declared
	// applied without running
	_, _ = db.Exec("CREATE TABLE thing (id INTEGER PRIMARY KEY)")
	o := migrate.Options{FS: os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
		"0002-add-name.sql": "ALTER TABLE thing ADD COLUMN name TEXT NOT NULL DEFAULT '';",
	}))}
	_ = migrate.Baseline(context.Background(), db, o, 1)
	applied, _ := migrate.Run(context.Background(), db, o)
	fmt.Println(len(applied), "applied:", applied[0].File)
	// Output: 1 applied: 0002-add-name.sql
}

func ExampleNext() {
	name, _ := migrate.Next(os.DirFS(dir(map[string]string{
		"0001-baseline.sql": "CREATE TABLE thing (id INTEGER PRIMARY KEY);",
	})), "add-name")
	fmt.Println(name)
	// Output: 0002-add-name.sql
}
