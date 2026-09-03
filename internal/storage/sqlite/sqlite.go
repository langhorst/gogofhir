// Package sqlite implements the storage backend on SQLite.
//
// It is the default and, through v1, the only backend: a dataset is one file a
// developer can copy, reset, or commit as a fixture, and the server needs no
// database to run. PostgreSQL follows the same schema and interface later.
//
// modernc.org/sqlite is a pure-Go translation of SQLite, so there is no CGo and
// nothing to install -- which is what keeps the single-static-binary promise.
//
// Dialect notes, kept current so the PostgreSQL port is a checklist rather than
// an archaeology exercise:
//
//   - AUTOINCREMENT is SQLite's spelling; PostgreSQL uses a generated identity.
//   - Placeholders are "?" here and "$n" there.
//   - Type affinity is loose: INTEGER and REAL are advisory, and a mismatched
//     Go type will be coerced rather than rejected.
//   - Upserts use "ON CONFLICT ... DO UPDATE", which both engines support.
//   - Only one writer at a time. Reads are concurrent under WAL.
//   - Full-text search (_text, _content) will need FTS5 here and tsvector
//     there; it is the one genuinely divergent feature.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the SQLite-backed storage.Backend.
type Store struct {
	db        *sql.DB
	idx       *conformance.Index
	extractor *storage.Extractor
	// now is the clock, replaceable in tests so lastUpdated is predictable.
	now func() time.Time
}

var _ storage.Backend = (*Store)(nil)

