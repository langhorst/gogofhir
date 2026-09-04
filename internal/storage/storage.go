// Package storage persists FHIR resources and the indexes search runs against.
//
// The Backend interface is the only thing the rest of the server talks to, and
// no SQL appears outside its implementations. That boundary is deliberate: a
// second backend retrofitted onto a query layer that has quietly grown one
// engine's habits is exactly how a "portable" abstraction turns out not to be.
// Keeping the query surface expressed as values rather than strings is what
// made the PostgreSQL port a translation rather than a rewrite.
//
// There is one SQL implementation, in internal/storage/sqlstore, and the
// engines supply only what they genuinely do differently. What keeps them
// honest is internal/storage/storagetest: the identical suite runs against
// both, so an assertion that cannot hold on one is a documented divergence or
// a bug rather than a surprise in production.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/langhorst/gogofhir/internal/resource"
)

// Errors the REST layer maps onto status codes.
var (
	// ErrNotFound: no such resource. 404.
	ErrNotFound = errors.New("storage: resource not found")
	// ErrDeleted: the resource existed and was deleted. 410 Gone, which is
	// distinct from 404 and observable to clients.
	ErrDeleted = errors.New("storage: resource deleted")
	// ErrConflict: an If-Match precondition did not hold. 409.
	ErrConflict = errors.New("storage: version conflict")
	// ErrDuplicate: a create would collide with an existing id. 409.
	ErrDuplicate = errors.New("storage: resource already exists")
	// ErrMultipleMatches: a conditional operation matched more than one
	// resource, which the specification makes an error rather than a choice.
	ErrMultipleMatches = errors.New("storage: conditional operation matched multiple resources")
)

// Resource is one stored version of a resource.
type Resource struct {
	Type string
	// ID is the logical id a client sees, which is an arbitrary string chosen
	// by the client or the server -- not the storage key.
	ID string
	// VersionID counts from 1 and increments on every write, including the
	// delete that tombstones a resource.
	VersionID   int64
	LastUpdated time.Time
	// Deleted marks a tombstone: the row records that a version existed and was
	// removed, which history and 410 responses both need.
	Deleted bool
	// Content is the canonical JSON of this version, with meta.versionId and
	// meta.lastUpdated already stamped. Empty for a tombstone.
	Content []byte
}

// HistoryQuery selects a slice of the version history.
type HistoryQuery struct {
	// Type and ID narrow the scope: both empty is system-wide history, Type
	// alone is type-wide, both set is one resource's history.
	Type string
	ID   string
	// Since restricts to versions updated at or after this instant.
	Since time.Time
	// Count is the maximum number of entries; zero means the backend default.
	Count int
	// Offset skips entries, for paging.
	Offset int
}

// SearchQuery is a conjunction of parameter matches.
//
// This is the query *plan*, not a query string: parsing the FHIR search syntax
// -- modifiers, prefixes, chaining -- happens above, and rendering to SQL
// happens inside a backend. Neither end knows about the other.
type SearchQuery struct {
	Type   string
	Params []ParamMatch
	Count  int
	Offset int
	SortBy []SortKey

	// Cursor resumes a previous page. When set, Offset is ignored: paging is by
	// keyset rather than by offset, so a resource created between two page
	// fetches cannot shift the ones after it.
	Cursor string

	// SkipTotal omits the count query, for _total=none. Counting is a second
	// full scan of the predicate, and a client paging through results rarely
	// needs it more than once.
	SkipTotal bool

	// Filter is the _filter expression, ANDed with Params. It is a tree rather
	// than a list because _filter is the one part of FHIR search that can say
	// "or" and "not" across whole parameters -- the query string itself can
	// only express a conjunction.
	Filter *FilterExpr
}

// FilterExpr is a boolean combination of parameter matches, from _filter.
//
// A node is either a leaf holding one Match, or an operator with Operands.
// "not" takes exactly one operand.
type FilterExpr struct {
	// Op is FilterAnd, FilterOr, or FilterNot; empty for a leaf.
	Op       FilterOp
	Operands []*FilterExpr
	Match    *ParamMatch
}

// FilterOp is a boolean connective in a _filter expression.
type FilterOp string

const (
	FilterAnd FilterOp = "and"
	FilterOr  FilterOp = "or"
	FilterNot FilterOp = "not"
)

