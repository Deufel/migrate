// Package migrate runs numbered SQL migrations against a SQLite database —
// the fastmigrate rules (answer.ai) with two additions: every file runs in
// its own transaction with a foreign-key check before commit, and every
// applied file's checksum is recorded so a committed file can never change.
//
// A migration is a file named NNNN-description.sql (a four-digit version,
// a kebab description). Files run in version order; each is applied at
// most once; the record lives in one table (sys_migration by default):
//
//	version · name · sha256 · applied_at
//
// Rules, in the order the runner checks them:
//
//  1. every applied version must still have its file, byte-identical
//     (a changed or missing file is an error, never silently re-run);
//  2. a file whose version is below the newest applied one but was never
//     applied is an error (history is append-only);
//  3. before the first pending file runs, an optional backup is taken with
//     VACUUM INTO (the caller names the path; it must not exist);
//  4. each file runs on one connection: PRAGMA foreign_keys=OFF, BEGIN,
//     the file, PRAGMA foreign_key_check (any row fails), the record row,
//     COMMIT, PRAGMA foreign_keys=ON — the documented SQLite rebuild
//     procedure, so a file may drop and recreate a table with data kept;
//  5. the first error stops the run; files before it stay applied, the
//     failed one is rolled back whole.
//
// A structure change is written as SQL: create the new table, INSERT …
// SELECT, drop the old, rename, recreate the indexes. The declaration
// changes and the data follows in the same file.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Step is one migration file, applied or pending.
type Step struct {
	Version   int
	Name      string // the description part of the file name
	File      string
	SHA       string // sha256 of the file's bytes
	AppliedAt string // RFC3339, "" when pending
}

// Options configure a run.
type Options struct {
	FS     fs.FS  // the directory of NNNN-name.sql files (its root)
	Table  string // the record table; "" = sys_migration
	Backup string // a path for VACUUM INTO before the first pending step; "" = none
	Log    func(format string, args ...any)
}

func (o Options) table() string {
	if o.Table == "" {
		return "sys_migration"
	}
	return o.Table
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// filePattern — NNNN-description.sql, the description kebab-case.
const filePattern = `^(\d{4})-([a-z0-9]+(?:-[a-z0-9]+)*)\.sql$`

// Load reads and orders the files. A name off the pattern, a duplicate
// version or an empty file is an error.
func Load(fsys fs.FS) ([]Step, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Step
	seen := map[int]string{}
	re := regexp.MustCompile(filePattern)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("migrate: %s is not NNNN-description.sql", e.Name())
			}
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrate: version %04d twice: %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(b)) == "" {
			return nil, fmt.Errorf("migrate: %s is empty", e.Name())
		}
		sum := sha256.Sum256(b)
		out = append(out, Step{Version: v, Name: m[2], File: e.Name(), SHA: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (o Options) ensureTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+o.table()+` (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		sha256     TEXT NOT NULL,
		applied_at TEXT NOT NULL)`)
	return err
}

func (o Options) applied(ctx context.Context, db *sql.DB) (map[int]Step, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, name, sha256, applied_at FROM `+o.table()+` ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]Step{}
	for rows.Next() {
		var s Step
		if err := rows.Scan(&s.Version, &s.Name, &s.SHA, &s.AppliedAt); err != nil {
			return nil, err
		}
		out[s.Version] = s
	}
	return out, rows.Err()
}