// Open opens (creating if needed) a database at path. The path ":memory:"
// yields a private in-memory database, which is what tests use.
func Open(path string, idx *conformance.Index) (*Store, error) {
	dsn := dsnFor(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: opening %s: %w", path, err)
	}
	// One connection serializes every access. SQLite allows a single writer
	// regardless, and this sidesteps SQLITE_BUSY between our own goroutines
	// without a retry loop. It is also the reason concurrency is a documented
	// limit of this backend rather than a bug.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, idx: idx, extractor: storage.NewExtractor(idx), now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func dsnFor(path string) string {
	pragmas := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		return "file::memory:?" + pragmas
	}
	return "file:" + filepath.ToSlash(path) + "?" + pragmas
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("storage: migrations table: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("storage: applying %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- writes ----

// Create stores a new resource at version 1.
func (s *Store) Create(ctx context.Context, node *resource.Node) (*storage.Resource, error) {
	if node.ID() == "" {
		return nil, errors.New("storage: create requires an id")
	}
	created, res, err := s.write(ctx, node, "", writeCreate)
	_ = created
	return res, err
}

// Update stores a new version, creating the resource if the id is unused.
func (s *Store) Update(ctx context.Context, node *resource.Node, ifMatch string) (bool, *storage.Resource, error) {
	if node.ID() == "" {
		return false, nil, errors.New("storage: update requires an id")
	}
	return s.write(ctx, node, ifMatch, writeUpdate)
}

type writeMode int

const (
	writeCreate writeMode = iota
	writeUpdate
)

// write is the single path through which a version is stored, so versioning,
// history, and indexing cannot drift apart: they all happen in one transaction,
// and a failure anywhere leaves nothing behind.
func (s *Store) write(ctx context.Context, node *resource.Node, ifMatch string, mode writeMode) (created bool, out *storage.Resource, err error) {
	resourceType, id := node.FHIRType(), node.ID()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()

	var pid, currentVersion int64
	var currentDeleted bool
	row := tx.QueryRowContext(ctx,
		`SELECT pid, version_id, deleted FROM resource WHERE resource_type = ? AND fhir_id = ?`,
		resourceType, id)
	switch err := row.Scan(&pid, &currentVersion, &currentDeleted); {
	case errors.Is(err, sql.ErrNoRows):
		if mode == writeCreate {
			created = true
		} else {
			// A PUT to an unused id creates the resource, which the
			// specification permits and clients rely on.
			created = true
		}
	case err != nil:
		return false, nil, err
	default:
		if mode == writeCreate {
			return false, nil, storage.ErrDuplicate
		}
		if ifMatch != "" && ifMatch != strconv.FormatInt(currentVersion, 10) {
			return false, nil, storage.ErrConflict
		}
	}
	if ifMatch != "" && created {
		// The client asserted a version for something that does not exist.
		return false, nil, storage.ErrConflict
	}

	version := currentVersion + 1
	updated := s.now().UTC().Truncate(time.Millisecond)

	// Stamp the server-owned metadata onto a copy: the caller's document is
	// theirs, and a client's meta is never authoritative.
	stamped := node.Clone()
	stamped.SetMeta(strconv.FormatInt(version, 10), updated)
	content, err := stamped.MarshalJSON()
	if err != nil {
		return false, nil, fmt.Errorf("storage: serializing resource: %w", err)
	}

	micros := updated.UnixMicro()
	if created {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO resource (resource_type, fhir_id, version_id, last_updated, deleted, content)
			VALUES (?, ?, ?, ?, 0, ?)`,
			resourceType, id, version, micros, content)
		if err != nil {
			return false, nil, err
		}
		if pid, err = res.LastInsertId(); err != nil {
			return false, nil, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE resource SET version_id = ?, last_updated = ?, deleted = 0, content = ?
			WHERE pid = ?`, version, micros, content, pid); err != nil {
			return false, nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_history (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES (?, ?, ?, ?, 0, ?)`,
		resourceType, id, version, micros, content); err != nil {
		return false, nil, err
	}

	if err := s.reindex(ctx, tx, pid, stamped); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return created, &storage.Resource{
		Type: resourceType, ID: id, VersionID: version,
		LastUpdated: updated, Content: content,
	}, nil
}

// Delete tombstones a resource. Deleting something absent succeeds: the
// specification makes delete idempotent, so a client retrying a delete must not
// see an error.
func (s *Store) Delete(ctx context.Context, resourceType, id, ifMatch string) (bool, *storage.Resource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()

	var pid, currentVersion int64
	var deleted bool
	row := tx.QueryRowContext(ctx,
		`SELECT pid, version_id, deleted FROM resource WHERE resource_type = ? AND fhir_id = ?`,
		resourceType, id)
	switch err := row.Scan(&pid, &currentVersion, &deleted); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil, nil
	case err != nil:
		return false, nil, err
	}
	if ifMatch != "" && ifMatch != strconv.FormatInt(currentVersion, 10) {
		return false, nil, storage.ErrConflict
	}
	if deleted {
		// Already a tombstone; deleting again changes nothing.
		return false, nil, nil
	}

	version := currentVersion + 1
	updated := s.now().UTC().Truncate(time.Millisecond)
	micros := updated.UnixMicro()

	if _, err := tx.ExecContext(ctx, `
		UPDATE resource SET version_id = ?, last_updated = ?, deleted = 1, content = NULL
		WHERE pid = ?`, version, micros, pid); err != nil {
		return false, nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_history (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES (?, ?, ?, ?, 1, NULL)`, resourceType, id, version, micros); err != nil {
		return false, nil, err
	}
	// A deleted resource must stop matching searches, so its index rows go.
	if err := s.clearIndex(ctx, tx, pid); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, &storage.Resource{
		Type: resourceType, ID: id, VersionID: version,
		LastUpdated: updated, Deleted: true,
	}, nil
}

// ---- indexing ----

// indexTables maps each index kind to its table and columns, so writing and
// clearing stay in step with the schema in one place.
var indexTables = map[storage.IndexKind]struct {
	table   string
	columns []string
}{
	storage.IndexString:    {"idx_string", []string{"code", "norm", "exact"}},
	storage.IndexToken:     {"idx_token", []string{"code", "system", "value"}},
	storage.IndexReference: {"idx_reference", []string{"code", "target_type", "target_id", "url"}},
	storage.IndexDate:      {"idx_date", []string{"code", "low", "high"}},
	storage.IndexQuantity:  {"idx_quantity", []string{"code", "low", "high", "system", "unit"}},
	storage.IndexURI:       {"idx_uri", []string{"code", "value"}},
	storage.IndexNumber:    {"idx_number", []string{"code", "low", "high"}},
}

func (s *Store) reindex(ctx context.Context, tx *sql.Tx, pid int64, node *resource.Node) error {
	if err := s.clearIndex(ctx, tx, pid); err != nil {
		return err
	}
	narrative, content := s.extractor.FullText(node)
	if narrative != "" || content != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idx_fulltext (rowid, narrative, content) VALUES (?, ?, ?)`,
			pid, narrative, content); err != nil {
			return fmt.Errorf("storage: full-text indexing: %w", err)
		}
	}
	for _, entry := range s.extractor.Extract(node) {
		spec, ok := indexTables[entry.Kind]
		if !ok {
			continue
		}
		values := indexValues(entry)
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(values)+1), ", ")
		query := fmt.Sprintf("INSERT INTO %s (pid, %s) VALUES (%s)",
			spec.table, strings.Join(spec.columns, ", "), placeholders)
		if _, err := tx.ExecContext(ctx, query, append([]any{pid}, values...)...); err != nil {
			return fmt.Errorf("storage: indexing %s: %w", entry.Code, err)
		}
	}
	return nil
}

// indexValues returns an entry's column values in the order indexTables lists
// them.
func indexValues(e storage.IndexEntry) []any {
	switch e.Kind {
	case storage.IndexString:
		return []any{e.Code, e.Normalized, e.Exact}
	case storage.IndexToken:
		return []any{e.Code, e.System, e.Value}
	case storage.IndexReference:
		return []any{e.Code, e.RefType, e.RefID, e.RefURL}
	case storage.IndexDate:
		return []any{e.Code, e.DateLow, e.DateHigh}
	case storage.IndexQuantity:
		return []any{e.Code, e.NumLow, e.NumHigh, e.QuantitySystem, e.QuantityCode}
	case storage.IndexURI:
		return []any{e.Code, e.URI}
	case storage.IndexNumber:
		return []any{e.Code, e.NumLow, e.NumHigh}
	}
	return nil
}

func (s *Store) clearIndex(ctx context.Context, tx *sql.Tx, pid int64) error {
	for _, spec := range indexTables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+spec.table+" WHERE pid = ?", pid); err != nil {
			return err
		}
	}
	// The full-text table is keyed by rowid rather than a pid column, so it is
	// cleared separately.
	if _, err := tx.ExecContext(ctx, "DELETE FROM idx_fulltext WHERE rowid = ?", pid); err != nil {
		return err
	}
	return nil
}

// ---- reads ----

// Read returns the current version.
func (s *Store) Read(ctx context.Context, resourceType, id string) (*storage.Resource, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT version_id, last_updated, deleted, content
		FROM resource WHERE resource_type = ? AND fhir_id = ?`, resourceType, id)
	res, err := scanResource(row, resourceType, id)
	if err != nil {
		return nil, err
	}
	if res.Deleted {
		return res, storage.ErrDeleted
	}
	return res, nil
}

// VRead returns one specific version, tombstones included.
func (s *Store) VRead(ctx context.Context, resourceType, id, versionID string) (*storage.Resource, error) {
	version, err := strconv.ParseInt(versionID, 10, 64)
	if err != nil {
		return nil, storage.ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT version_id, last_updated, deleted, content
		FROM resource_history
		WHERE resource_type = ? AND fhir_id = ? AND version_id = ?`, resourceType, id, version)
	res, err := scanResource(row, resourceType, id)
	if err != nil {
		return nil, err
	}
	if res.Deleted {
		return res, storage.ErrDeleted
	}
	return res, nil
}

func scanResource(row *sql.Row, resourceType, id string) (*storage.Resource, error) {
	var (
		version int64
		micros  int64
		deleted bool
		content []byte
	)
	switch err := row.Scan(&version, &micros, &deleted, &content); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, storage.ErrNotFound
	case err != nil:
		return nil, err
	}
	return &storage.Resource{
		Type: resourceType, ID: id, VersionID: version,
		LastUpdated: time.UnixMicro(micros).UTC(),
		Deleted:     deleted, Content: content,
	}, nil
}

// History returns versions newest first, across one resource, one type, or the
// whole server depending on how the query is scoped.
func (s *Store) History(ctx context.Context, q storage.HistoryQuery) ([]*storage.Resource, error) {
	var where []string
	var args []any
	if q.Type != "" {
		where = append(where, "resource_type = ?")
		args = append(args, q.Type)
	}
	if q.ID != "" {
		where = append(where, "fhir_id = ?")
		args = append(args, q.ID)
	}
	if !q.Since.IsZero() {
		where = append(where, "last_updated >= ?")
		args = append(args, q.Since.UnixMicro())
	}
	query := `SELECT resource_type, fhir_id, version_id, last_updated, deleted, content FROM resource_history`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY last_updated DESC, version_id DESC"
	count := q.Count
	if count <= 0 {
		count = 100
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, count, q.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*storage.Resource
	for rows.Next() {
		var (
			res     storage.Resource
			micros  int64
			content []byte
		)
		if err := rows.Scan(&res.Type, &res.ID, &res.VersionID, &micros, &res.Deleted, &content); err != nil {
			return nil, err
		}
		res.LastUpdated = time.UnixMicro(micros).UTC()
		res.Content = content
		out = append(out, &res)
	}
	return out, rows.Err()
}
