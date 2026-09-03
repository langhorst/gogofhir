# gogofhir

A FHIR server in Go — a dev/test server and conformance target. One static
binary, an embedded conformance package, and a SQLite file: run it with no
dependencies, point a FHIR client at it, and run it in CI to prove that client
behaves.

The name is a Dhalsim joke. Go for Go, go for let's go, fhir for FHIR.

> **Status: M1 (FHIRPath), in progress.** The conformance pipeline is complete,
> and the FHIRPath lexer and parser are done — proven against every expression
> the specification publishes. The evaluator is next. There is no HTTP server
> yet; the REST layer is M2. See [Milestones](#milestones).

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
make build                  # no network, no vendored packages needed
./bin/gogofhir conformance -fhir r5
```

```
release        r5 (5.0.0)
package        hl7.fhir.r5.core
resources      158
datatypes      69
search params  1988 bindings
invariants     383
compartments   Device, Encounter, Patient, Practitioner, RelatedPerson
```

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

## Layout

```
cmd/gogofhir/            daemon
internal/conformance/    embedded compiled index + loader
internal/conformance/model/  index types, importable by the generator
tools/confgen/           build-time conformance compiler
tools/vendorpkg/         pinned package fetcher
third_party/             package pins (packages.lock); fetched packages are ignored
```

`model` is a separate package deliberately: the generator needs the index types,
while the loader embeds the generated files. Were they one package, deleting the
generated data would stop the generator from compiling — leaving no way to
regenerate it.

## Milestones

- [x] **M0 — Foundation.** Module, build, CI, vendoring, conformance compiler.
- [ ] **M1 — FHIRPath.** Full engine. Done when the official R4 and R5 test
      suites pass. *Lexer and parser complete: all 4636 FHIRPath expressions
      published across both releases parse and round-trip. Evaluator next.*
- [ ] **M2 — Storage + REST core.** CRUD, versioning, history, conditional
      operations, ETag concurrency, CapabilityStatement.
- [ ] **M3 — Search fundamentals.** All nine parameter types, modifiers,
      prefixes, sorting, cursor paging.
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
