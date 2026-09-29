// Command migrate — the runner at the shell:
//
//	migrate -db app.db -dir migrations status
//	migrate -db app.db -dir migrations up [-backup path]
//	migrate -db app.db -dir migrations baseline 1
//	migrate -dir migrations new add-visit-table
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"

	"github.com/Deufel/migrate"
)

func main() {
	dbPath := flag.String("db", "", "the SQLite file")
	dir := flag.String("dir", "migrations", "the directory of NNNN-name.sql files")
	backup := flag.String("backup", "", "VACUUM INTO this path before the first pending step (up only)")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fail("usage: migrate -db FILE -dir DIR status|up|baseline N|new NAME")
	}
	o := migrate.Options{FS: os.DirFS(*dir), Backup: *backup, Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	if args[0] == "new" {
		if len(args) != 2 {
			fail("new NAME")
		}
		name, err := migrate.Next(o.FS, args[1])
		if err != nil {
			fail(err.Error())
		}
		p := filepath.Join(*dir, name)
		if err := os.WriteFile(p, []byte("-- "+args[1]+"\n"), 0o644); err != nil {
			fail(err.Error())
		}
		fmt.Println(p)
		return
	}
	if *dbPath == "" {
		fail("-db is required")
	}
	db, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		fail(err.Error())
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "status":
		applied, pending, err := migrate.Status(ctx, db, o)
		if err != nil {
			fail(err.Error())
		}
		for _, s := range applied {
			fmt.Printf("applied  %s  %s\n", s.File, s.AppliedAt)
		}
		for _, s := range pending {
			fmt.Printf("pending  %s\n", s.File)
		}
	case "up":
		done, err := migrate.Run(ctx, db, o)
		for _, s := range done {
			fmt.Printf("applied  %s\n", s.File)
		}
		if err != nil {
			fail(err.Error())
		}
	case "baseline":
		if len(args) != 2 {
			fail("baseline N")
		}
		v, err := strconv.Atoi(args[1])
		if err != nil {
			fail("baseline N: a version number")
		}
		if err := migrate.Baseline(ctx, db, o, v); err != nil {
			fail(err.Error())
		}
	default:
		fail("unknown command " + args[0])
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