// IncludeSpec is one _include or _revinclude request.
type IncludeSpec struct {
	// Reverse distinguishes _revinclude (find resources pointing at the
	// matches) from _include (find resources the matches point at).
	Reverse bool
	// SourceType is the resource type holding the reference. For _include it is
	// the type being searched; for _revinclude it is the type doing the
	// referencing.
	SourceType string
	// Code is the reference search parameter to follow. Empty with Wildcard
	// set means every reference.
	Code     string
	Wildcard bool
	// TargetType optionally restricts which referenced type is pulled in.
	TargetType string
	// Iterate repeats the expansion over what it finds, for :iterate.
	Iterate bool
}

// ParamMatch is one indexed parameter constrained to a value.
type ParamMatch struct {
	// Code is the search parameter's name, as it appears in a query string.
	Code string
	// Kind is the index the match runs against.
	Kind IndexKind
	// Values are alternatives: a match succeeds if any of them does, which is
	// how FHIR's comma-separated "or" works.
	Values []MatchValue
	// Negate inverts the whole match, for the :not modifier. It negates the
	// parameter rather than each value, which is the difference between "has no
	// value in this list" and "has some value not in this list" -- the
	// specification means the former.
	Negate bool
	// TextSearch redirects a token parameter to the string index, for the :text
	// modifier: extraction writes a CodeableConcept's text there under the same
	// parameter code.
	TextSearch bool

	// Chain turns this into a join through a reference: the resources this
	// parameter points at must themselves match Chain's parameters. It is how
	// "subject.name=peter" is expressed.
	Chain *Chain
	// Has is the reverse join: some resource must reference this one and match.
	// It is how "_has:Observation:subject:code=1234" is expressed.
	Has *Has
	// Composite holds the alternatives of a composite parameter, of which any
	// one satisfies the match. Within an alternative every component must be
	// satisfied by the *same* occurrence, which is what makes a composite
	// different from writing the components as separate parameters.
	Composite []CompositeMatch
}

// CompositeMatch is one alternative of a composite parameter: the component
// matches that a single occurrence of the composite's base expression has to
// satisfy together.
type CompositeMatch struct {
	Components []ParamMatch
}

// Chain is a forward reference join, from "subject.name=peter" or
// "subject:Patient.name=peter". Chains nest, so Params may contain further
// chained matches.
type Chain struct {
	// TargetType restricts which referenced type is followed, from the
	// "subject:Patient" form. Empty follows any.
	TargetType string
	Params     []ParamMatch
}

// Has is a reverse reference join, from "_has:Observation:subject:code=1234":
// find resources that some Observation points at through its subject
// parameter, where that Observation matches the remaining criteria.
type Has struct {
	// SourceType is the referencing resource type.
	SourceType string
	// Code is the reference parameter on the source that points back here.
	Code   string
	Params []ParamMatch
}

// MatchValue is one alternative within a ParamMatch.
type MatchValue struct {
	// System and Code together match a token; Code alone matches any system.
	System string
	Code   string
	// Text matches a string index; Match says how. A prefix or contains match
	// is compared on the folded form, an exact one on the value as written.
	Text  string
	Match StringMatch
	// Reference matches a reference index by target type and id, or by the
	// reference URL when the query gives an absolute one.
	RefType string
	RefID   string
	RefURL  string
	// DateLow and DateHigh bound a date range in microseconds since the epoch.
	DateLow, DateHigh int64
	// NumLow and NumHigh bound a number or quantity range.
	NumLow, NumHigh float64
	// QuantitySystem and QuantityCode narrow a quantity to a unit.
	QuantitySystem, QuantityCode string
	// URI matches a uri index exactly.
	URI string
	// Prefix is the comparison the value carries: eq, ne, gt, lt, ge, le, sa,
	// eb, or ap. Empty means eq.
	Prefix string
	// Prefix scoping for uri parameters: Above matches the stored value's
	// ancestors, Below its descendants.
	URIAbove bool
	URIBelow bool

	// Missing, when set, matches resources that have (or lack) the parameter
	// rather than any particular value.
	Missing *bool
}

// StringMatch is how a string index value is compared.
type StringMatch int

const (
	// MatchPrefix is FHIR's default for string parameters: the stored value
	// starts with the query value, compared on the folded form.
	MatchPrefix StringMatch = iota
	// MatchExact compares the value as written, case and accents included.
	MatchExact
	// MatchContains looks anywhere in the value, for the :contains modifier.
	MatchContains
	// MatchEndsWith anchors at the end. No query-string modifier produces it;
	// it exists for _filter's "ew" operator.
	MatchEndsWith
)

// SortKey is one ordering term.
type SortKey struct {
	Code       string
	Kind       IndexKind
	Descending bool
}

