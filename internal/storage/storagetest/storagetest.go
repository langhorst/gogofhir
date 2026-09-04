// Package storagetest is the storage conformance suite every backend must pass.
//
// It exists because a "portable" abstraction is only portable if something
// checks. The suite is written against the storage.Backend interface and knows
// nothing about SQL, so the identical assertions run on SQLite and on
// PostgreSQL; anything that cannot hold on both is a documented divergence or a
// bug, and running them side by side is the only way to tell which.
//
// It is an ordinary package rather than a _test.go file so both backends can
// import it.
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Open is how a suite gets a fresh, empty backend. Each call must return a
// database of its own: the tests assume nothing is left over from the last one.
type Open func(t *testing.T) storage.Backend

// suite is one run of the conformance tests against one backend factory.
//
// The factory travels in a value rather than a package variable so two
// backends can be exercised in one process, and so nothing stops a caller
// running the tests in parallel.
type suite struct{ open Open }

// Run executes the whole storage conformance suite against a backend.
//
// It is the parity gate. Every assertion below is about behaviour the Backend
// interface promises, not about how one engine happens to implement it, so an
// assertion that cannot hold on both engines is either a documented divergence
// or a bug -- and running the identical suite is the only way to find out which.
func Run(t *testing.T, factory Open) {
	t.Helper()
	s := suite{open: factory}
	// Named so a failure says which behaviour broke rather than which line.
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{"CreateReadRoundTrip", s.testCreateReadRoundTrip},
		{"CreateRejectsDuplicate", s.testCreateRejectsDuplicate},
		{"UpdateVersionsAndCreates", s.testUpdateVersionsAndCreates},
		{"OptimisticConcurrency", s.testOptimisticConcurrency},
		{"DeleteIsATombstoneAndIdempotent", s.testDeleteIsATombstoneAndIdempotent},
		{"VReadReachesOldVersions", s.testVReadReachesOldVersions},
		{"HistoryIsNewestFirstAndScoped", s.testHistoryIsNewestFirstAndScoped},
		{"SearchByIndexedParameters", s.testSearchByIndexedParameters},
		{"SearchByDateRange", s.testSearchByDateRange},
		{"IndexesFollowTheCurrentVersion", s.testIndexesFollowTheCurrentVersion},
		{"SearchPagingAndSort", s.testSearchPagingAndSort},
		{"CursorPagingIsStableUnderWrites", s.testCursorPagingIsStableUnderWrites},
		{"CursorRejectsMismatchedSort", s.testCursorRejectsMismatchedSort},
		{"SkipTotal", s.testSkipTotal},
		{"TxCommitsAndRollsBack", s.testTxCommitsAndRollsBack},
		{"FullText", s.testFullText},
		{"Include", s.testInclude},
		{"Composite", s.testComposite},
	}
	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}

// store opens a fresh backend for one test.
func (s suite) store(t *testing.T) storage.Backend {
	t.Helper()
	return s.open(t)
}

func doc(t *testing.T, body string) *resource.Node {
	t.Helper()
	node, err := resource.FromJSON(conformance.MustLoad(conformance.R5), []byte(body))
	if err != nil {
		t.Fatalf("parsing document: %v", err)
	}
	return node
}

func patient(t *testing.T, id, family string) *resource.Node {
	t.Helper()
	return doc(t, fmt.Sprintf(`{
	  "resourceType": "Patient",
	  "id": %q,
	  "active": true,
	  "gender": "female",
	  "birthDate": "1974-12-25",
	  "identifier": [{"system": "http://example.org/mrn", "value": "%s-mrn"}],
	  "name": [{"family": %q, "given": ["Ann"]}]
	}`, id, id, family))
}