// Status answers what is applied and what is pending, after verifying
// rules 1 and 2. The applied steps carry the file's name and hash from
// the record; the pending ones from the file.
func Status(ctx context.Context, db *sql.DB, o Options) (applied, pending []Step, err error) {
	if err := o.ensureTable(ctx, db); err != nil {
		return nil, nil, err
	}
	files, err := Load(o.FS)
	if err != nil {
		return nil, nil, err
	}
	have, err := o.applied(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	byVersion := map[int]Step{}
	for _, f := range files {
		byVersion[f.Version] = f
	}
	newest := 0
	for v, a := range have {
		f, ok := byVersion[v]
		if !ok {
			return nil, nil, fmt.Errorf("migrate: version %04d (%s) is applied but its file is gone", v, a.Name)
		}
		if f.SHA != a.SHA {
			return nil, nil, fmt.Errorf("migrate: %s changed after it was applied (recorded %s, file %s)", f.File, a.SHA[:12], f.SHA[:12])
		}
		if v > newest {
			newest = v
		}
	}
	for _, f := range files {
		if a, ok := have[f.Version]; ok {
			a.File = f.File
			applied = append(applied, a)
			continue
		}
		if f.Version < newest {
			return nil, nil, fmt.Errorf("migrate: %s was never applied but %04d already is — history is append-only", f.File, newest)
		}
		pending = append(pending, f)
	}
	return applied, pending, nil
}

// Run applies every pending file in order and answers the ones applied.
// The first error stops the run with the failed file rolled back.
func Run(ctx context.Context, db *sql.DB, o Options) ([]Step, error) {
	_, pending, err := Status(ctx, db, o)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if o.Backup != "" {
		o.log("migrate: backup to %s", o.Backup)
		if _, err := conn.ExecContext(ctx, `VACUUM INTO `+quote(o.Backup)); err != nil {
			return nil, fmt.Errorf("migrate: backup: %w", err)
		}
	}
	var done []Step
	for _, s := range pending {
		body, err := fs.ReadFile(o.FS, s.File)
		if err != nil {
			return done, err
		}
		o.log("migrate: apply %s", s.File)
		if err := o.applyOne(ctx, conn, s, string(body)); err != nil {
			return done, fmt.Errorf("migrate: %s: %w", s.File, err)
		}
		s.AppliedAt = now()
		done = append(done, s)
	}
	return done, nil
}

func (o Options) applyOne(ctx context.Context, conn *sql.Conn, s Step, body string) (err error) {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		if _, e := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); e != nil && err == nil {
			err = e
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	rollback := func(cause error) error {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		return cause
	}
	if _, err := conn.ExecContext(ctx, body); err != nil {
		return rollback(err)
	}
	var bad int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&bad); err != nil {
		return rollback(err)
	}
	if bad > 0 {
		return rollback(fmt.Errorf("%d foreign-key violations after the file ran", bad))
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO `+o.table()+` (version, name, sha256, applied_at) VALUES (?,?,?,?)`,
		s.Version, s.Name, s.SHA, now()); err != nil {
		return rollback(err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return rollback(err)
	}
	return nil
}

// Baseline stamps every file up to and including version as applied
// WITHOUT running it — for a database that already carries the schema
// the baseline file describes. Refused when anything is recorded already.
func Baseline(ctx context.Context, db *sql.DB, o Options, version int) error {
	if err := o.ensureTable(ctx, db); err != nil {
		return err
	}
	have, err := o.applied(ctx, db)
	if err != nil {
		return err
	}
	if len(have) > 0 {
		return errors.New("migrate: baseline refused — the record is not empty")
	}
	files, err := Load(o.FS)
	if err != nil {
		return err
	}
	stamped := 0
	for _, f := range files {
		if f.Version > version {
			break
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO `+o.table()+` (version, name, sha256, applied_at) VALUES (?,?,?,?)`,
			f.Version, f.Name, f.SHA, now()); err != nil {
			return err
		}
		stamped++
	}
	if stamped == 0 {
		return fmt.Errorf("migrate: no file at or below version %04d", version)
	}
	o.log("migrate: baseline stamped through %04d (%d files)", version, stamped)
	return nil
}

// Next answers the file name a new migration should take.
func Next(fsys fs.FS, description string) (string, error) {
	files, err := Load(fsys)
	if err != nil {
		return "", err
	}
	v := 1
	if n := len(files); n > 0 {
		v = files[n-1].Version + 1
	}
	name := fmt.Sprintf("%04d-%s.sql", v, description)
	if ok, _ := regexp.MatchString(filePattern, name); !ok {
		return "", fmt.Errorf("migrate: %q is not a kebab-case description", description)
	}
	return name, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
