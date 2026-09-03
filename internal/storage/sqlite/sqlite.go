// Package sqlite runs gogofhir's storage on SQLite.
//
// It is the default backend: a dataset is one file a developer can copy, reset,
// or commit as a fixture, and the server needs no database to run.
// modernc.org/sqlite is a pure-Go translation of SQLite, so there is no CGo and
// nothing to install -- which is what keeps the single-static-binary promise.
//
// Everything but this file lives in internal/storage/sqlstore, shared with
// PostgreSQL. What is here is the whole of the difference.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) a database at path. The path ":memory:"
// yields a private in-memory database, which is what tests use.
func Open(path string, idx *conformance.Index) (*sqlstore.Store, error) {
	return sqlstore.Open("sqlite", dsnFor(path), idx, dialect{}, func(db *sql.DB) {
		// One connection serializes every access. SQLite allows a single writer
		// regardless, and this sidesteps SQLITE_BUSY between our own goroutines
		// without a retry loop. It is also the reason concurrency is a
		// documented limit of this backend rather than a bug.
		db.SetMaxOpenConns(1)
	})
}

func dsnFor(path string) string {
	pragmas := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		return "file::memory:?" + pragmas
	}
	return "file:" + filepath.ToSlash(path) + "?" + pragmas
}

// dialect is what SQLite does differently.
type dialect struct{}

func (dialect) Name() string { return "SQLite" }

// Rebind is a no-op: "?" is SQLite's own placeholder.
func (dialect) Rebind(query string) string { return sqlstore.RebindNone(query) }

func (dialect) Migrations() fs.FS { return migrations }

// InsertResource reads back the key SQLite assigned. An INTEGER PRIMARY KEY is
// the rowid, so last_insert_rowid answers it without a second statement.
func (dialect) InsertResource(ctx context.Context, q sqlstore.Querier, row sqlstore.ResourceRow) (int64, error) {
	res, err := q.ExecContext(ctx, `
		INSERT INTO resource (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES (?, ?, ?, ?, FALSE, ?)`,
		row.Type, row.ID, row.VersionID, row.LastUpdated, row.Content)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// WriteFullText replaces the FTS5 row. The virtual table is keyed by rowid,
// which is the resource's surrogate key, so reindexing is a delete and an
// insert by rowid rather than a scan.
func (d dialect) WriteFullText(ctx context.Context, q sqlstore.Querier, pid int64, narrative, content string) error {
	if err := d.ClearFullText(ctx, q, pid); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO idx_fulltext (rowid, narrative, content) VALUES (?, ?, ?)`,
		pid, narrative, content)
	return err
}

func (dialect) ClearFullText(ctx context.Context, q sqlstore.Querier, pid int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM idx_fulltext WHERE rowid = ?`, pid)
	return err
}

// FullTextPredicate builds an FTS5 MATCH.
//
// FTS5 has its own query language, and a user's search terms are not written in
// it: a value containing NEAR, OR, or a quote would otherwise be read as syntax
// rather than as words. Each term is quoted as a phrase and the terms combined
// with AND, which gives "all of these words appear".
func (dialect) FullTextPredicate(alias, column, terms string) (string, []any, bool) {
	fields := strings.Fields(terms)
	if len(fields) == 0 {
		return "", nil, false
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	query := column + " : (" + strings.Join(quoted, " AND ") + ")"
	// FTS5 requires the table's own name on the left of MATCH; an alias is not
	// a column and the parser rejects it.
	clause := "EXISTS (SELECT 1 FROM idx_fulltext" +
		" WHERE idx_fulltext.rowid = " + alias + ".pid AND idx_fulltext MATCH ?)"
	return clause, []any{query}, true
}
