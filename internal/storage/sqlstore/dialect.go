package sqlstore

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
)

// The engine seam.
//
// gogofhir has one SQL implementation, not two. Everything below this file --
// versioning, history, index extraction, the whole query renderer -- is shared,
// and a Dialect supplies only what the engines genuinely do differently. That
// is the point: a second backend written alongside the first is how a
// "portable" abstraction turns out not to be, and the surest way to keep two
// engines in step is to give them one body of code.
//
// The list is short, and it is the same list the storage package doc has
// carried since the first backend:
//
//   - Placeholder syntax: "?" here, "$n" there.
//   - The surrogate key of an inserted row: last_insert_rowid, or RETURNING.
//   - Full-text search: FTS5, or tsvector with a GIN index. This is the one
//     genuinely divergent feature.
//   - The DDL itself, which is what the migrations directories hold.
//
// Everything else was written portably on purpose. Booleans are spelled TRUE
// and FALSE, which both engines accept; string matching runs against a
// pre-folded column rather than relying on LIKE's case sensitivity; dates and
// numbers are stored as ordinary integers and doubles; and no engine-specific
// JSON indexing appears anywhere.

// Dialect is what differs between the SQL engines.
type Dialect interface {
	// Name identifies the engine in diagnostics.
	Name() string

	// Rebind converts a statement written with "?" placeholders into the
	// engine's own syntax.
	Rebind(query string) string

	// Migrations holds the numbered .sql files defining this engine's schema,
	// under a "migrations" directory.
	Migrations() fs.FS

	// InsertResource writes the current-version row and returns its surrogate
	// key.
	InsertResource(ctx context.Context, q Querier, row ResourceRow) (int64, error)

	// WriteFullText replaces a resource's full-text row; empty text removes it.
	WriteFullText(ctx context.Context, q Querier, pid int64, narrative, content string) error

	// ClearFullText removes a resource's full-text row.
	ClearFullText(ctx context.Context, q Querier, pid int64) error

	// FullTextPredicate renders the _text or _content condition against a
	// resource alias. column is "narrative" or "content"; terms is what the
	// client searched for. It reports false when there is nothing to match.
	FullTextPredicate(alias, column, terms string) (clause string, args []any, ok bool)
}

// ResourceRow is the current-version row being inserted.
type ResourceRow struct {
	Type        string
	ID          string
	VersionID   int64
	LastUpdated int64 // microseconds since the epoch
	Content     []byte
}

// Querier is the statement surface *sql.DB and *sql.Tx have in common.
//
// Statements reaching it are already rebound, so a Dialect implementation never
// has to think about placeholders except in the SQL it writes itself.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RebindDollar converts "?" placeholders to PostgreSQL's "$n", skipping over
// single-quoted string literals.
//
// Skipping literals is not paranoia: the ORDER BY subqueries embed a quoted
// parameter code, and a naive replacement would renumber anything inside one.
func RebindDollar(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 8)
	n := 0
	inLiteral := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			// A doubled quote inside a literal is an escaped quote, not the end
			// of one.
			if inLiteral && i+1 < len(query) && query[i+1] == '\'' {
				out.WriteString("''")
				i++
				continue
			}
			inLiteral = !inLiteral
			out.WriteByte(c)
		case c == '?' && !inLiteral:
			n++
			out.WriteByte('$')
			out.WriteString(itoa(n))
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// RebindNone leaves "?" placeholders alone, for engines that use them.
func RebindNone(query string) string { return query }

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var digits [8]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
