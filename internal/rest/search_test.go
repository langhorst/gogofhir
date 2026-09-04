package rest_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Search behaviour: modifiers, result parameters, and paging.

// observation posts an Observation with a coded value and narrative, for the
// token, quantity, and full-text cases.
func (c *client) createObservation(code, display, narrative string, value float64) string {
	c.t.Helper()
	body := fmt.Sprintf(`{
	  "resourceType": "Observation",
	  "status": "final",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\"><p>%s</p></div>"},
	  "code": {"coding": [{"system": "http://loinc.org", "code": %q, "display": %q}], "text": %q},
	  "valueQuantity": {"value": %v, "unit": "kg", "system": "http://unitsofmeasure.org", "code": "kg"}
	}`, narrative, code, display, display, value)
	resp := c.expect(http.StatusCreated, "POST", "/Observation", body)
	id, _ := resp.json(c.t)["id"].(string)
	return id
}

// total reads a search bundle's total. Headers are passed through for the
// searches that carry a token.
func (c *client) total(query string, headers ...string) float64 {
	c.t.Helper()
	bundle := c.expect(http.StatusOK, "GET", query, "", headers...).json(c.t)
	total, _ := bundle["total"].(float64)
	return total
}

func TestStringModifiers(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	c.createPatient("B2", "MacChalmers")

	cases := []struct {
		query string
		want  float64
	}{
		// The default is a prefix match on the folded form.
		{"/Patient?family=chal", 1},
		{"/Patient?family=CHAL", 1},
		// :contains looks anywhere in the value.
		{"/Patient?family:contains=chal", 2},
		{"/Patient?family:contains=CHAL", 2},
		// :exact is the value as written, case included.
		{"/Patient?family:exact=Chalmers", 1},
		{"/Patient?family:exact=chalmers", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

// :not negates the parameter rather than each value: it means "has no value
// among these", not "has some value that is not among these". The difference
// shows on a resource with several values.
func TestNotModifier(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	c.expect(http.StatusCreated, "POST", "/Patient", `{
	  "resourceType": "Patient",
	  "identifier": [
	    {"system": "http://example.org/mrn", "value": "B2"},
	    {"system": "http://example.org/mrn", "value": "EXTRA"}
	  ]
	}`)

	if got := c.total("/Patient?identifier:not=A1"); got != 1 {
		t.Errorf("identifier:not=A1 total = %v, want 1", got)
	}
	// The two-identifier patient has a value other than EXTRA, but it does have
	// EXTRA, so :not must exclude it.
	if got := c.total("/Patient?identifier:not=EXTRA"); got != 1 {
		t.Errorf("identifier:not=EXTRA total = %v, want 1 (negates the parameter, not each value)", got)
	}
	if got := c.total("/Patient?gender:not=female"); got != 1 {
		t.Errorf("gender:not=female total = %v, want 1", got)
	}
}

// :text searches the text alongside a coded value, which extraction writes into
// the string index under the same parameter code.
func TestTokenTextModifier(t *testing.T) {
	c := newServer(t)
	c.createObservation("29463-7", "Body Weight", "Weight measurement", 70)

	if got := c.total("/Observation?code:text=body"); got != 1 {
		t.Errorf("code:text=body total = %v, want 1", got)
	}
	if got := c.total("/Observation?code:text=nonsense"); got != 0 {
		t.Errorf("code:text=nonsense total = %v, want 0", got)
	}
	// The coded form still works alongside it.
	if got := c.total("/Observation?code=http://loinc.org|29463-7"); got != 1 {
		t.Errorf("coded search total = %v, want 1", got)
	}
}

func TestURIAboveAndBelow(t *testing.T) {
	c := newServer(t)
	c.expect(http.StatusCreated, "POST", "/ValueSet", `{
	  "resourceType": "ValueSet", "status": "active",
	  "url": "http://example.org/fhir/ValueSet/things/colours"
	}`)

	cases := []struct {
		query string
		want  float64
	}{
		{"/ValueSet?url=http://example.org/fhir/ValueSet/things/colours", 1},
		{"/ValueSet?url=http://example.org/fhir/ValueSet/things", 0},
		// :below matches the value and anything under it.
		{"/ValueSet?url:below=http://example.org/fhir/ValueSet/things", 1},
		// :above matches the value and its ancestors.
		{"/ValueSet?url:above=http://example.org/fhir/ValueSet/things/colours/red", 1},
		{"/ValueSet?url:above=http://example.org/other", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReferenceSearchAndTypeModifier(t *testing.T) {
	c := newServer(t)
	patientID := c.createPatient("A1", "Chalmers")
	c.expect(http.StatusCreated, "POST", "/Observation", fmt.Sprintf(`{
	  "resourceType": "Observation", "status": "final",
	  "code": {"text": "weight"},
	  "subject": {"reference": "Patient/%s"}
	}`, patientID))

	cases := []struct {
		query string
		want  float64
	}{
		{"/Observation?subject=Patient/" + patientID, 1},
		{"/Observation?subject=" + patientID, 1},
		{"/Observation?subject:Patient=" + patientID, 1},
		{"/Observation?subject:Group=" + patientID, 0},
		{"/Observation?subject=Patient/nonexistent", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

// Modifiers that need terminology are refused with a clear reason rather than
// silently answered with nothing: an empty result reads as "no matching
// resources", which is a different and misleading claim.
func TestTerminologyModifiersAreRefused(t *testing.T) {
	c := newServer(t)
	for _, query := range []string{
		"/Observation?code:in=http://example.org/ValueSet/x",
		"/Observation?code:not-in=http://example.org/ValueSet/x",
		"/Observation?code:above=1234",
		"/Observation?code:below=1234",
	} {
		t.Run(query, func(t *testing.T) {
			resp := c.expect(http.StatusBadRequest, "GET", query, "")
			assertOutcome(t, resp, "invalid")
			if !strings.Contains(string(resp.body), "does not provide") {
				t.Errorf("the error should say what is missing: %s", resp.body)
			}
		})
	}
}

func TestInvalidModifierIsRejected(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusBadRequest, "GET", "/Patient?family:nonsense=x", "")
	assertOutcome(t, resp, "invalid")
	// :exact is valid for a string but not for a date.
	c.expect(http.StatusBadRequest, "GET", "/Patient?birthdate:exact=1974", "")
	// :missing takes a boolean.
	c.expect(http.StatusBadRequest, "GET", "/Patient?family:missing=maybe", "")
}

func TestQuantitySearch(t *testing.T) {
	c := newServer(t)
	c.createObservation("29463-7", "Body Weight", "weight", 70)

	cases := []struct {
		query string
		want  float64
	}{
		{"/Observation?value-quantity=70", 1},
		{"/Observation?value-quantity=70|http://unitsofmeasure.org|kg", 1},
		{"/Observation?value-quantity=gt60", 1},
		{"/Observation?value-quantity=lt60", 0},
		{"/Observation?value-quantity=71", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

// _text searches the narrative; _content searches every text value in the
// resource. Both go through the full-text index.
func TestFullTextSearch(t *testing.T) {
	c := newServer(t)
	c.createObservation("29463-7", "Body Weight", "Patient weighed at the clinic", 70)
	c.createObservation("8302-2", "Body Height", "Height recorded at home", 180)

	cases := []struct {
		query string
		want  float64
	}{
		{"/Observation?_text=clinic", 1},
		{"/Observation?_text=home", 1},
		{"/Observation?_text=weighed%20clinic", 1}, // terms are ANDed
		{"/Observation?_text=weighed%20home", 0},
		{"/Observation?_text=nonsense", 0},
		// _content reaches values outside the narrative, such as the code
		// display, which _text does not see.
		{"/Observation?_content=Height", 1},
		{"/Observation?_content=nonsense", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := c.total(tc.query); got != tc.want {
				t.Errorf("total = %v, want %v", got, tc.want)
			}
		})
	}
}

// A full-text value is data, not query syntax: FTS5 operators inside it must be
// treated as words rather than parsed.
func TestFullTextValueIsNotQuerySyntax(t *testing.T) {
	c := newServer(t)
	c.createObservation("1", "A", "alpha beta", 1)
	c.createObservation("2", "B", "gamma delta", 2)

	// Read as syntax, "alpha OR gamma" would match both. As words, it matches
	// neither, since no narrative contains all three.
	if got := c.total("/Observation?_text=alpha%20OR%20gamma"); got != 0 {
		t.Errorf("total = %v, want 0; the value was parsed as FTS5 syntax", got)
	}
	// A value full of operators must not error.
	c.expect(http.StatusOK, "GET", `/Observation?_text=NEAR%28%22x%22%20%22y%22%29`, "")
}

func TestSummary(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")
	c.expect(http.StatusOK, "PUT", "/Patient/"+id, fmt.Sprintf(`{
	  "resourceType": "Patient", "id": %q,
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">narrative</div>"},
	  "name": [{"family": "Chalmers"}],
	  "gender": "female",
	  "communication": [{"language": {"text": "English"}}]
	}`, id))

	// _summary=true keeps the elements the specification marks as summary.
	// Patient.communication is not one of them; name and gender are.
	summary := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_summary=true", "").json(t)
	if summary["name"] == nil {
		t.Error("_summary=true dropped a summary element (name)")
	}
	if summary["communication"] != nil {
		t.Error("_summary=true kept a non-summary element (communication)")
	}
	assertSubsetted(t, summary)

	// _summary=text keeps the narrative and little else.
	text := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_summary=text", "").json(t)
	if text["text"] == nil {
		t.Error("_summary=text dropped the narrative")
	}
	if text["name"] != nil {
		t.Error("_summary=text kept data elements")
	}

	// _summary=data is the reverse.
	data := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_summary=data", "").json(t)
	if data["text"] != nil {
		t.Error("_summary=data kept the narrative")
	}
	if data["name"] == nil {
		t.Error("_summary=data dropped data elements")
	}

	// _summary=false is the whole resource, untagged.
	full := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_summary=false", "").json(t)
	if full["communication"] == nil {
		t.Error("_summary=false subsetted the resource")
	}
	if meta, _ := full["meta"].(map[string]any); meta["tag"] != nil {
		t.Error("_summary=false marked the resource SUBSETTED")
	}

	c.expect(http.StatusBadRequest, "GET", "/Patient/"+id+"?_summary=nonsense", "")
}

// _summary=count returns the number of matches and no entries.
func TestSummaryCount(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	c.createPatient("B2", "Windsor")

	bundle := c.expect(http.StatusOK, "GET", "/Patient?_summary=count", "").json(t)
	if bundle["total"] != float64(2) {
		t.Errorf("total = %v, want 2", bundle["total"])
	}
	if bundle["entry"] != nil {
		t.Error("_summary=count returned entries")
	}
}

func TestElements(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")

	filtered := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_elements=name,gender", "").json(t)
	if filtered["name"] == nil || filtered["gender"] == nil {
		t.Error("_elements dropped a requested element")
	}
	if filtered["birthDate"] != nil || filtered["identifier"] != nil {
		t.Error("_elements kept an element that was not requested")
	}
	// id and meta always survive: without them a resource cannot be referred to
	// or version-checked.
	if filtered["id"] == nil || filtered["meta"] == nil {
		t.Error("_elements dropped id or meta")
	}
	assertSubsetted(t, filtered)
}

// Subsetting applies inside search bundles too, not only to single reads.
func TestElementsInSearchBundle(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")

	bundle := c.expect(http.StatusOK, "GET", "/Patient?_elements=gender", "").json(t)
	entries, _ := bundle["entry"].([]any)
	entry, _ := entries[0].(map[string]any)
	res, _ := entry["resource"].(map[string]any)
	if res["gender"] == nil {
		t.Error("_elements dropped the requested element in a bundle")
	}
	if res["name"] != nil {
		t.Error("_elements kept an unrequested element in a bundle")
	}
	assertSubsetted(t, res)
}

func TestTotalModes(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")

	accurate := c.expect(http.StatusOK, "GET", "/Patient?_total=accurate", "").json(t)
	if accurate["total"] != float64(1) {
		t.Errorf("_total=accurate gave %v, want 1", accurate["total"])
	}
	// _total=none skips the count entirely, so the bundle carries no total.
	none := c.expect(http.StatusOK, "GET", "/Patient?_total=none", "").json(t)
	if _, present := none["total"]; present {
		t.Errorf("_total=none returned a total: %v", none["total"])
	}
	if none["entry"] == nil {
		t.Error("_total=none dropped the entries as well as the count")
	}
	c.expect(http.StatusOK, "GET", "/Patient?_total=estimate", "")
	c.expect(http.StatusBadRequest, "GET", "/Patient?_total=nonsense", "")
}

func TestSort(t *testing.T) {
	c := newServer(t)
	for _, family := range []string{"Delta", "Alpha", "Charlie"} {
		c.createPatient("M"+family, family)
	}

	families := func(query string) []string {
		t.Helper()
		bundle := c.expect(http.StatusOK, "GET", query, "").json(t)
		var out []string
		for _, raw := range bundle["entry"].([]any) {
			entry, _ := raw.(map[string]any)
			res, _ := entry["resource"].(map[string]any)
			names, _ := res["name"].([]any)
			name, _ := names[0].(map[string]any)
			out = append(out, name["family"].(string))
		}
		return out
	}

	if got := families("/Patient?_sort=family"); !equal(got, []string{"Alpha", "Charlie", "Delta"}) {
		t.Errorf("_sort=family gave %v", got)
	}
	if got := families("/Patient?_sort=-family"); !equal(got, []string{"Delta", "Charlie", "Alpha"}) {
		t.Errorf("_sort=-family gave %v", got)
	}
	c.expect(http.StatusBadRequest, "GET", "/Patient?_sort=nonsense", "")
}

// A resource with no value for the sort parameter still has a definite place,
// so paging over a sorted set cannot lose it.
func TestSortWithMissingValues(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	c.expect(http.StatusCreated, "POST", "/Patient", `{"resourceType":"Patient","gender":"other"}`)

	bundle := c.expect(http.StatusOK, "GET", "/Patient?_sort=family", "").json(t)
	if bundle["total"] != float64(2) {
		t.Errorf("sorting dropped a resource without a value: total = %v", bundle["total"])
	}
}

func assertSubsetted(t *testing.T, res map[string]any) {
	t.Helper()
	meta, _ := res["meta"].(map[string]any)
	tags, _ := meta["tag"].([]any)
	for _, raw := range tags {
		tag, _ := raw.(map[string]any)
		if tag["code"] == "SUBSETTED" {
			return
		}
	}
	t.Error("a subsetted resource is not tagged SUBSETTED; a client cannot tell it from a sparse one")
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
