package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/sqlite"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(":memory:", conformance.MustLoad(conformance.R5))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
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

func TestCreateReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

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

func TestCreateRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, patient(t, "p1", "B")); !errors.Is(err, storage.ErrDuplicate) {
		t.Errorf("second Create error = %v, want ErrDuplicate", err)
	}
}

func TestUpdateVersionsAndCreates(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

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

func TestOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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

func TestDeleteIsATombstoneAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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

func TestVReadReachesOldVersions(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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

func TestHistoryIsNewestFirstAndScoped(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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

func TestSearchByIndexedParameters(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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
			Values: []storage.MatchValue{{Text: "Chalmers", Exact: true}},
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
			got, total, _, err := store.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != tc.want || total != tc.want {
				t.Errorf("got %d results (total %d), want %d", len(got), total, tc.want)
			}
		})
	}
}

// A date parameter indexes the interval a partial date denotes, so a search for
// the year finds a resource dated to the day.
func TestSearchByDateRange(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}

	year := storage.MatchValue{
		DateLow:  time.Date(1974, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro(),
		DateHigh: time.Date(1974, 12, 31, 23, 59, 59, 0, time.UTC).UnixMicro(),
	}
	_, total, _, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
		Code: "birthdate", Kind: storage.IndexDate, Values: []storage.MatchValue{year},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("searching 1974 found %d, want 1", total)
	}

	otherYear := storage.MatchValue{
		DateLow:  time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro(),
		DateHigh: time.Date(1980, 12, 31, 0, 0, 0, 0, time.UTC).UnixMicro(),
	}
	_, total, _, err = store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
		Code: "birthdate", Kind: storage.IndexDate, Values: []storage.MatchValue{otherYear},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("searching 1980 found %d, want 0", total)
	}
}

// Indexes describe the current version only: an updated resource stops matching
// its old values, and a deleted one stops matching entirely.
func TestIndexesFollowTheCurrentVersion(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.Create(ctx, patient(t, "p1", "Chalmers")); err != nil {
		t.Fatal(err)
	}

	byFamily := func(text string) int {
		t.Helper()
		_, total, _, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Params: []storage.ParamMatch{{
			Code: "family", Kind: storage.IndexString, Values: []storage.MatchValue{{Text: text}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return total
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

func TestSearchPagingAndSort(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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
	page1, total, cursor, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 (the count ignores paging)", total)
	}
	if cursor == "" {
		t.Fatal("a full page returned no cursor")
	}
	q.Cursor = cursor
	page2, _, _, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, r := range append(page1, page2...) {
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
func TestCursorPagingIsStableUnderWrites(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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
	page1, _, cursor, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	// Someone inserts a resource sorting inside the page already returned.
	if _, err := store.Create(ctx, patient(t, "p-Bravo", "Bravo")); err != nil {
		t.Fatal(err)
	}

	q.Cursor = cursor
	page2, _, _, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	var seen []string
	for _, r := range append(page1, page2...) {
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
func TestCursorRejectsMismatchedSort(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
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
	_, _, cursor, err := store.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.SortBy = nil
	q.Cursor = cursor
	if _, _, _, err := store.Search(ctx, q); err == nil {
		t.Error("a cursor from a different sort order was accepted")
	}
	if _, _, _, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", Cursor: "not-base64!"}); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}

// _total=none skips the count, which is a second evaluation of the predicate.
func TestSkipTotal(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := store.Create(ctx, patient(t, "p1", "A")); err != nil {
		t.Fatal(err)
	}
	results, total, _, err := store.Search(ctx, storage.SearchQuery{Type: "Patient", SkipTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1", len(results))
	}
	if total != -1 {
		t.Errorf("total = %d, want -1 to mean it was not computed", total)
	}
}
