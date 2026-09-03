// Package sqlstore implements storage.Backend over SQL.
//
// It is the whole implementation for every engine gogofhir supports. What the
// engines do differently is supplied through a Dialect -- placeholder syntax,
// how an insert reports its surrogate key, full-text search, and the DDL -- and
// nothing else is duplicated. A second backend written alongside the first is
// exactly how a "portable" abstraction turns out not to be, so there is only
// one.
//
// The schema is ordinary relational tables with B-tree indexes, not
// engine-specific JSON indexing. That is the lesson worth taking from HAPI's
// design, and the reason parity is achievable at all.
//
// Portability is written into the shared half rather than negotiated in the
// seam: booleans are spelled TRUE and FALSE, which both engines accept; string
// matching runs against a pre-folded column rather than relying on LIKE's case
// sensitivity; "ESCAPE '\'" is standard and behaves identically; dates and
// numbers are ordinary integers and doubles. What is left is in Dialect.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Store is the SQL-backed storage.Backend.
type Store struct {
	db *sql.DB
	// q is what statements actually run against: the pool normally, or an open
	// transaction for a store scoped by Tx. Everything below goes through it,
	// so one implementation serves both cases and a transaction bundle's reads
	// see its own uncommitted writes.
	q         Querier
	dialect   Dialect
	idx       *conformance.Index
	extractor *storage.Extractor
	// now is the clock, replaceable in tests so lastUpdated is predictable.
	now func() time.Time
}

var _ storage.Backend = (*Store)(nil)

