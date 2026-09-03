# gogofhir

A FHIR server in Go — a dev/test server and conformance target. One static
binary, an embedded conformance package, and a SQLite file: run it with no
dependencies, point a FHIR client at it, and run it in CI to prove that client
behaves.

The name is a Dhalsim joke. Go for Go, go for let's go, fhir for FHIR.

> **Status: M7a complete.** `gogofhir serve` gives you CRUD, versioning,
> history, conditional operations, optimistic concurrency, atomic transaction
> and batch bundles, the whole of search — every indexed parameter type with its
> modifiers, full-text, chaining, `_has`, `_include`/`_revinclude`, composites,
> `_filter`, `_summary`, `_elements`, `_total`, cursor-stable paging — and
> `$validate` against the type system, the specification's invariants, embedded
> value sets, and profiles with slicing. Over 158 resource types, in JSON and
> XML, on **SQLite or PostgreSQL** — one implementation, with both engines
> passing the identical storage and REST suites. The FHIRPath engine passes
> **1966 of 1998** official HL7 conformance tests. See
> [Milestones](#milestones).

## Why

Existing options force a trade. HAPI FHIR is complete but is a JVM deployment
with real operational weight. The lightweight servers are lightweight because
they skip the parts that make conformance testing meaningful — profile
validation, chained search, transactions.

A Go server can plausibly have both: a single binary a developer runs with no
setup, that still answers `_has` chains and validates against US Core.

Design follows the specification (R4 4.0.1 / R5 5.0.0) directly, taking
architectural lessons — not code — from HAPI FHIR, particularly its relational
search-index model.

## Design decisions

| Decision | Choice |
|---|---|
| Resource model | Schema-driven untyped JSON documents — no generated resource structs |
| FHIR versions | R4 and R5, **one per server instance**, selected at startup |
| Conformance data | Compiled at build time into an embedded index |
| FHIRPath | Full engine, own package |
| Validation | Structural + profiles + invariants |
| Storage | SQLite and PostgreSQL, one implementation |
| Tenancy | Single tenant |

**Untyped documents** are the load-bearing choice. Rather than generating Go
structs for 158 resource types, resources stay JSON and the compiled
conformance index supplies everything a generated type would have carried:
element types, cardinality, choice expansions, bindings, invariants. All
resource types work on day one, and supporting a second FHIR release becomes a
data-loading problem instead of a second code-generation tree.

**One release per instance** then removes the version dimension entirely — no
code outside `internal/conformance` knows which release it is serving.

## Quick start

```sh
make build                            # no network, no vendored packages needed
./bin/gogofhir serve -db :memory:     # or -db fhir.db to keep the data

# Stricter, for a conformance run:
#   -validate-writes       refuse resources with validation errors
#   -strict-terminology    a binding that cannot be checked offline is an error
#   -fhir r4               serve R4 instead of R5
```

```sh
curl -X POST localhost:8080/Patient -H 'Content-Type: application/fhir+json' \
  -d '{"resourceType":"Patient","name":[{"family":"Chalmers"}],"birthDate":"1974-12-25"}'
# 201 Created, ETag: W/"1", Location: .../Patient/<id>/_history/1

curl 'localhost:8080/Patient?family=chal&birthdate=1974'   # searchset Bundle
curl -H 'Accept: application/fhir+xml' localhost:8080/Patient/<id>
curl localhost:8080/metadata                               # CapabilityStatement
```

`./bin/gogofhir conformance -fhir r5` summarizes the embedded index instead:

```
release        r5 (5.0.0)
package        hl7.fhir.r5.core
resources      158
datatypes      69
search params  1988 bindings
invariants     383
compartments   Device, Encounter, Patient, Practitioner, RelatedPerson
```

## The REST API

Every resource type in the release is served, with no per-type code:

```
GET    /metadata                          generated CapabilityStatement
GET    /{type}/{id}                       read, with ETag and If-None-Match
GET    /{type}/{id}/_history/{vid}        vread
POST   /{type}                            create (server assigns the id)
PUT    /{type}/{id}                       update, or create at a chosen id
DELETE /{type}/{id}                       delete
GET    /{type}?...                        search
POST   /{type}/_search                    search with criteria in the body
GET    /{type}/{id}/_history              history, per resource
GET    /{type}/_history  ·  GET /_history history, per type and system-wide
POST   /{type}   If-None-Exist: ...       conditional create
PUT    /{type}?...  ·  DELETE /{type}?... conditional update and delete
POST   /                                  transaction and batch bundles
POST   /{type}/$validate                  validate without storing
POST   /{type}/{id}/$validate             validate a replacement, or what is stored
```

`PATCH` is not implemented: it is a distinct interaction with its own body
formats (JSON Patch, FHIRPath Patch), and a bundle entry using it is refused
rather than silently ignored.

Behaviours worth stating, because each is easy to get subtly wrong:

- **A delete is a version, not an erasure.** Reading a deleted resource is
  `410 Gone`, not `404` — the client is entitled to know it existed — and the
  prior versions stay reachable through `vread`. Delete is idempotent, so
  repeating one, or deleting something absent, still succeeds.
- **Optimistic concurrency.** `If-Match` with a stale version is `412`, not a
  silent overwrite. `If-None-Match` on a read gives `304` with no body.
- **A conditional operation matching several resources is an error** (`412`),
  never a guess at which one was meant.
- **Every error carries an OperationOutcome.** There are no bare status codes.
- **JSON and XML are the same server.** Both are read and written by one pair
  of codecs over one document model, so a resource created as XML reads back
  identically as JSON.

## Transactions and batches

`POST /` takes a Bundle whose entries are RESTful interactions. The two kinds
differ in one thing that changes everything.

A **batch** is a convenience: independent interactions posted together, each
succeeding or failing on its own. A **transaction** is a unit — entries may
refer to each other, and either all of them happen or none do.

```jsonc
{"resourceType": "Bundle", "type": "transaction", "entry": [
  {"fullUrl": "urn:uuid:1",
   "resource": {"resourceType": "Patient", "name": [{"family": "Chalmers"}]},
   "request": {"method": "POST", "url": "Patient",
               "ifNoneExist": "identifier=http://example.org/mrn|A1"}},
  {"resource": {"resourceType": "Observation", "status": "final",
                "subject": {"reference": "urn:uuid:1"}},
   "request": {"method": "POST", "url": "Observation"}}
]}
```

- **Internal references are resolved.** The two entries above are posted
  together precisely because neither resource exists yet: the Observation names
  the Patient by the placeholder in its `fullUrl`, and the server substitutes
  the id it assigned. Finding those references is schema-driven rather than a
  search for JSON keys named `reference` — `DetectedIssue.reference`,
  `Expression.reference` and three others are plain URIs, and rewriting one
  would corrupt the resource.
- **Conditional references** name a resource by search criteria instead of by
  id (`"reference": "Patient?identifier=..."`), for when the client knows an
  identifier but not what the server called it. No match, or more than one, is
  an error rather than a guess.
- **Identities are settled before anything is written**, which is what makes a
  reference to a not-yet-created resource resolvable at all. Conditional
  creates, updates and deletes are evaluated in the same pass, so by execution
  time every entry is a plain instance-level interaction.
- **Entries execute in the specification's order** — delete, create, update,
  read. It is not arbitrary: deleting before creating lets one transaction
  replace a resource, and reading last means a `GET` observes the
  transaction's own writes. Responses come back in request order regardless.
- **A resource may be touched only once.** Two entries writing one resource
  have no defined order and no defined outcome, so it is an error rather than a
  race the server resolves silently.
- **A failed transaction leaves nothing behind**, and the outcome names the
  entry that failed and why. Storage exposes one `Tx` on the backend and every
  entry runs inside it, index rows included.

Entries are executed by dispatching them back through the server's own handler.
A create inside a bundle is therefore the *same* create — same conditional
handling, same status codes, same OperationOutcome — rather than a second
implementation that drifts from the first.

## Search

Every parameter type is indexed and searchable, with the modifiers and prefixes
the specification defines:

| | |
|---|---|
| token | `identifier=system\|code`, `\|code`, `system\|`, `:not`, `:text`, `:of-type` |
| string | prefix by default, `:exact`, `:contains` |
| reference | `subject=Patient/123`, `subject:Patient=123`, `:identifier` |
| date | `birthdate=ge1974`, and `eq ne gt lt ge le sa eb ap` |
| quantity | `value-quantity=gt60\|http://unitsofmeasure.org\|kg` |
| uri | `url:below=`, `url:above=` |
| number | prefixes as for dates |
| special | `_text` over the narrative, `_content` over every text value |
| all | `:missing`, comma-separated alternatives, `_id`, `_lastUpdated` |

Result parameters: `_sort` (multi-key, `-` for descending), `_count`,
`_summary` (`true`/`text`/`data`/`count`/`false`), `_elements`, and `_total`
(`none` skips the count entirely, which is a second evaluation of the
predicate). A subsetted resource is tagged `SUBSETTED` — without it a client
cannot tell a filtered resource from a sparse one, which is how a display bug
becomes a clinical one.

**Paging is by cursor.** The `next` link carries an opaque token encoding where
the previous page stopped, so a resource created between two fetches cannot
shift the rows after it. Offset paging silently repeats or skips rows under
concurrent writes, and a conformance suite paging through a dataset someone
else is writing to will find that. There is deliberately no `last` or
`previous` link: both need an offset to point at, which a keyset cursor has
not got.

**Modifiers that need terminology are refused, not faked.** `:in`, `:not-in`,
and `:above`/`:below` on a token need a value set expansion or a code system
hierarchy. Answering them with an empty result would read as "no matching
resources", which is a different and misleading claim, so they return a 400
saying what is missing.

### Beyond one resource

**Chaining** follows a reference and applies the far side's criteria:
`Observation?subject:Patient.family=chal`, or `patient.family=chal` where the
reference has a single target. A reference that could point at several types is
resolved against the one that defines the next parameter, and a genuine
ambiguity is a 400 naming the candidates — `subject.family` matches both
`Practitioner` and `Patient`, and guessing would silently search the wrong one.
Chains nest to four levels; deeper is refused, because each level is another
join and an untrusted query should not be able to buy that cheaply.

**`_has`** is the same join reversed:
`Patient?_has:Observation:subject:code=29463-7` finds the patients some
matching observation points at.

**`_include` and `_revinclude`** run after the match query rather than as part
of it, so an include never changes which resources matched. Entries carry
`search.mode` — `match` or `include` — because a client that cannot tell them
apart cannot tell which resources answered its query. `:iterate` follows what
an earlier include found, bounded to five rounds with cycle detection: FHIR
reference graphs contain cycles, and following them unbounded is a request that
never finishes.

**Composite parameters** ask about one occurrence.
`component-code-value-quantity=http://loinc.org|8480-6$lt100` must not match a
blood pressure of 120/80: the code and the value have to come from the *same*
component, even though both conditions are individually satisfied. Extraction
tags every row from one occurrence of the composite's base expression with a
shared sequence number, and the query joins on it. Matching the components
independently is the classic way this returns a confidently wrong answer.

**`_filter`** is the one part of search with its own grammar — `and`, `or`,
`not(…)`, grouping, and comparison operators — over the same parameters,
chains, and `_has` clauses:

```
GET /Patient?_filter=(name eq "Chalmers" or name eq "Nowak") and birthdate gt 1970
GET /Observation?_filter=patient.family sw "Chal" and value-quantity gt 60
```

`eq` on a string is equality rather than the prefix match a bare parameter
performs, since the operator says what it means; `co`, `sw`, and `ew` are the
substring forms. The operators that need terminology — `in`, `ni`, `ss`, `sb`
— are refused for the same reason the modifiers are.

Parameters that are declared but not indexed are deliberately absent from the
CapabilityStatement rather than advertised and broken.

## Validation

`$validate` answers "would this be accepted, and what is wrong with it" without
storing anything. Four layers run together and report as one OperationOutcome:

| Layer | What it checks |
|---|---|
| Structure | Every element is defined; cardinality holds; a choice element carries one value; repetition and JSON shape agree; primitives match the lexical pattern their type declares; references point at permitted types |
| Invariants | The specification's own FHIRPath constraints, on every type and element |
| Bindings | Coded values against the value sets required and extensible bindings name |
| Profiles | `meta.profile` and the `profile` parameter, including slices told apart by discriminator |

```sh
curl -X POST -H 'Content-Type: application/fhir+json'   --data '{"resourceType":"Patient","gender":"lady","birthDate":"25-12-1974"}'   'http://localhost:8080/Patient/$validate'
```

```
error  Patient.birthDate  "25-12-1974" is not a valid date
error  Patient.gender     "lady" is not in .../ValueSet/administrative-gender,
                          which this element is required bound to (4 codes)
```

A resource with problems is still a **successful** operation — the question was
answered. Only a malformed request is an error status.

**Nothing is reported as valid that was not actually checked.** That is the rule
the rest of this section follows from. A validator whose silence cannot be
distinguished from a pass is worse than no validator, because a conformance
target that overstates what it verified is one nobody can rely on.

- **Value sets are expanded at build time**, so required bindings are checked
  with no terminology server and no network. Coverage is 334 of 346 value sets
  for R4 and 357 of 373 for R5, counting every required and extensible binding
  in the type system and in profiles.
- **What cannot be expanded is named, not glossed.** SNOMED CT is licensed;
  LOINC and RxNorm are too large to embed; UCUM, ISO 3166, ISO 4217, BCP 13,
  BCP 47, IANA time zones and DICOM are external standards the packages only
  reference. A binding to one of those reports "the required binding to *X* was
  **NOT checked**: it draws on *Y*, which is defined outside this package".
  `--strict-terminology` turns those into errors for teams who have wired up a
  terminology service.
- **An invariant that cannot be evaluated says so.** R4 spells `dom-3` with
  `.as(canonical)` over `descendants()`, and `as()` is defined for a single
  item — so on any document with more than one descendant the expression is
  unevaluable. It is reported as not checked. (R5 spells the same rule with
  `ofType()`, and there it evaluates.)
- **Slices within slices are reported as unchecked**, and named, rather than
  passed over.

Profiles come from the release's own package, compiled from their published
snapshots — generating a snapshot from a differential means replaying a chain of
constraints down a derivation tree, and the packages already ship the answer.
Slices are assigned by discriminator (`value`, `pattern`, `type`, `exists`); a
discriminator this server cannot evaluate makes the whole assignment
undecidable and is reported, because a wrong slice is worse than no slice.

**Writes are not validated by default.** This server exists to be developed
against, and a developer building up a data model wants their half-finished
resources to round-trip; one that refuses them is a server people work around
rather than with. `--validate-writes` refuses a resource with errors (`422`,
since the request was understood and the content is what failed), which is what
a conformance run wants.

### The example corpus

Every resource HL7 ships with the FHIRPath suites is validated on every `make
check`, and the errors found are pinned in `expectedErrors` — enforced in both
directions, so a file that starts failing fails the build and so does one that
stops. Every pinned entry is a defect in the example, with the reason written
down:

| File | Why |
|---|---|
| `codesystem-example` (R4, R5) | Ships a duplicate concept code, annotated `<!-- wrong! -->` in the source, so `csd-1` fails as it should |
| `explanationofbenefit-example` | A fragment for exercising FHIRPath, carrying none of the resource's required elements |
| `patient-container-example` | Its contained Organization has only an id, which `org-1` forbids; nothing references it, which `dom-3` forbids |
| `valueset-example-expansion` (R5) | Claims `meta.profile = shareablevalueset` and omits the title that profile requires |

The full HL7 validator manifest is **not** driven yet: its expectations are the
reference implementation's own message wording and issue counts, which a second
implementation does not reproduce by writing better code. Matching it means
building a message-equivalence mapping, which is its own piece of work.

## Storage

**SQLite and PostgreSQL, from one implementation.** `-db fhir.db` for a file,
`-db :memory:` for nothing at all, `-db postgres://...` for a server.

One `Backend` interface, and no SQL outside its implementations. The query
surface is a typed plan rather than a string, so the search layer never writes
SQL and the backend never parses FHIR syntax — which is what made the
PostgreSQL port a translation rather than a rewrite.

The schema is ordinary relational tables with B-tree indexes, not
engine-specific JSON indexing: a `resource` table, a `resource_history` table,
and one index table per search parameter type. Two details carry most of the
correctness:

- **Dates are indexed as intervals**, because `2024` denotes a year and not an
  instant. The search prefixes are then interval algebra over those bounds.
  Storing a point instead is the most common way FHIR date search goes quietly
  wrong. Numbers work the same way: `1.1` means anything that would round to it
  — and their intervals are half-open, so a search for `71` does not match a
  stored `70` where `[69.5, 70.5)` and `[70.5, 71.5)` would otherwise touch.
- **The logical id is separate from the surrogate key.** Clients choose
  arbitrary ids and those ids appear in references, but joins want an integer.
- **Composite components carry a sequence number.** Every index row records
  which occurrence of a composite's base expression it came from, so a
  composite query can require its components to agree on one. Ordinary
  parameters leave it at zero and never consult it.

Indexes are written in the same transaction as the resource, so an index can
never describe a version that is not stored, and they follow the current
version: an updated resource stops matching its old values, a deleted one stops
matching at all.

### One implementation, two engines

There is a single SQL implementation, in `internal/storage/sqlstore`. A
`Dialect` supplies what the engines genuinely do differently, and the list is
short enough to read:

| | SQLite | PostgreSQL |
|---|---|---|
| Placeholders | `?` | `$1`, renumbered at the driver boundary |
| Surrogate key of an insert | `last_insert_rowid` | `RETURNING pid` |
| Full text | FTS5 virtual table | `tsvector` with a GIN index |
| DDL | `migrations/*.sql` | `migrations/*.sql` |

Everything else was written portably on purpose rather than negotiated in the
seam: booleans are spelled `TRUE`/`FALSE`, which both accept; string matching
runs against a pre-folded column so it never depends on whether `LIKE` ignores
case; `ESCAPE '\'` is standard; dates and numbers are ordinary integers and
doubles. Writing the second backend as a second *implementation* is exactly how
a portable abstraction turns out not to be, so there is only one.

Full text is the one genuinely divergent feature. Both are given the same
semantics — all of these words appear, case folded, the client's terms treated
as words rather than as query syntax — and PostgreSQL uses the `simple` text
search configuration rather than `english` precisely so a stemmer cannot make
the same query match different documents on the two engines.

### The parity gate

`internal/storage/storagetest` is a suite written against the `Backend`
interface, knowing nothing about SQL, and **both engines run all of it**. So
does the entire REST suite — CRUD, versioning, history, conditional operations,
every search parameter type, chaining, `_has`, includes, composites, `_filter`,
transactions, validation — which is the wider gate, because a divergence that
only shows through a search parameter or a transaction is one a storage-level
suite can miss.

```sh
make check          # SQLite, and says so
GOGOFHIR_TEST_POSTGRES='postgres://user@host:5432/db?sslmode=disable' \
  make check-parity # both engines, the identical assertions
```

Without the variable the PostgreSQL half skips *loudly* — a silently skipped
parity gate is not a gate — and CI supplies it from a service container.

Finding this way rather than by inspection is the point. The suite immediately
caught a string search that only worked because SQLite's `LIKE` ignores ASCII
case, which would have been a silent behaviour difference between deployments.

## Conformance data

The compiled indexes under `internal/conformance/data/` are committed, so an
ordinary build needs neither the published packages nor a network. Regenerating
them does:

```sh
make vendor   # fetch the packages pinned in third_party/packages.lock
make gen      # compile them into internal/conformance/data/
```

`make gen-check` fails if the committed indexes drift from what the pinned
packages produce.

### Where the packages come from

None come from the official FHIR registry (`packages.fhir.org`), which is
frequently unreachable from CI and sandboxed networks. Every mirror is pinned
exactly — a tarball digest, or a commit for R4 — and every package is CC0-1.0,
so vendoring is unencumbered.

| Release | Source | Pin |
|---|---|---|
| R4 4.0.1 | `google/fhir`, `spec/hl7.fhir.core/4.0.1/package` | commit `74fce953` |
| R5 5.0.0 | npm `hl7.fhir.r5.core@5.0.0` | sha256 `09f22107…` |
| Terminology | npm `hl7.terminology.r4` / `.r5` @7.0.1 | sha256 `170c546f…` / `b34d9c6b…` |

The terminology package is vendored alongside each release because R5 moved
most of its code systems out of core and into it. Without it, two thirds of
R5's required bindings would be reported as unchecked purely because the codes
live in a package nobody fetched — 64 unresolvable value sets rather than 16.
THO is versioned independently of the FHIR releases, so it is pinned on its own
rather than derived from the core package, which declares no dependency on it.

Cross-checked when the pins were chosen: the R4 JSON package above and the
independent npm package `hl7.fhir.r4.corexml@4.0.1` — the XML serialization of
the same release — agree exactly on resource counts (658 StructureDefinition,
1400 SearchParameter, 1316 ValueSet, 1062 CodeSystem, 47 OperationDefinition,
6 CompartmentDefinition).

### What survives compilation

R5 core ships 2969 JSON files; the compiled index keeps what a server actually
consults and discards examples, narrative, and terminology.

| | R4 | R5 |
|---|---|---|
| Resource types | 146 | 158 |
| Datatypes | 61 | 69 |
| Search parameter bindings | 1716 | 1988 |
| Profiles | 441 | 64 |
| Value set expansions | 334 of 346 | 357 of 373 |
| Invariants | 241 | 383 |
| Compartments | 5 | 5 |
| Index size | 2.3 MB | 3.9 MB |

Invariants are stored once on the type that declares them, not copied onto every
element that inherits them — `ele-1` alone appears tens of thousands of times
across a release's snapshots. `Index.Invariants` reassembles them by walking the
base chain. Search parameters work the same way: `_id` lives on `Resource`, not
on all 158 concrete types.

## FHIRPath

The engine is the keystone: because resources are untyped documents, every
question the server asks about their contents is asked in FHIRPath — search
parameter extraction, invariants, `_filter`, subscription criteria. It is built
as a standalone package with no dependency on the storage or conformance layers,
so it can be developed and proven on its own.

### Conformance

HL7 publishes a test suite for FHIRPath. It is vendored (see
`third_party/packages.lock`) and runs offline as part of `make check`.

| Suite | Passing | Known divergences | Skipped |
|---|---|---|---|
| R4 | 924 / 935 | 11 | 0 |
| R5 | 1042 / 1063 | 11 | 10 |

Every non-passing case is enumerated with its reason in `knownDivergences`
(`internal/fhirpath/suite_test.go`). The list is kept honest in both directions:
an unlisted failure fails the build, and a listed case that *starts* passing also
fails it, so a fixed divergence cannot linger. The divergences fall into four
groups:

- **Static type checking (10).** Cases expecting an error for navigating an
  element a type does not define. At runtime that is a no-op yielding empty;
  diagnosing it needs the expression checked against a type before evaluation.
  Worth building — it would catch typos in stored search parameters and
  invariants — but it is a separate component from the evaluator.
- **Decimal boundary rounding (6).** The reference implementation is not
  self-consistent when rounding a boundary to a precision coarser than the
  value's own: it wants `1.587.highBoundary(2)` = 1.59, rounding away from the
  value, but `0.0034.highBoundary(1)` = 0.0, rounding toward it. No single rule
  yields both. We implement the rule that actually bounds the interval.
- **UCUM dimensional algebra (2).** `2.0 'cm' * 2.0 'm' = 0.040 'm2'` requires
  deriving new dimensions from unit products. The unit table here converts
  within a dimension but does not derive across them.
- **Suite disagreement (1).** R4 expects `+ 0.1 's'` to truncate to zero; R5
  expects it to add 100 milliseconds. We follow R5, which is the maintained
  suite — the repository marks `r4` as no longer maintained.

Ten R5 cases are skipped rather than diverged: they exercise CDA documents,
a terminology server, and the `htmlChecks()` hook, none of which are FHIRPath.

Set `GOGOFHIR_SUITE_ALL=1` to print every failure instead of the first forty.

### Beyond the suite

The compiled indexes carry 4636 FHIRPath expressions published by the
specification — every search parameter, composite component, and invariant across
both releases. All 4636 parse, and all render back to source that reparses
identically; a mis-associated operator generally shows up as a round-trip
mismatch.

## Documents

Resources are decoded JSON — maps, slices, and scalars — with all meaning
supplied by the conformance index. Two readers produce the same in-memory shape:

- **JSON**, including the parallel `_name` objects that carry a primitive's
  extensions, and numbers preserved in their source spelling. FHIR decimals are
  exact: 1.10 asserts a precision 1.1 does not, and `float64` would destroy the
  distinction before anything else could go wrong.
- **XML**, where primitives carry a `value` attribute, repetition is repeated
  elements, and a primitive's extensions are children. Converting it to the same
  shape means navigation, evaluation, and validation are written once — and
  yields an XML-to-JSON converter for M2's content negotiation as a side effect.

A cross-format test asserts the two navigate identically; that equivalence is
what the design rests on.

## Layout

```
cmd/gogofhir/            daemon
internal/conformance/    embedded compiled index + loader
internal/conformance/model/  index types, importable by the generator
internal/fhirpath/       lexer, parser, evaluator, function library
internal/resource/       untyped documents; JSON and XML readers and writers
internal/storage/        backend interface, query plan, index extraction
internal/storage/sqlite/ SQLite backend and schema
internal/rest/           routing, interactions, bundles, CapabilityStatement
tools/confgen/           build-time conformance compiler
tools/vendorpkg/         pinned package and test-suite fetcher
third_party/             package pins (packages.lock); fetched packages are ignored
```

`model` is a separate package deliberately: the generator needs the index types,
while the loader embeds the generated files. Were they one package, deleting the
generated data would stop the generator from compiling — leaving no way to
regenerate it.

## Milestones

- [x] **M0 — Foundation.** Module, build, CI, vendoring, conformance compiler.
- [x] **M1 — FHIRPath.** Full engine: lexer, parser, evaluator, function
      library, and both document readers. 1966 of 1998 official conformance
      tests pass; the rest are enumerated above.
- [x] **M2 — Storage + REST core.** CRUD, versioning, history, conditional
      operations, ETag concurrency, CapabilityStatement, JSON and XML.
- [x] **M3 — Search fundamentals.** Every indexed parameter type with its
      modifiers and prefixes, full-text, `_summary`, `_elements`, `_total`,
      multi-key sorting, and cursor-stable paging.
- [x] **M4 — Search advanced.** Chaining, `_has`, `_include`/`_revinclude`
      with `:iterate`, composite parameters, and `_filter`.
- [x] **M5 — Transactions & batch.** Internal reference resolution, conditional
      references, processing order, and atomicity. **v1 complete.**
- [x] **M6 — Validation.** `$validate`, structural checks, the specification's
      invariants, bindings against build-time value set expansions, profiles
      with slicing, and the terminology policy.
- [x] **M7a — PostgreSQL.** One SQL implementation for both engines, and the
      storage and REST suites passing identically on each.
- [ ] **M7b — US Core.** Profile conformance against the published package.
- [ ] **M7c — SMART App Launch.** OAuth2, to make Inferno meaningful.

FHIRPath comes first because everything depends on it: search extraction,
`_filter`, invariants, and subscription criteria are all FHIRPath. It is also
the one component with an official conformance suite, so it can be proven
correct before any endpoint exists.

## Development

```sh
make check    # the pre-push gate: gofmt check + go vet + race tests
make test     # plain test run
make cover    # race tests with a coverage summary
```

`make help` lists everything.
