# gogofhir

A FHIR server in Go — a dev/test server and conformance target. One static
binary, an embedded conformance package, and a SQLite file: run it with no
dependencies, point a FHIR client at it, and run it in CI to prove that client
behaves.

The name is a Dhalsim joke. Go for Go, go for let's go, fhir for FHIR.

> **Status: M3 complete.** `gogofhir serve` gives you CRUD, versioning,
> history, conditional operations, optimistic concurrency, and full search —
> every indexed parameter type with its modifiers, full-text, `_summary`,
> `_elements`, `_total`, and cursor-stable paging — over 158 resource types, in
> JSON and XML. The FHIRPath engine passes **1966 of 1998** official HL7
> conformance tests. See [Milestones](#milestones).

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
| Storage | SQLite through v1; PostgreSQL at M7 |
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
```

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

Chaining, `_has`, `_include`/`_revinclude`, and composite parameters arrive
with M4. Parameters that are declared but not yet indexed are deliberately
absent from the CapabilityStatement rather than advertised and broken.

## Storage

One `Backend` interface, and no SQL outside its implementations. The query
surface is a typed plan rather than a string, so the search layer never writes
SQL and the backend never parses FHIR syntax — which is what should make the
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

Indexes are written in the same transaction as the resource, so an index can
never describe a version that is not stored, and they follow the current
version: an updated resource stops matching its old values, a deleted one stops
matching at all.

Full-text (`_text`, `_content`) is the one place the two backends genuinely
diverge: SQLite uses FTS5, PostgreSQL will use `tsvector`. Everything else is
ordinary tables and B-tree lookups that both engines index identically, so
`0002_fulltext.sql` is the whole of the divergence rather than the start of it.

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

Neither comes from the official FHIR registry (`packages.fhir.org`), which is
frequently unreachable from CI and sandboxed networks. Both mirrors are pinned
exactly — a tarball digest for R5, a commit for R4 — and both packages are
CC0-1.0, so vendoring is unencumbered.

| Release | Source | Pin |
|---|---|---|
| R4 4.0.1 | `google/fhir`, `spec/hl7.fhir.core/4.0.1/package` | commit `74fce953` |
| R5 5.0.0 | npm `hl7.fhir.r5.core@5.0.0` | sha256 `09f22107…` |

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
- [ ] **M4 — Search advanced.** Chaining, `_has`, `_include`/`_revinclude`,
      composites, `_filter`.
- [ ] **M5 — Transactions & batch.** v1 complete.
- [ ] **M6 — Validation.** `$validate`, profiles, invariants, bindings.
- [ ] **M7 — PostgreSQL, US Core, SMART.**

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