func (s suite) testCreateReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)

	res, err := store.Create(ctx, patient(t, "p1", "Chalmers"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.VersionID != 1 {
		t.Errorf("first version = %d, want 1", res.VersionID)
	}

	got, err := store.Read(ctx, "Patient", "p1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The server stamps meta; a client's own meta is never authoritative.
	node := doc(t, string(got.Content))
	version, lastUpdated := node.Meta()
	if version != "1" {
		t.Errorf("meta.versionId = %q, want \"1\"", version)
	}
	if lastUpdated == "" {
		t.Error("meta.lastUpdated was not stamped")
	}
}

func (s suite) testCreateRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, patient(t, "p1", "B")); !errors.Is(err, storage.ErrDuplicate) {
		t.Errorf("second Create error = %v, want ErrDuplicate", err)
	}
}

func (s suite) testUpdateVersionsAndCreates(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)

	// A PUT to an unused id creates the resource; the specification permits it.
	created, res, err := store.Update(ctx, patient(t, "p1", "A"), "")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !created || res.VersionID != 1 {
		t.Errorf("created = %v, version = %d; want true, 1", created, res.VersionID)
	}

	created, res, err = store.Update(ctx, patient(t, "p1", "B"), "")
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}
	if created || res.VersionID != 2 {
		t.Errorf("created = %v, version = %d; want false, 2", created, res.VersionID)
	}
}

func (s suite) testOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}

	// A stale version must be refused rather than silently overwriting.
	if _, _, err := store.Update(ctx, patient(t, "p1", "B"), "7"); !errors.Is(err, storage.ErrConflict) {
		t.Errorf("stale If-Match error = %v, want ErrConflict", err)
	}
	if _, _, err := store.Update(ctx, patient(t, "p1", "B"), "1"); err != nil {
		t.Errorf("current If-Match should succeed, got %v", err)
	}
	// Asserting a version for a resource that does not exist is also a conflict.
	if _, _, err := store.Update(ctx, patient(t, "absent", "C"), "1"); !errors.Is(err, storage.ErrConflict) {
		t.Errorf("If-Match on an absent resource = %v, want ErrConflict", err)
	}
}

func (s suite) testDeleteIsATombstoneAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}

	existed, res, err := store.Delete(ctx, "Patient", "p1", "")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed || res.VersionID != 2 {
		t.Errorf("existed = %v, version = %d; want true, 2 (a delete is a version)", existed, res.VersionID)
	}

	// Reading a deleted resource is Gone, which is distinct from Not Found and
	// visible to clients.
	if _, err := store.Read(ctx, "Patient", "p1"); !errors.Is(err, storage.ErrDeleted) {
		t.Errorf("Read after delete = %v, want ErrDeleted", err)
	}
	if _, err := store.Read(ctx, "Patient", "never-existed"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Read of an unknown id = %v, want ErrNotFound", err)
	}

	// Deleting again is a no-op rather than an error: delete must be idempotent.
	existed, _, err = store.Delete(ctx, "Patient", "p1", "")
	if err != nil || existed {
		t.Errorf("second Delete: existed = %v, err = %v; want false, nil", existed, err)
	}
	// So is deleting something that never existed.
	if _, _, err := store.Delete(ctx, "Patient", "absent", ""); err != nil {
		t.Errorf("deleting an absent resource should succeed, got %v", err)
	}
}

func (s suite) testVReadReachesOldVersions(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "First")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, patient(t, "p1", "Second"), ""); err != nil {
		t.Fatal(err)
	}

	first, err := store.VRead(ctx, "Patient", "p1", "1")
	if err != nil {
		t.Fatalf("VRead v1: %v", err)
	}
	node := doc(t, string(first.Content))
	if got := eval(t, node, "Patient.name.family"); got != "First" {
		t.Errorf("version 1 family = %q, want First", got)
	}
	if _, err := store.VRead(ctx, "Patient", "p1", "99"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("VRead of a missing version = %v, want ErrNotFound", err)
	}
}

