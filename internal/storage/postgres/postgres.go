// Package postgres runs gogofhir's storage on PostgreSQL.
//
// It is the backend for deployments that need real concurrency: SQLite allows
// one writer at a time, which is a documented limit rather than a bug, and this
// one does not.
//
// Everything but this file lives in internal/storage/sqlstore, shared with
// SQLite. What is here is the whole of the difference -- placeholder syntax,
// how an insert reports its surrogate key, full-text search, and the DDL. That
// the list is this short is the point: the schema was built portable from M2,
// and this port is a translation rather than a rewrite.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage/sqlstore"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open connects to a PostgreSQL database and prepares the schema.
//
// dsn is a libpq connection string or a postgres:// URL.
func Open(dsn string, idx *conformance.Index) (*sqlstore.Store, error) {
	return sqlstore.Open("pgx", dsn, idx, dialect{}, func(db *sql.DB) {
		// Unlike SQLite there is no single-writer limit to work around, so the
		// pool is left at a size a small deployment can sustain without
		// exhausting the server's connection slots.
		db.SetMaxOpenConns(16)
		db.SetMaxIdleConns(4)
	})
}

// Rebind renumbers "?" placeholders as "$1", "$2", and so on, which is what
// lets every query in the shared implementation be written once. Exported for
// the test that pins its treatment of quoted literals.
func Rebind(query string) string { return sqlstore.RebindDollar(query) }

// dialect is what PostgreSQL does differently.
type dialect struct{}

func (dialect) Name() string { return "PostgreSQL" }

func (dialect) Rebind(query string) string { return Rebind(query) }

func (dialect) Migrations() fs.FS { return migrations }

// InsertResource reads back the key the identity column assigned. PostgreSQL
// has no last-insert-rowid, so the statement returns it.
func (dialect) InsertResource(ctx context.Context, q sqlstore.Querier, row sqlstore.ResourceRow) (int64, error) {
	var pid int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO resource (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES ($1, $2, $3, $4, FALSE, $5)
		RETURNING pid`,
		row.Type, row.ID, row.VersionID, row.LastUpdated, row.Content).Scan(&pid)
	return pid, err
}

// WriteFullText replaces a resource's tsvectors.
func (dialect) WriteFullText(ctx context.Context, q sqlstore.Querier, pid int64, narrative, content string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO idx_fulltext (pid, narrative, content)
		VALUES ($1, to_tsvector('simple', $2), to_tsvector('simple', $3))
		ON CONFLICT (pid) DO UPDATE
		SET narrative = EXCLUDED.narrative, content = EXCLUDED.content`,
		pid, narrative, content)
	return err
}

func (dialect) ClearFullText(ctx context.Context, q sqlstore.Querier, pid int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM idx_fulltext WHERE pid = $1`, pid)
	return err
}

// FullTextPredicate builds a tsquery match.
//
// plainto_tsquery treats its input as words rather than as query syntax and
// combines them with AND, which is both the safe reading of a client's search
// terms and the same semantics FTS5 is given on the other engine.
func (dialect) FullTextPredicate(alias, column, terms string) (string, []any, bool) {
	if terms == "" {
		return "", nil, false
	}
	clause := "EXISTS (SELECT 1 FROM idx_fulltext ft WHERE ft.pid = " + alias +
		".pid AND ft." + column + " @@ plainto_tsquery('simple', ?))"
	return clause, []any{terms}, true
}