// Open connects with a driver and prepares the schema.
//
// Callers use the per-engine constructors -- sqlite.Open, postgres.Open --
// which supply the driver name and the dialect. tune, when given, configures
// the pool before the first statement runs.
func Open(driverName, dsn string, idx *conformance.Index, dialect Dialect, tune func(*sql.DB)) (*Store, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: opening %s: %w", dialect.Name(), err)
	}
	if tune != nil {
		tune(db)
	}
	s := &Store{
		db: db, q: db, dialect: dialect, idx: idx,
		extractor: storage.NewExtractor(idx), now: time.Now,
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Engine names the SQL engine in use, for diagnostics and the
// CapabilityStatement.
func (s *Store) Engine() string { return s.dialect.Name() }

// DB exposes the connection pool, for the health checks and administrative
// statements a deployment needs. Queries against resources go through the
// Backend interface; this is not a way around it.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database. A store scoped to a transaction shares its
// parent's pool, so closing it is a no-op rather than a way to take the whole
// server's database down from inside one request.
func (s *Store) Close() error {
	if s.nested() {
		return nil
	}
	return s.db.Close()
}

func (s *Store) nested() bool {
	_, ok := s.q.(*sql.Tx)
	return ok
}

// exec, query, queryRow and queryRowIn are the only places statements reach the
// database.
//
// Rebinding here rather than at each call site is what lets every query in this
// package be written once, with "?" placeholders, and still run on an engine
// that spells them "$1".
func (s *Store) exec(ctx context.Context, q Querier, query string, args ...any) (sql.Result, error) {
	return q.ExecContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.q.QueryContext(ctx, s.dialect.Rebind(query), args...)
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.q.QueryRowContext(ctx, s.dialect.Rebind(query), args...)
}

// queryRowIn runs against a specific querier, for the reads a write performs
// inside its own transaction.
func (s *Store) queryRowIn(ctx context.Context, q Querier, query string, args ...any) *sql.Row {
	return q.QueryRowContext(ctx, s.dialect.Rebind(query), args...)
}

// Tx runs fn against a store scoped to one database transaction.
//
// Atomicity is the point: a transaction bundle that half-applies leaves a
// client unable to tell what happened. Nesting joins the enclosing transaction
// instead of starting a second one, because SQLite has no independent nested
// transactions and a savepoint would give the inner one the power to commit
// while the outer rolls back.
func (s *Store) Tx(ctx context.Context, fn func(context.Context, storage.Backend) error) error {
	if s.nested() {
		return fn(ctx, s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	scoped := *s
	scoped.q = tx
	if err := fn(ctx, &scoped); err != nil {
		return err
	}
	return tx.Commit()
}

// inTx runs a write in a transaction, joining an enclosing one where there is
// one. Every write goes through it, so versioning, history, and indexing can
// never land without each other.
func (s *Store) inTx(ctx context.Context, fn func(q Querier) error) error {
	if tx, ok := s.q.(*sql.Tx); ok {
		// The enclosing transaction owns the commit; committing here would
		// release writes the caller may still roll back.
		return fn(tx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("storage: migrations table: %w", err)
	}
	files := s.dialect.Migrations()
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := s.db.QueryRow(s.dialect.Rebind(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`), name).Scan(&applied); err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		if applied > 0 {
			continue
		}
		body, err := fs.ReadFile(files, name)
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
		if _, err := tx.Exec(s.dialect.Rebind(
			`INSERT INTO schema_migrations (name) VALUES (?)`), name); err != nil {
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
	err = s.inTx(ctx, func(tx Querier) error {
		created, out, err = s.writeIn(ctx, tx, node, ifMatch, mode)
		return err
	})
	if err != nil {
		return false, nil, err
	}
	return created, out, nil
}

func (s *Store) writeIn(ctx context.Context, tx Querier, node *resource.Node, ifMatch string, mode writeMode) (created bool, out *storage.Resource, err error) {
	resourceType, id := node.FHIRType(), node.ID()

	var pid, currentVersion int64
	var currentDeleted bool
	row := s.queryRowIn(ctx, tx,
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
		// How an insert reports the key it assigned is one of the few things
		// the engines genuinely differ on, so it goes through the dialect.
		pid, err = s.dialect.InsertResource(ctx, tx, ResourceRow{
			Type: resourceType, ID: id, VersionID: version,
			LastUpdated: micros, Content: content,
		})
		if err != nil {
			return false, nil, err
		}
	} else {
		if _, err := s.exec(ctx, tx, `
			UPDATE resource SET version_id = ?, last_updated = ?, deleted = FALSE, content = ?
			WHERE pid = ?`, version, micros, content, pid); err != nil {
			return false, nil, err
		}
	}

	if _, err := s.exec(ctx, tx, `
		INSERT INTO resource_history (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES (?, ?, ?, ?, FALSE, ?)`,
		resourceType, id, version, micros, content); err != nil {
		return false, nil, err
	}

	if err := s.reindex(ctx, tx, pid, stamped); err != nil {
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
func (s *Store) Delete(ctx context.Context, resourceType, id, ifMatch string) (existed bool, out *storage.Resource, err error) {
	err = s.inTx(ctx, func(tx Querier) error {
		existed, out, err = s.deleteIn(ctx, tx, resourceType, id, ifMatch)
		return err
	})
	if err != nil {
		return false, nil, err
	}
	return existed, out, nil
}

func (s *Store) deleteIn(ctx context.Context, tx Querier, resourceType, id, ifMatch string) (bool, *storage.Resource, error) {
	var pid, currentVersion int64
	var deleted bool
	row := s.queryRowIn(ctx, tx,
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

	if _, err := s.exec(ctx, tx, `
		UPDATE resource SET version_id = ?, last_updated = ?, deleted = TRUE, content = NULL
		WHERE pid = ?`, version, micros, pid); err != nil {
		return false, nil, err
	}
	if _, err := s.exec(ctx, tx, `
		INSERT INTO resource_history (resource_type, fhir_id, version_id, last_updated, deleted, content)
		VALUES (?, ?, ?, ?, TRUE, NULL)`, resourceType, id, version, micros); err != nil {
		return false, nil, err
	}
	// A deleted resource must stop matching searches, so its index rows go.
	if err := s.clearIndex(ctx, tx, pid); err != nil {
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
	storage.IndexString:    {"idx_string", []string{"code", "seq", "norm", "exact"}},
	storage.IndexToken:     {"idx_token", []string{"code", "seq", "system", "value"}},
	storage.IndexReference: {"idx_reference", []string{"code", "seq", "target_type", "target_id", "url"}},
	storage.IndexDate:      {"idx_date", []string{"code", "seq", "low", "high"}},
	storage.IndexQuantity:  {"idx_quantity", []string{"code", "seq", "low", "high", "system", "unit"}},
	storage.IndexURI:       {"idx_uri", []string{"code", "seq", "value"}},
	storage.IndexNumber:    {"idx_number", []string{"code", "seq", "low", "high"}},
}

func (s *Store) reindex(ctx context.Context, tx Querier, pid int64, node *resource.Node) error {
	if err := s.clearIndex(ctx, tx, pid); err != nil {
		return err
	}
	narrative, content := s.extractor.FullText(node)
	if narrative != "" || content != "" {
		if err := s.dialect.WriteFullText(ctx, tx, pid, narrative, content); err != nil {
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
		if _, err := s.exec(ctx, tx, query, append([]any{pid}, values...)...); err != nil {
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
		return []any{e.Code, e.Seq, e.Normalized, e.Exact}
	case storage.IndexToken:
		return []any{e.Code, e.Seq, e.System, e.Value}
	case storage.IndexReference:
		return []any{e.Code, e.Seq, e.RefType, e.RefID, e.RefURL}
	case storage.IndexDate:
		return []any{e.Code, e.Seq, e.DateLow, e.DateHigh}
	case storage.IndexQuantity:
		return []any{e.Code, e.Seq, e.NumLow, e.NumHigh, e.QuantitySystem, e.QuantityCode}
	case storage.IndexURI:
		return []any{e.Code, e.Seq, e.URI}
	case storage.IndexNumber:
		return []any{e.Code, e.Seq, e.NumLow, e.NumHigh}
	}
	return nil
}

func (s *Store) clearIndex(ctx context.Context, tx Querier, pid int64) error {
	for _, spec := range indexTables {
		if _, err := s.exec(ctx, tx, "DELETE FROM "+spec.table+" WHERE pid = ?", pid); err != nil {
			return err
		}
	}
	// Full text is keyed differently by each engine, so it is cleared through
	// the dialect rather than here.
	return s.dialect.ClearFullText(ctx, tx, pid)
}

// ---- reads ----

// Read returns the current version.
func (s *Store) Read(ctx context.Context, resourceType, id string) (*storage.Resource, error) {
	row := s.queryRow(ctx, `
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
	row := s.queryRow(ctx, `
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

	rows, err := s.query(ctx, query, args...)
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
