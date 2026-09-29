# migrate

Numbered SQL migrations for SQLite — the fastmigrate rules with two
additions: every file runs in its own transaction with a foreign-key
check before commit, and every applied file's checksum is recorded so a
committed file can never change. A standalone module; the doc comment in
`migrate.go` is the specification.

```
go test ./...                      # the module on its own
go run ./cmd/migrate -db x.db -dir migrations status
go run ./cmd/migrate -db x.db -dir migrations up -backup backups/before-0004.db
go run ./cmd/migrate -db x.db -dir migrations baseline 1
go run ./cmd/migrate -dir migrations new add-visit-table
```

A structure change is a rebuild written as SQL in the file:

```sql
CREATE TABLE thing_new (…the new declaration…);
INSERT INTO thing_new (…) SELECT … FROM thing;
DROP TABLE thing;
ALTER TABLE thing_new RENAME TO thing;
CREATE INDEX … ;
```

Foreign keys are off while the file runs and checked before commit, so a
rebuild that strands a child row is refused whole.
