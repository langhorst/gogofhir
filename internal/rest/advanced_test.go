package rest_test

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// Advanced search: chaining, _has, _include/_revinclude, composites, _filter.
//
// These are the parts of search that reach beyond one resource, and the ones
// where a wrong answer looks like a right one -- an empty bundle reads as "no
// matching resources" whether the join was wrong or the data simply is not
// there. The assertions below are therefore mostly about counts on data where
// the correct count is not zero.

// createObservationFor posts an Observation pointing at a patient.
func (c *client) createObservationFor(patientID, code string, value float64) string {
	c.t.Helper()
	body := fmt.Sprintf(`{
	  "resourceType": "Observation",
	  "status": "final",
	  "subject": {"reference": "Patient/%s"},
	  "code": {"coding": [{"system": "http://loinc.org", "code": %q}]},
	  "valueQuantity": {"value": %v, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
	}`, patientID, code, value)
	resp := c.expect(http.StatusCreated, "POST", "/Observation", body)
	id, _ := resp.json(c.t)["id"].(string)
	return id
}

// entryModes lists a bundle's entries as "Type/mode", sorted, which is how the
// include tests assert that matches and includes are distinguished.
func (c *client) entryModes(query string) []string {
	c.t.Helper()
	bundle := c.expect(http.StatusOK, "GET", query, "").json(c.t)
	entries, _ := bundle["entry"].([]any)
	var out []string
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		res, _ := entry["resource"].(map[string]any)
		search, _ := entry["search"].(map[string]any)
		mode, _ := search["mode"].(string)
		out = append(out, fmt.Sprintf("%v/%s", res["resourceType"], mode))
	}
	sort.Strings(out)
	return out
}

// assertBadRequest checks that a query is refused with an OperationOutcome
// mentioning a phrase, so the client is told which link of the query failed
// rather than handed an empty bundle.
func assertBadRequest(t *testing.T, c *client, query, phrase string) {
	t.Helper()
	resp := c.expect(http.StatusBadRequest, "GET", query, "")
	assertOutcome(t, resp, "invalid")
	if !strings.Contains(string(resp.body), phrase) {
		t.Errorf("%s: outcome does not mention %q\nbody: %s", query, phrase, resp.body)
	}
}