// IndexKind names one of the search index tables.
//
// FHIR defines nine search parameter types, and each is indexed differently:
// a token needs a system and a code, a date needs a range, a reference needs a
// target type and id. Splitting them into typed tables -- rather than one
// stringly-typed table or engine-specific JSON indexes -- is what keeps the
// schema portable and the queries ordinary B-tree lookups.
type IndexKind string

const (
	IndexString    IndexKind = "string"
	IndexToken     IndexKind = "token"
	IndexReference IndexKind = "reference"
	IndexDate      IndexKind = "date"
	IndexQuantity  IndexKind = "quantity"
	IndexURI       IndexKind = "uri"
	IndexNumber    IndexKind = "number"
	// IndexFullText backs _text and _content. It is not a typed index table
	// like the others but a full-text index, and it is the one place the two
	// backends genuinely diverge: SQLite uses FTS5 and PostgreSQL will use
	// tsvector. Everything else is ordinary B-tree lookups on both.
	IndexFullText IndexKind = "fulltext"
)

// IndexEntry is one extracted, indexable value.
type IndexEntry struct {
	Code string
	Kind IndexKind

	// Token
	System string
	Value  string

	// String: Normalized is folded for matching, Exact keeps the original.
	Normalized string
	Exact      string

	// Reference
	RefType string
	RefID   string
	RefURL  string

	// DateLow and DateHigh bound the instant range a date covers, in
	// microseconds since the epoch.
	//
	// Dates are ranges because "2024" denotes a year rather than an instant:
	// storing a point makes every prefix comparison subtly wrong, and it is the
	// single most common way FHIR date search goes astray. Microseconds keep
	// the column an ordinary integer, which both engines index identically.
	DateLow, DateHigh int64

	// NumLow and NumHigh bound a number or quantity, which are ranges for the
	// same reason: a result recorded as 1.1 means [1.05, 1.15).
	NumLow, NumHigh float64

	// Quantity
	QuantitySystem string
	QuantityCode   string

	// URI
	URI string

	// Seq groups the rows that came from the same occurrence of a composite
	// parameter's base expression. Ordinary parameters leave it zero.
	Seq int
}

// Backend is the persistence contract.
//
// Every method takes a context so a slow query cannot outlive its request.
type Backend interface {
	// Create stores a new resource, assigning version 1. The node's id must
	// already be set. It returns ErrDuplicate if the id is taken.
	Create(ctx context.Context, node *resource.Node) (*Resource, error)

	// Update stores a new version, creating the resource if absent (which FHIR
	// permits: a PUT to an unused id is a create). ifMatch, when non-empty, is
	// the version the caller believes is current; a mismatch is ErrConflict.
	Update(ctx context.Context, node *resource.Node, ifMatch string) (created bool, res *Resource, err error)

	// Read returns the current version, or ErrDeleted if it is a tombstone.
	Read(ctx context.Context, resourceType, id string) (*Resource, error)

	// VRead returns one specific version, tombstones included.
	VRead(ctx context.Context, resourceType, id, versionID string) (*Resource, error)

	// Delete tombstones a resource. Deleting an absent resource succeeds and
	// reports existed=false, which the specification requires so that delete is
	// idempotent.
	Delete(ctx context.Context, resourceType, id, ifMatch string) (existed bool, res *Resource, err error)

	// History returns versions newest first.
	History(ctx context.Context, q HistoryQuery) ([]*Resource, error)

	// Search returns the current versions matching a query, the total number of
	// matches irrespective of paging, and a cursor for the next page (empty
	// when there is none). Total is -1 when the query asked for it to be
	// skipped.
	Search(ctx context.Context, q SearchQuery) (matches []*Resource, total int, nextCursor string, err error)

	// Tx runs fn against a backend whose writes commit together: every Create,
	// Update, and Delete fn performs either lands or none of them do.
	// Returning an error rolls all of them back.
	//
	// It exists for transaction bundles, where atomicity is the whole point --
	// a half-applied transaction leaves a client with no way to know what
	// happened. Reads inside fn see fn's own uncommitted writes, which is what
	// lets a later entry reference a resource an earlier one created. Nested
	// calls join the enclosing transaction rather than starting a new one.
	Tx(ctx context.Context, fn func(context.Context, Backend) error) error

	// Include resolves _include and _revinclude against a set of matches,
	// returning the additional resources to place in the bundle. Resources
	// already among the seeds are not returned again.
	Include(ctx context.Context, seeds []*Resource, specs []IncludeSpec) ([]*Resource, error)

	// Close releases the backend's resources.
	Close() error
}