func (s suite) testHistoryIsNewestFirstAndScoped(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, patient(t, "p1", "B"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, doc(t, `{"resourceType":"Observation","id":"o1","status":"final","code":{"text":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Delete(ctx, "Patient", "p1", ""); err != nil {
		t.Fatal(err)
	}

	// One resource: three versions, newest first, ending with the tombstone.
	entries, err := store.History(ctx, storage.HistoryQuery{Type: "Patient", ID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("history entries = %d, want 3", len(entries))
	}
	if entries[0].VersionID != 3 || !entries[0].Deleted {
		t.Errorf("newest entry = version %d deleted=%v; want 3, true",
			entries[0].VersionID, entries[0].Deleted)
	}

	// Type-wide history excludes the Observation.
	entries, err = store.History(ctx, storage.HistoryQuery{Type: "Patient"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("Patient history = %d entries, want 3", len(entries))
	}

	// System-wide history includes everything.
	entries, err = store.History(ctx, storage.HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("system history = %d entries, want 4", len(entries))
	}
}

func (s suite) testSearchByIndexedParameters(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "Chalmers")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, patient(t, "p2", "Windsor")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query storage.SearchQuery
		want  int
	}{
		{"token by system and code", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "identifier", Kind: storage.IndexToken,
			Values: []storage.MatchValue{{System: "http://example.org/mrn", Code: "p1-mrn"}},
		}}}, 1},
		{"token by code alone", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "gender", Kind: storage.IndexToken,
			Values: []storage.MatchValue{{Code: "female"}},
		}}}, 2},
		{"string prefix, case-folded", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "chal"}},
		}}}, 1},
		{"string exact", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "Chalmers", Match: storage.MatchExact}},
		}}}, 1},
		{"string exact is case-sensitive", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "chalmers", Match: storage.MatchExact}},
		}}}, 0},
		{"string prefix folds case in the query too", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "CHAL"}},
		}}}, 1},
		{"alternatives are an or", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "chal"}, {Text: "wind"}},
		}}}, 2},
		{"separate parameters are an and", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{
			{Code: "family", Kind: storage.IndexString, Values: []storage.MatchValue{{Text: "chal"}}},
			{Code: "gender", Kind: storage.IndexToken, Values: []storage.MatchValue{{Code: "male"}}},
		}}, 0},
		{"_id", storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "_id", Values: []storage.MatchValue{{Code: "p2"}},
		}}}, 1},
		{"no parameters returns all", storage.SearchQuery{Type: "Patient"}, 2},
		{"other types are excluded", storage.SearchQuery{Type: "Observation"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(result.Matches) != tc.want || result.Total != tc.want {
				t.Errorf("got %d results (total %d), want %d", len(result.Matches), result.Total, tc.want)
			}
		})
	}
}

// A date parameter indexes the interval a partial date denotes, so a search for
// the year finds a resource dated to the day.
func (s suite) testSearchByDateRange(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}

	year := storage.MatchValue{
		DateLow:  time.Date(1974, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro(),
		DateHigh: time.Date(1974, 12, 31, 23, 59, 59, 0, time.UTC).UnixMicro(),
	}
	result, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
		Code: "birthdate", Kind: storage.IndexDate, Values: []storage.MatchValue{year},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("searching 1974 found %d, want 1", result.Total)
	}

	otherYear := storage.MatchValue{
		DateLow:  time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro(),
		DateHigh: time.Date(1980, 12, 31, 0, 0, 0, 0, time.UTC).UnixMicro(),
	}
	result, err = store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
		Code: "birthdate", Kind: storage.IndexDate, Values: []storage.MatchValue{otherYear},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("searching 1980 found %d, want 0", result.Total)
	}
}

// Indexes describe the current version only: an updated resource stops matching
// its old values, and a deleted one stops matching entirely.
func (s suite) testIndexesFollowTheCurrentVersion(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "Chalmers")); err != nil {
		t.Fatal(err)
	}

	byFamily := func(text string) int {
		t.Helper()
		result, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString, Values: []storage.MatchValue{{Text: text}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Total
	}

	if byFamily("chal") != 1 {
		t.Fatal("newly created resource is not indexed")
	}
	if _, _, err := store.Update(ctx, patient(t, "p1", "Windsor"), ""); err != nil {
		t.Fatal(err)
	}
	if got := byFamily("chal"); got != 0 {
		t.Errorf("stale index entry survived an update: found %d", got)
	}
	if got := byFamily("wind"); got != 1 {
		t.Errorf("updated value not indexed: found %d", got)
	}
	if _, _, err := store.Delete(ctx, "Patient", "p1", ""); err != nil {
		t.Fatal(err)
	}
	if got := byFamily("wind"); got != 0 {
		t.Errorf("deleted resource still matches: found %d", got)
	}
}

func (s suite) testSearchPagingAndSort(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	for _, family := range []string{"Delta", "Alpha", "Charlie", "Bravo"} {
		if _, err := store.Create(ctx, patient(t, "p-"+family, family)); err != nil {
			t.Fatal(err)
		}
	}

	q := storage.SearchQuery{
		Type:   "Patient",
		SortBy: []storage.SortKey{{Code: "family", Kind: storage.IndexString}},
		Count:  2,
	}
	first, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 4 {
		t.Errorf("total = %d, want 4 (the count ignores paging)", first.Total)
	}
	if first.Next == "" {
		t.Fatal("a full page returned no cursor")
	}
	q.Cursor = first.Next
	second, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, r := range append(first.Matches, second.Matches...) {
		order = append(order, eval(t, doc(t, string(r.Content)), "Patient.name.family"))
	}
	want := []string{"Alpha", "Bravo", "Charlie", "Delta"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("sorted order = %v, want %v", order, want)
		}
	}
}

// Cursor paging resumes by value rather than by position, so a resource
// inserted between two page fetches cannot shift the rows that follow. This is
// the whole reason for cursors: with an offset, inserting a row that sorts
// inside the first page pushes one past the boundary and the client never sees
// it.
func (s suite) testCursorPagingIsStableUnderWrites(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	for _, family := range []string{"Alpha", "Charlie", "Echo", "Golf"} {
		if _, err := store.Create(ctx, patient(t, "p-"+family, family)); err != nil {
			t.Fatal(err)
		}
	}

	q := storage.SearchQuery{
		Type:   "Patient",
		SortBy: []storage.SortKey{{Code: "family", Kind: storage.IndexString}},
		Count:  2,
	}
	first, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	// Someone inserts a resource sorting inside the page already returned.
	if _, err := store.Create(ctx, patient(t, "p-Bravo", "Bravo")); err != nil {
		t.Fatal(err)
	}

	q.Cursor = first.Next
	second, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	var seen []string
	for _, r := range append(first.Matches, second.Matches...) {
		family := eval(t, doc(t, string(r.Content)), "Patient.name.family")
		counts[family]++
		seen = append(seen, family)
	}
	for family, n := range counts {
		if n > 1 {
			t.Errorf("%s appeared %d times across pages: %v", family, n, seen)
		}
	}
	for _, want := range []string{"Alpha", "Charlie", "Echo", "Golf"} {
		if counts[want] == 0 {
			t.Errorf("%s was skipped: %v", want, seen)
		}
	}
}

// A cursor made for one sort order cannot be replayed against another: its
// values would be compared against the wrong expressions.
func (s suite) testCursorRejectsMismatchedSort(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	for i := 0; i < 3; i++ {
		if _, err := store.Create(ctx, patient(t, fmt.Sprintf("p%d", i), fmt.Sprintf("F%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	q := storage.SearchQuery{
		Type:   "Patient",
		SortBy: []storage.SortKey{{Code: "family", Kind: storage.IndexString}},
		Count:  1,
	}
	first, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.SortBy = nil
	q.Cursor = first.Next
	if _, err := store.Search(ctx, q); err == nil {
		t.Error("a cursor from a different sort order was accepted")
	}
	if _, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Cursor: "not-base64!"}); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}

// _total=none skips the count, which is a second evaluation of the predicate.
func (s suite) testSkipTotal(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}
	result, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", SkipTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 {
		t.Errorf("results = %d, want 1", len(result.Matches))
	}
	if result.HasTotal {
		t.Errorf("HasTotal is set on a query that asked for no count")
	}
}

// Tx is the transaction bundle's atomicity: every write inside it lands, or
// none does. The failure case is the one that matters -- a half-applied
// transaction leaves a client with no way to find out what happened.
func (s suite) testTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)

	err := store.Tx(ctx, func(ctx context.Context, tx storage.Backend) error {
		if _, err := tx.Create(ctx, patient(t, "kept-1", "Kept")); err != nil {
			return err
		}
		_, err := tx.Create(ctx, patient(t, "kept-2", "Kept"))
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	for _, id := range []string{"kept-1", "kept-2"} {
		if _, err := store.Read(ctx, "Patient", id); err != nil {
			t.Errorf("Read %s after a committed Tx: %v", id, err)
		}
	}

	sentinel := errors.New("deliberate")
	err = store.Tx(ctx, func(ctx context.Context, tx storage.Backend) error {
		if _, err := tx.Create(ctx, patient(t, "dropped", "Dropped")); err != nil {
			return err
		}
		// The write above is visible inside the transaction, which is what lets
		// a later bundle entry reference what an earlier one created.
		if _, err := tx.Read(ctx, "Patient", "dropped"); err != nil {
			t.Errorf("a transaction cannot see its own write: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want the sentinel", err)
	}
	if _, err := store.Read(ctx, "Patient", "dropped"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Read after a rolled-back Tx = %v, want ErrNotFound", err)
	}

	// The index has to roll back with the content; a row left behind would make
	// a deleted resource keep matching searches.
	result, err := store.Search(ctx, storage.SearchQuery{
		Type: "Patient",
		Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString,
			Values: []storage.MatchValue{{Text: "dropped", Match: storage.MatchPrefix}},
		}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Errorf("a rolled-back write left %d index rows behind", len(result.Matches))
	}
}

// observation builds an Observation with a narrative, a coded value, and an
// optional subject -- enough to exercise full text, includes, and composites.
func observation(t *testing.T, id, code, narrative, subject string, value float64) *resource.Node {
	t.Helper()
	reference := ""
	if subject != "" {
		reference = fmt.Sprintf(`"subject": {"reference": "Patient/%s"},`, subject)
	}
	return doc(t, fmt.Sprintf(`{
	  "resourceType": "Observation",
	  "id": %q,
	  "status": "final",
	  "text": {"status": "generated",
	           "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"><p>%s</p></div>"},
	  %s
	  "code": {"coding": [{"system": "http://loinc.org", "code": %q}]},
	  "valueQuantity": {"value": %v, "unit": "kg",
	                    "system": "http://unitsofmeasure.org", "code": "kg"}
	}`, id, narrative, reference, code, value))
}

// Full text is the one genuinely engine-specific feature -- FTS5 on one side,
// tsvector on the other -- which makes it the part of the parity gate most
// worth having. _text searches the narrative alone; _content searches every
// text value in the resource.
func (s suite) testFullText(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, observation(t, "o1", "29463-7", "Body Weight Measured", "", 70)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, observation(t, "o2", "8302-2", "Body Height Measured", "", 180)); err != nil {
		t.Fatal(err)
	}

	search := func(code, terms string) int {
		t.Helper()
		result, err := store.Search(ctx, storage.SearchQuery{
			Type: "Observation",
			Params: []storage.ParamMatch{{
				Code: code, Kind: storage.IndexFullText,
				Values: []storage.MatchValue{{Text: terms}},
			}},
		})
		if err != nil {
			t.Fatalf("Search %s=%q: %v", code, terms, err)
		}
		return len(result.Matches)
	}

	cases := []struct {
		code, terms string
		want        int
	}{
		{"_text", "weight", 1},
		{"_text", "measured", 2},
		// Terms combine with AND: all of these words must appear.
		{"_text", "body weight", 1},
		{"_text", "body elbow", 0},
		// Matching folds case on both engines.
		{"_text", "BODY", 2},
		// _content reaches values the narrative does not carry.
		{"_content", "29463-7", 1},
		{"_content", "kilogram", 0},
		// A term written in the engine's own query syntax is words, not syntax.
		{"_text", `weight OR height`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.code+" "+tc.terms, func(t *testing.T) {
			if got := search(tc.code, tc.terms); got != tc.want {
				t.Errorf("got %d results, want %d", got, tc.want)
			}
		})
	}

	// The full-text index follows the current version like every other index.
	if _, _, err := store.Delete(ctx, "Observation", "o1", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := search("_text", "weight"); got != 0 {
		t.Errorf("a deleted resource still matches full text: %d results", got)
	}
}

func (s suite) testInclude(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, patient(t, "p1", "Chalmers")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, observation(t, "o1", "29463-7", "Weight", "p1", 70)); err != nil {
		t.Fatal(err)
	}
	observations, err := store.Search(ctx, storage.SearchQuery{Type: "Observation"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	forward, err := store.Include(ctx, observations.Matches, []storage.IncludeSpec{
		{SourceType: "Observation", Code: "subject"},
	})
	if err != nil {
		t.Fatalf("Include: %v", err)
	}
	if len(forward) != 1 || forward[0].Type != "Patient" || forward[0].ID != "p1" {
		t.Errorf("_include returned %v, want the referenced Patient/p1", forward)
	}

	patients, err := store.Search(ctx, storage.SearchQuery{Type: "Patient"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	reverse, err := store.Include(ctx, patients.Matches, []storage.IncludeSpec{
		{Reverse: true, SourceType: "Observation", Code: "subject"},
	})
	if err != nil {
		t.Fatalf("Include: %v", err)
	}
	if len(reverse) != 1 || reverse[0].Type != "Observation" {
		t.Errorf("_revinclude returned %v, want the referencing Observation", reverse)
	}
}

// A composite asks about one occurrence: the code and the value must come from
// the same component. Matching them independently is the classic way this
// returns a confidently wrong answer, and the join that prevents it is on
// (pid, seq) -- which both engines have to render the same way.
func (s suite) testComposite(t *testing.T) {
	ctx := context.Background()
	store := s.store(t)
	if _, err := store.Create(ctx, doc(t, `{
	  "resourceType": "Observation",
	  "id": "bp",
	  "status": "final",
	  "code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
	  "component": [
	    {"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
	     "valueQuantity": {"value": 120, "unit": "mm[Hg]",
	                       "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
	    {"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
	     "valueQuantity": {"value": 80, "unit": "mm[Hg]",
	                       "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}}
	  ]
	}`)); err != nil {
		t.Fatal(err)
	}

	composite := func(code string, low, high float64, prefix string) storage.SearchQuery {
		return storage.SearchQuery{Type: "Observation", Params: []storage.ParamMatch{{
			Code: "component-code-value-quantity",
			Composite: []storage.CompositeMatch{{Components: []storage.ParamMatch{
				{
					Code: storage.CompositeComponentCode("component-code-value-quantity", 0),
					Kind: storage.IndexToken,
					Values: []storage.MatchValue{{
						System: "http://loinc.org", Code: code,
					}},
				},
				{
					Code:   storage.CompositeComponentCode("component-code-value-quantity", 1),
					Kind:   storage.IndexQuantity,
					Values: []storage.MatchValue{{Prefix: prefix, NumLow: low, NumHigh: high}},
				},
			}}},
		}}}
	}

	cases := []struct {
		name  string
		query storage.SearchQuery
		want  int
	}{
		{"systolic above 100", composite("8480-6", 100, 100, "gt"), 1},
		{"diastolic below 100", composite("8462-4", 100, 100, "lt"), 1},
		// The trap: 8480-6 is present and a value below 100 is present, but not
		// in the same component.
		{"systolic below 100", composite("8480-6", 100, 100, "lt"), 0},
		{"diastolic above 100", composite("8462-4", 100, 100, "gt"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(result.Matches) != tc.want {
				t.Errorf("got %d results, want %d", len(result.Matches), tc.want)
			}
		})
	}
}