func TestChainedSearch(t *testing.T) {
	c := newServer(t)
	chalmers := c.createPatient("A1", "Chalmers")
	other := c.createPatient("B2", "Nowak")
	c.createObservationFor(chalmers, "29463-7", 70)
	c.createObservationFor(other, "29463-7", 80)

	cases := []struct {
		query string
		want  float64
	}{
		// The type modifier names which referenced type the chain follows.
		{"/Observation?subject:Patient.family=chal", 1},
		// "patient" is a reference with a single target, so no modifier is
		// needed to disambiguate it.
		{"/Observation?patient.family=chal", 1},
		{"/Observation?patient.family=now", 1},
		{"/Observation?patient.family=missing", 0},
		// Chained parameters combine with ordinary ones.
		{"/Observation?patient.family=chal&code=29463-7", 1},
		{"/Observation?patient.family=chal&code=nope", 0},
		// A chain through _id resolves against the referenced resource.
		{"/Observation?patient._id=" + chalmers, 1},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

// A chain through a reference with several possible targets has to be resolved
// against one of them. Guessing would silently search the wrong type, so an
// unresolvable chain is a 400 that names the candidates.
func TestChainedSearchAmbiguity(t *testing.T) {
	c := newServer(t)
	assertBadRequest(t, c, "/Observation?subject.family=chal", "is ambiguous")
	assertBadRequest(t, c, "/Observation?subject:Encounter.family=chal", "does not reference Encounter")
	assertBadRequest(t, c, "/Observation?code.family=x", "only reference parameters can be chained")
	assertBadRequest(t, c, "/Observation?nosuch.family=x", "cannot be chained")
	assertBadRequest(t, c, "/Observation?subject:Patient.nosuch=x",
		"does not support the search parameter")
}

func TestReverseChain(t *testing.T) {
	c := newServer(t)
	chalmers := c.createPatient("A1", "Chalmers")
	c.createPatient("B2", "Nowak")
	c.createObservationFor(chalmers, "29463-7", 70)

	cases := []struct {
		query string
		want  float64
	}{
		{"/Patient?_has:Observation:subject:code=29463-7", 1},
		{"/Patient?_has:Observation:subject:code=99999-9", 0},
		// The reverse chain composes with the forward one: patients that some
		// observation of this code points at, whose own family matches.
		{"/Patient?_has:Observation:subject:code=29463-7&family=chal", 1},
		{"/Patient?_has:Observation:subject:code=29463-7&family=now", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}

	assertBadRequest(t, c, "/Patient?_has:Observation:subject=x",
		"_has takes the form")
	assertBadRequest(t, c, "/Patient?_has:Nonesuch:subject:code=1",
		"unknown resource type")
	assertBadRequest(t, c, "/Patient?_has:Observation:code:code=1",
		"_has needs a reference parameter")
}

// The distinction between a match and an include is mandatory: a client that
// cannot tell them apart cannot tell which resources answered its query.
func TestIncludeAndRevinclude(t *testing.T) {
	c := newServer(t)
	patient := c.createPatient("A1", "Chalmers")
	c.createObservationFor(patient, "29463-7", 70)

	forward := c.entryModes("/Observation?code=29463-7&_include=Observation:subject")
	if want := []string{"Observation/match", "Patient/include"}; !equalStrings(forward, want) {
		t.Errorf("_include entries = %v, want %v", forward, want)
	}

	reverse := c.entryModes("/Patient?family=chal&_revinclude=Observation:subject")
	if want := []string{"Observation/include", "Patient/match"}; !equalStrings(reverse, want) {
		t.Errorf("_revinclude entries = %v, want %v", reverse, want)
	}

	// An included resource is not counted in the total, which reports matches.
	if got := c.total("/Observation?code=29463-7&_include=Observation:subject"); got != 1 {
		t.Errorf("total with _include = %v, want 1 (includes are not matches)", got)
	}

	// The same resource reached twice is returned once.
	both := c.entryModes("/Observation?code=29463-7&_include=Observation:subject&_include=Observation:patient")
	if want := []string{"Observation/match", "Patient/include"}; !equalStrings(both, want) {
		t.Errorf("two includes of the same target = %v, want %v", both, want)
	}

	assertBadRequest(t, c, "/Observation?_include=Observation:code",
		"needs a reference parameter")
	assertBadRequest(t, c, "/Observation?_include=Patient:link",
		"but the search is over Observation")
	assertBadRequest(t, c, "/Observation?_include:deep=Observation:subject",
		"the only modifier _include accepts")
}

// :iterate follows what an include found, which is the only way to reach a
// resource two references away.
func TestIncludeIterate(t *testing.T) {
	c := newServer(t)
	organization := c.expect(http.StatusCreated, "POST", "/Organization",
		`{"resourceType":"Organization","name":"General Hospital"}`).json(t)["id"].(string)
	practitioner := c.expect(http.StatusCreated, "POST", "/Practitioner",
		`{"resourceType":"Practitioner","name":[{"family":"House"}]}`).json(t)["id"].(string)
	role := c.expect(http.StatusCreated, "POST", "/PractitionerRole", fmt.Sprintf(
		`{"resourceType":"PractitionerRole","practitioner":{"reference":"Practitioner/%s"},`+
			`"organization":{"reference":"Organization/%s"}}`, practitioner, organization)).
		json(t)["id"].(string)
	c.expect(http.StatusCreated, "POST", "/Patient", fmt.Sprintf(
		`{"resourceType":"Patient","name":[{"family":"Chalmers"}],`+
			`"generalPractitioner":[{"reference":"PractitionerRole/%s"}]}`, role))

	// One level: the role itself.
	oneLevel := c.entryModes("/Patient?family=chal&_include=Patient:general-practitioner")
	if want := []string{"Patient/match", "PractitionerRole/include"}; !equalStrings(oneLevel, want) {
		t.Errorf("one-level include = %v, want %v", oneLevel, want)
	}

	// Two levels: :iterate re-runs the second include over what the first
	// found, reaching the organization the role points at.
	twoLevels := c.entryModes(
		"/Patient?family=chal&_include=Patient:general-practitioner" +
			"&_include:iterate=PractitionerRole:organization")
	want := []string{"Organization/include", "Patient/match", "PractitionerRole/include"}
	if !equalStrings(twoLevels, want) {
		t.Errorf("iterated include = %v, want %v", twoLevels, want)
	}
}

// createBloodPressure posts an Observation whose two components carry the same
// element structure with different codes and values -- the case a composite
// parameter exists to answer.
func (c *client) createBloodPressure(systolic, diastolic float64) string {
	c.t.Helper()
	body := fmt.Sprintf(`{
	  "resourceType": "Observation",
	  "status": "final",
	  "code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
	  "component": [
	    {"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
	     "valueQuantity": {"value": %v, "unit": "mm[Hg]", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}},
	    {"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
	     "valueQuantity": {"value": %v, "unit": "mm[Hg]", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}}
	  ]
	}`, systolic, diastolic)
	resp := c.expect(http.StatusCreated, "POST", "/Observation", body)
	id, _ := resp.json(c.t)["id"].(string)
	return id
}

// A composite asks about one occurrence. On a blood pressure with systolic 120
// and diastolic 80, "systolic below 100" must not match: the code and the value
// have to come from the same component, and matching them independently is the
// classic way this returns a confidently wrong answer.
func TestCompositeSearch(t *testing.T) {
	c := newServer(t)
	c.createBloodPressure(120, 80)

	cases := []struct {
		query string
		want  float64
	}{
		{"/Observation?component-code-value-quantity=http://loinc.org|8480-6$gt100", 1},
		{"/Observation?component-code-value-quantity=http://loinc.org|8462-4$lt100", 1},
		// The trap: 8480-6 is present and a value below 100 is present, but not
		// in the same component.
		{"/Observation?component-code-value-quantity=http://loinc.org|8480-6$lt100", 0},
		{"/Observation?component-code-value-quantity=http://loinc.org|8462-4$gt100", 0},
		// Comma-separated alternatives are alternatives of whole tuples.
		{"/Observation?component-code-value-quantity=" +
			"http://loinc.org|8480-6$lt100,http://loinc.org|8462-4$lt100", 1},
		// A composite over the resource itself, rather than a repeating element.
		{"/Observation?code-value-quantity=http://loinc.org|85354-9$gt100", 0},
		// :missing asks whether the composite matched anything at all.
		{"/Observation?component-code-value-quantity:missing=false", 1},
		{"/Observation?component-code-value-quantity:missing=true", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}

	assertBadRequest(t, c, "/Observation?component-code-value-quantity=http://loinc.org|8480-6",
		"$-separated components")
	assertBadRequest(t, c, "/Observation?component-code-value-quantity:exact=a$b",
		"not valid for the composite parameter")
}

func TestFilter(t *testing.T) {
	c := newServer(t)
	chalmers := c.createPatient("A1", "Chalmers")
	c.createPatient("B2", "Nowak")
	c.createObservationFor(chalmers, "29463-7", 70)

	cases := []struct {
		filter string
		want   float64
	}{
		// "eq" on a string is equality, not the prefix match a bare parameter
		// performs: the operator says what it means.
		{`name eq "Chalmers"`, 1},
		{`name eq "Chal"`, 0},
		{`name sw "Chal"`, 1},
		{`name co "halme"`, 1},
		{`name ew "mers"`, 1},
		{`name ne "Chalmers"`, 1},
		// and, or, not, and grouping -- the whole reason _filter exists.
		{`name eq "Chalmers" or name eq "Nowak"`, 2},
		{`name eq "Chalmers" and gender eq female`, 1},
		{`name eq "Chalmers" and gender eq male`, 0},
		{`not (name eq "Chalmers")`, 1},
		{`(name eq "Chalmers" or name eq "Nowak") and gender eq female`, 2},
		{`name eq "Chalmers" or (name eq "Nowak" and gender eq male)`, 1},
		// Presence, and the ordered comparisons.
		{`birthdate pr true`, 2},
		{`birthdate pr false`, 0},
		{`birthdate gt 1970-01-01`, 2},
		{`birthdate lt 1970-01-01`, 0},
		{`birthdate ge 1974-12-25`, 2},
		// Tokens compare by system and code.
		{`identifier eq http://example.org/mrn|A1`, 1},
		{`identifier ne http://example.org/mrn|A1`, 1},
		// A chain inside a filter resolves like any other chained parameter.
		{`_id eq ` + chalmers, 1},
	}
	for _, tc := range cases {
		t.Run(tc.filter, func(t *testing.T) {
			query := "/Patient?_filter=" + url.QueryEscape(tc.filter)
			if got := c.total(query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}

	// Chained and reverse-chained leaves.
	if got := c.total("/Observation?_filter=" + url.QueryEscape(`patient.family sw "Chal"`)); got != 1 {
		t.Errorf("chained filter total = %v, want 1", got)
	}
	if got := c.total("/Observation?_filter=" + url.QueryEscape(`subject[Patient].family sw "Chal"`)); got != 1 {
		t.Errorf("bracketed chain total = %v, want 1", got)
	}

	// _filter combines with ordinary parameters, and repeats conjoin.
	if got := c.total("/Patient?gender=female&_filter=" + url.QueryEscape(`name sw "Chal"`)); got != 1 {
		t.Errorf("filter with a parameter = %v, want 1", got)
	}
	if got := c.total("/Patient?_filter=" + url.QueryEscape(`name sw "Chal"`) +
		"&_filter=" + url.QueryEscape(`gender eq male`)); got != 0 {
		t.Errorf("two filters = %v, want 0 (they conjoin)", got)
	}
}

// Operators the specification defines but which need terminology this server
// does not have are refused rather than answered with nothing: an empty bundle
// would read as "no matching resources".
func TestFilterErrors(t *testing.T) {
	c := newServer(t)
	cases := []struct{ filter, phrase string }{
		{`gender in http://example.org/vs`, "a value set expansion"},
		{`gender sb something`, "a code system hierarchy"},
		{`name`, "expects a comparison operator"},
		{`name eq`, "expects a value"},
		{`(name eq "x"`, "unclosed parenthesis"},
		{`name eq "x")`, "trailing input"},
		{`name eq "x` + ``, "unterminated quoted string"},
		{`nosuch eq 1`, "does not support the search parameter"},
		{`birthdate co 1970`, "is not valid for the date parameter"},
		{`not name eq "x"`, "parenthesized expression"},
	}
	for _, tc := range cases {
		t.Run(tc.filter, func(t *testing.T) {
			assertBadRequest(t, c, "/Patient?_filter="+url.QueryEscape(tc.filter), tc.phrase)
		})
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
